package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
)

//go:embed worker.mjs adapter.mjs transport.mjs quota.mjs vendor/cursor/index.mjs vendor/cursor/native-session.mjs
var assets embed.FS

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type message struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
	Event  *struct {
		Payload []byte `json:"payload"`
	} `json:"event,omitempty"`
	Done     bool `json:"done,omitempty"`
	Callback *struct {
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Request json.RawMessage `json:"request"`
	} `json:"callback,omitempty"`
}

type hostCaller func(string, []byte) ([]byte, error)

type pendingCall struct {
	ch         chan message
	host       hostCaller
	callbackID string
}

type nodeWorker struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	cmd     *exec.Cmd
	in      io.WriteCloser
	dir     string
	pending map[uint64]pendingCall
	next    atomic.Uint64
	closed  bool
}

func (w *nodeWorker) startLocked() error {
	if w.closed {
		return errors.New("Cursor plugin is shut down")
	}
	if w.cmd != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "cliproxy-cursor-*")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	err = fs.WalkDir(assets, ".", func(path string, entry fs.DirEntry, errWalk error) error {
		if errWalk != nil {
			return errWalk
		}
		target := filepath.Join(dir, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		raw, errRead := assets.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		return os.WriteFile(target, raw, 0600)
	})
	if err != nil {
		cleanup()
		return err
	}
	node := os.Getenv("CURSOR_PLUGIN_NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.Command(node, filepath.Join(dir, "worker.mjs"))
	// The worker never logs credential-bearing requests to stderr.
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		cleanup()
		return err
	}
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		cleanup()
		return fmt.Errorf("start Cursor worker (Node.js 22+ required): %w", err)
	}
	w.cmd, w.in, w.dir = cmd, in, dir
	w.pending = make(map[uint64]pendingCall)
	go w.read(cmd, out, dir)
	return nil
}

func (w *nodeWorker) read(cmd *exec.Cmd, out io.Reader, dir string) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		var msg message
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			break
		}
		w.mu.Lock()
		pending, exists := w.pending[msg.ID]
		w.mu.Unlock()
		if msg.Callback != nil {
			// Host reads can block until upstream data arrives. They must never
			// block the pipe reader or cancellation and sibling RPCs would stall.
			go w.callback(cmd, msg, pending, exists)
		} else if exists {
			pending.ch <- msg
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	w.mu.Lock()
	if w.cmd == cmd {
		for id, pending := range w.pending {
			pending.ch <- message{Error: &rpcError{Code: "worker_exit", Message: "Cursor worker exited; request was not replayed", HTTPStatus: 502}, Done: true}
			delete(w.pending, id)
		}
		w.cmd = nil
		_ = w.in.Close()
		w.in = nil
	}
	w.mu.Unlock()
	_ = os.RemoveAll(dir)
}

