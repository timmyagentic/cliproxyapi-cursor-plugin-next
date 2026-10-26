package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestWorker(t *testing.T) *nodeWorker {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required")
	}
	w := &nodeWorker{}
	t.Cleanup(w.stop)
	return w
}

func invoke(t *testing.T, w *nodeWorker, method string, req any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	result := dispatch(w, method, raw, func(string, []byte) ([]byte, error) { t.Error("unexpected host callback"); return nil, nil })
	var env map[string]json.RawMessage
	if err = json.Unmarshal(result, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestEmbeddedWorkerRegistration(t *testing.T) {
	w := newTestWorker(t)
	result := invoke(t, w, "plugin.register", map[string]any{})
	if string(result["ok"]) != "true" {
		t.Fatalf("registration failed: %s", result["error"])
	}
	var registration struct {
		SchemaVersion int            `json:"schema_version"`
		Capabilities  map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(result["result"], &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != 1 || registration.Capabilities["executor"] != true {
		t.Fatalf("invalid registration: %+v", registration)
	}
}

func TestConcurrentAuthCallsStayIsolated(t *testing.T) {
	w := newTestWorker(t)
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			key := fmt.Sprintf("key-%d", index)
			auth, _ := json.Marshal(map[string]any{"type": "cursor", "cursor_auth": map[string]any{"type": "api", "key": key}})
			result := invoke(t, w, "auth.parse", map[string]any{"RawJSON": base64.StdEncoding.EncodeToString(auth), "FileName": fmt.Sprintf("%d.json", index)})
			var parsed struct {
				Auth struct {
					FileName string
					Metadata struct {
						CursorAuth struct {
							Key string `json:"key"`
						} `json:"cursor_auth"`
					}
				}
			}
			if err := json.Unmarshal(result["result"], &parsed); err != nil {
				t.Error(err)
				return
			}
			if parsed.Auth.Metadata.CursorAuth.Key != key || parsed.Auth.FileName != fmt.Sprintf("%d.json", index) {
				t.Errorf("credential crossed calls: %+v", parsed)
			}
		}(i)
	}
	group.Wait()
}

func TestErrorsSurviveBridge(t *testing.T) {
	w := newTestWorker(t)
	result := invoke(t, w, "executor.count_tokens", map[string]any{})
	if string(result["ok"]) != "false" {
		t.Fatal("expected failure")
	}
	var errRPC rpcError
	if err := json.Unmarshal(result["error"], &errRPC); err != nil {
		t.Fatal(err)
	}
	if errRPC.HTTPStatus != 501 {
		t.Fatalf("status = %d", errRPC.HTTPStatus)
	}
}

func TestMissingNodeReturnsActionableFailure(t *testing.T) {
	t.Setenv("CURSOR_PLUGIN_NODE", "/not-a-node-binary")
	w := &nodeWorker{}
	t.Cleanup(w.stop)
	result := invoke(t, w, "plugin.register", map[string]any{})
	if string(result["ok"]) != "false" {
		t.Fatal("expected failure")
	}
}

func TestShutdownRejectsNewCalls(t *testing.T) {
	w := newTestWorker(t)
	invoke(t, w, "plugin.register", map[string]any{})
	w.mu.Lock()
	dir := w.dir
	w.mu.Unlock()
	w.stop()
	result := invoke(t, w, "plugin.register", map[string]any{})
	if string(result["ok"]) != "false" {
		t.Fatal("expected closed worker failure")
	}
	// Temp assets contain no credentials. Their permissions also keep other
	// local users from replacing the scripts before Node imports them.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0700 {
		t.Fatalf("unsafe directory mode: %v", info.Mode())
	}
}

func fakeWorker(t *testing.T) *nodeWorker {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture launcher uses /bin/sh")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required")
	}
	fixture, err := filepath.Abs("test/fake-worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "node-fixture")
	if err = os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$CURSOR_TEST_NODE\" \"$CURSOR_TEST_FIXTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_TEST_NODE", node)
	t.Setenv("CURSOR_TEST_FIXTURE", fixture)
	t.Setenv("CURSOR_PLUGIN_NODE", launcher)
	return newTestWorker(t)
}

func TestStreamingCallbacksAndBackpressure(t *testing.T) {
	w := fakeWorker(t)
	closed := make(chan struct{})
	var chunks []string
	raw := json.RawMessage(`{"stream_id":"stream-1","host_callback_id":"context-1"}`)
	result := dispatch(w, "executor.execute_stream", raw, func(method string, payload []byte) ([]byte, error) {
		var req struct {
			StreamID   string `json:"stream_id"`
			CallbackID string `json:"host_callback_id"`
			Payload    []byte `json:"payload"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Error(err)
		}
		if req.StreamID != "stream-1" || req.CallbackID != "context-1" {
			t.Error("lost host stream context")
		}
		if method == "host.stream.emit" {
			chunks = append(chunks, string(req.Payload))
			// An unrelated RPC still completes while a stream awaits its ack.
			invoke(t, w, "executor.identifier", map[string]any{})
		} else if method == "host.stream.close" {
			close(closed)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	if !strings.Contains(string(result), `"ok":true`) {
		t.Fatalf("stream failed: %s", result)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never closed")
	}
	if len(chunks) != 2 || chunks[1] != "data: [DONE]\n\n" {
		t.Fatalf("unexpected chunks: %q", chunks)
	}
}

func TestHostCancellationReleasesStream(t *testing.T) {
	w := fakeWorker(t)
	closed := make(chan struct{})
	raw := json.RawMessage(`{"stream_id":"cancel-me","host_callback_id":"ctx"}`)
	dispatch(w, "executor.execute_stream", raw, func(method string, payload []byte) ([]byte, error) {
		if method == "host.stream.emit" {
			return nil, fmt.Errorf("downstream canceled")
		}
		close(closed)
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled stream never closed")
	}
	invoke(t, w, "executor.identifier", map[string]any{})
}

func TestWorkerCannotForgeCallbackContext(t *testing.T) {
	w := fakeWorker(t)
	called := false
	raw := dispatch(w, "fixture.callback", json.RawMessage(`{"host_callback_id":"trusted-context"}`), func(method string, payload []byte) ([]byte, error) {
		called = true
		var req struct {
			ID string `json:"host_callback_id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		if req.ID != "trusted-context" {
			t.Errorf("worker substituted callback context: %q", req.ID)
		}
		return hostResult(map[string]any{"operation_id": "operation"}), nil
	})
	if !called || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("callback failed: %s", raw)
	}
}

func TestWorkerCallbacksAreLimitedToHTTP(t *testing.T) {
	w := fakeWorker(t)
	raw := dispatch(w, "fixture.callback", json.RawMessage(`{"host_callback_id":"trusted-context","method":"host.auth.save"}`), func(string, []byte) ([]byte, error) {
		t.Error("unexpected non-HTTP host callback")
		return nil, nil
	})
	if !strings.Contains(string(raw), `"host_callback_failed"`) {
		t.Fatalf("non-HTTP callback was accepted: %s", raw)
	}
}