func (w *nodeWorker) callback(cmd *exec.Cmd, msg message, pending pendingCall, exists bool) {
	callback := msg.Callback
	var raw []byte
	var err error
	allowed := map[string]bool{
		"host.http.operation_open": true, "host.http.do_stream": true,
		"host.http.stream_read": true, "host.http.stream_close": true, "host.http.cancel": true,
	}
	if !exists || pending.host == nil || pending.callbackID == "" || !allowed[callback.Method] {
		err = errors.New("CPA HTTP callback is unavailable outside its request scope")
	} else {
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(callback.Request, &fields); err == nil {
			if fields == nil {
				fields = make(map[string]json.RawMessage)
			}
			// Bind every callback to the scope allocated by CPA for this RPC.
			// Never trust a context ID supplied by the worker payload.
			fields["host_callback_id"], _ = json.Marshal(pending.callbackID)
			request, _ := json.Marshal(fields)
			raw, err = pending.host(callback.Method, request)
		}
	}
	if err != nil {
		raw = envelope(message{Error: &rpcError{Code: "host_callback_failed", Message: err.Error(), HTTPStatus: 502}})
	}
	if !json.Valid(raw) {
		raw = envelope(message{Error: &rpcError{Code: "host_callback_failed", Message: "Invalid CPA callback response", HTTPStatus: 502}})
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	in, current := w.in, w.cmd == cmd
	w.mu.Unlock()
	if !current || in == nil {
		return
	}
	reply, _ := json.Marshal(map[string]any{"callback_reply": callback.ID, "envelope": json.RawMessage(raw)})
	_, _ = in.Write(append(reply, '\n'))
}

func (w *nodeWorker) send(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	in := w.in
	w.mu.Unlock()
	if in == nil {
		return errors.New("Cursor worker is unavailable")
	}
	_, err = in.Write(append(raw, '\n'))
	return err
}

func (w *nodeWorker) begin(method string, request json.RawMessage, host hostCaller) (uint64, <-chan message, error) {
	w.mu.Lock()
	if err := w.startLocked(); err != nil {
		w.mu.Unlock()
		return 0, nil, err
	}
	id := w.next.Add(1)
	ch := make(chan message, 2)
	var req struct {
		CallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(request, &req)
	w.pending[id] = pendingCall{ch: ch, host: host, callbackID: req.CallbackID}
	w.mu.Unlock()
	if len(request) == 0 {
		request = json.RawMessage(`{}`)
	}
	if err := w.send(map[string]any{"id": id, "method": method, "request": request}); err != nil {
		w.forget(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (w *nodeWorker) forget(id uint64) {
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

func (w *nodeWorker) stop() {
	w.mu.Lock()
	w.closed = true
	cmd := w.cmd
	w.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
	}
}

func envelope(msg message) []byte {
	value := map[string]any{"ok": msg.Error == nil}
	if msg.Error != nil {
		value["error"] = msg.Error
	} else if len(msg.Result) != 0 {
		value["result"] = msg.Result
	} else {
		value["result"] = map[string]any{}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func dispatch(w *nodeWorker, method string, request json.RawMessage, host hostCaller) []byte {
	id, ch, err := w.begin(method, request, host)
	if err != nil {
		return envelope(message{Error: &rpcError{Code: "bridge_error", Message: err.Error(), HTTPStatus: 502}})
	}
	first := <-ch
	if method != "executor.execute_stream" || first.Error != nil {
		w.forget(id)
		return envelope(first)
	}
	var req struct {
		StreamID   string `json:"stream_id"`
		CallbackID string `json:"host_callback_id"`
	}
	if err = json.Unmarshal(request, &req); err != nil || req.StreamID == "" {
		_ = w.send(map[string]any{"id": id, "cancel": true})
		w.forget(id)
		return envelope(message{Error: &rpcError{Code: "invalid_stream", Message: "Host stream_id is required", HTTPStatus: 400}})
	}
	go func() {
		defer w.forget(id)
		call := func(method string, fields map[string]any) error {
			fields["stream_id"], fields["host_callback_id"] = req.StreamID, req.CallbackID
			raw, _ := json.Marshal(fields)
			response, errCall := host(method, raw)
			if errCall != nil {
				return errCall
			}
			var env struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal(response, &env) != nil || !env.OK {
				return errors.New("CPA rejected stream callback")
			}
			return nil
		}
		for msg := range ch {
			if msg.Done || msg.Error != nil {
				fields := map[string]any{}
				if msg.Error != nil {
					fields["error"] = msg.Error.Message
				}
				_ = call("host.stream.close", fields)
				return
			}
			if msg.Event != nil {
				if errEmit := call("host.stream.emit", map[string]any{"payload": msg.Event.Payload}); errEmit != nil {
					_ = w.send(map[string]any{"id": id, "cancel": true})
					_ = call("host.stream.close", map[string]any{})
					return
				}
				if errAck := w.send(map[string]any{"id": id, "ack": true}); errAck != nil {
					_ = call("host.stream.close", map[string]any{"error": "Cursor worker disconnected"})
					return
				}
			}
		}
	}()
	return envelope(first)
}
