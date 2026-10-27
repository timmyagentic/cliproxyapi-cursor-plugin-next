package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nativeFakeFrame struct {
	data []byte
	end  bool
	err  error
}
type nativeFakeTransport struct {
	frames    chan nativeFakeFrame
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	sent      [][]byte
	failSend  bool
	failAt    int
	sendError error
	closed    atomic.Int32
}

func newNativeFakeTransport() *nativeFakeTransport {
	return &nativeFakeTransport{frames: make(chan nativeFakeFrame, 64), done: make(chan struct{})}
}
func (f *nativeFakeTransport) Send(ctx context.Context, b []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return io.ErrClosedPipe
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), b...))
	if f.failSend && (f.failAt == 0 || len(f.sent) == f.failAt) {
		if f.sendError != nil {
			return f.sendError
		}
		return errors.New("uncertain synthetic write")
	}
	return nil
}
func (f *nativeFakeTransport) Next(ctx context.Context) ([]byte, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-f.done:
		return nil, false, io.EOF
	case v := <-f.frames:
		return v.data, v.end, v.err
	}
}
func (f *nativeFakeTransport) Close() error {
	f.once.Do(func() { f.closed.Add(1); close(f.done) })
	return nil
}
func (f *nativeFakeTransport) Protocol() string { return "fake-h2" }
func (f *nativeFakeTransport) push(b []byte)    { f.frames <- nativeFakeFrame{data: b} }
func (f *nativeFakeTransport) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func nativeTestPool(t *testing.T) (*nativeSessions, *nativeFakeTransport, *atomic.Int32) {
	t.Helper()
	tr := newNativeFakeTransport()
	opens := &atomic.Int32{}
	ns := newNativeSessions(context.Background(), func(ctx context.Context, account, token, requestID string, first []byte) (cursorRunTransport, error) {
		opens.Add(1)
		if len(first) == 0 || account == "" || token == "" || requestID == "" {
			t.Error("incomplete native open identity")
		}
		return tr, nil
	})
	t.Cleanup(func() { _ = ns.Close() })
	return ns, tr, opens
}
func nativeTestInput() nativeInput {
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": "path help"}, "description": map[string]any{"type": "string"}}, "required": []any{"path"}}
	chat := chatRequest{Model: "grok-4.7@500k", Messages: []map[string]any{{"role": "system", "content": "synthetic rules"}, {"role": "user", "content": "read synthetic files"}}, Tools: []map[string]any{{"type": "function", "function": map[string]any{"name": "read", "description": "Read one file", "parameters": schema}}, {"type": "function", "function": map[string]any{"name": "write", "description": "Write one file", "parameters": schema}}}}
	tools := toolsOf(chat)
	conv, _ := conversation(chat, tools)
	return nativeInput{Chat: chat, Model: modelResolution{ID: "grok-4.7-high-fast", Max: true, Params: []parameter{{"context", "500k"}, {"fast", "true"}}}, Tools: tools, Conversation: conv, CID: "physical-cid", Account: "synthetic-account", Token: "synthetic-credential"}
}
func nativeTestUpdate(kind int, b []byte) []byte {
	return protobuf(nil).bytes(1, protobuf(nil).bytes(kind, b))
}
func nativeTestCount(n int) []byte { return nativeTestUpdate(27, protobuf(nil).uint(1, uint64(n))) }
func nativeTestCall(id uint64, name string) []byte {
	arg := protobuf(nil).text(1, "magpie-"+name).text(3, "upstream-"+string(rune(id+64))).text(4, "magpie").text(5, name).text(9, "magpie").bytes(2, protobuf(nil).text(1, "path").bytes(2, pbValue("synthetic.txt")))
	return protobuf(nil).bytes(2, protobuf(nil).uint(1, id).text(15, "exec-synthetic").bytes(11, arg))
}
func nativeTestFinal(text string) []byte {
	return protobuf(nil).bytes(1, protobuf(nil).bytes(1, protobuf(nil).text(1, text)).bytes(14, protobuf(nil).uint(1, 100).uint(2, 4).uint(3, 80)))
}
func nativeReadEvents(t *testing.T, ch <-chan runEvent) []runEvent {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	out := []runEvent{}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline.C:
			t.Fatal("native response did not complete")
			return nil
		}
	}
}
func nativeWait(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("native condition did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
func nativeContinuation(t *testing.T, in nativeInput, events []runEvent) nativeInput {
	t.Helper()
	var chat chatRequest
	if err := json.Unmarshal(encodeJSON(in.Chat), &chat); err != nil {
		t.Fatal(err)
	}
	text, reason := "", ""
	calls := []any{}
	for _, ev := range events {
		text += ev.text
		reason += ev.reasoning
		if ev.tool != nil {
			c := copyMap(ev.tool)
			delete(c, "index")
			calls = append(calls, c)
		}
	}
	assistant := map[string]any{"role": "assistant", "content": text, "tool_calls": calls}
	if reason != "" {
		assistant["reasoning_content"] = reason
	}
	chat.Messages = append(chat.Messages, assistant)
	for _, raw := range calls {
		c := object(raw)
		chat.Messages = append(chat.Messages, map[string]any{"role": "tool", "tool_call_id": c["id"], "content": "synthetic result for " + str(c["id"])})
	}
	in.Chat = chat
	in.Tools = toolsOf(chat)
	in.Conversation, _ = conversation(chat, in.Tools)
	return in
}
func nativePark(t *testing.T, ns *nativeSessions, tr *nativeFakeTransport, in nativeInput, n int) (nativeInput, nativeReceipt, []runEvent) {
	t.Helper()
	events, r, err := ns.Start(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "synthetic reasoning")))
	for i := 0; i < n; i++ {
		tr.push(nativeTestCall(uint64(i+1), "read"))
	}
	tr.push(nativeTestCount(n))
	out := nativeReadEvents(t, events)
	if len(out) == 0 || out[len(out)-1].stop != "tool_calls" {
		t.Fatalf("handoff: %+v", out)
	}
	r.Delivered()
	return nativeContinuation(t, in, out), r, out
}

func TestNativeSameRunParallelResultsAndMeasuredEndHook(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	var starts, ends atomic.Int32
	endEvents := make(chan runEvent, 1)
	ns.OnStart = func(r nativeReceipt) error {
		starts.Add(1)
		if r.RunID == "" || r.token == "" {
			t.Error("missing receipt identity")
		}
		return nil
	}
	ns.OnEnd = func(r nativeReceipt, ev runEvent, err error) {
		if err != nil {
			t.Errorf("unexpected end: %v", err)
		}
		if tr.closed.Load() != 1 {
			t.Error("OnEnd ran before transport closure")
		}
		ends.Add(1)
		endEvents <- ev
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := nativeTestInput()
	events, r, err := ns.Start(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Context requests are answered but not counted. Tool-list requests count
	// toward the declared batch, even though the client never executes them.
	tr.push(protobuf(nil).bytes(2, protobuf(nil).uint(1, 90).bytes(10, nil)))
	tr.push(protobuf(nil).bytes(2, protobuf(nil).uint(1, 91).bytes(36, nil)))
	tr.push(nativeTestCall(7, "read"))
	tr.push(nativeTestCall(8, "write"))
	tr.push(nativeTestCount(3))
	out := nativeReadEvents(t, events)
	tools := 0
	for _, ev := range out {
		if ev.tool != nil {
			tools++
			if !nativeHandlePattern.MatchString(str(ev.tool["id"])) {
				t.Fatalf("unsafe handle: %v", ev.tool)
			}
		}
		if ev.usage != nil {
			t.Fatal("pending step fabricated usage")
		}
	}
	if tools != 2 {
		t.Fatalf("got %d external tools", tools)
	}
	r.Delivered()
	cancel()
	time.Sleep(10 * time.Millisecond)
	if tr.closed.Load() != 0 || !ns.Busy() {
		t.Fatal("delivered handoff remained attached to original caller cancellation")
	}
	next := nativeContinuation(t, in, out)
	next.CID = "new-per-request-random-cid" // no stable cache key is required
	continued, rr, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Resumed || rr.RunID != r.RunID || rr.CID != r.CID || rr.RequestID != r.RequestID {
		t.Fatalf("lost physical identity: %+v", rr)
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) >= 8 })
	resultIDs := map[uint64]bool{}
	for _, b := range tr.snapshot() {
		fs, _ := pbFields(b)
		if f := pbGet(fs, 2); f != nil {
			inner, _ := pbFields(f.data)
			if pbGet(inner, 11) != nil {
				resultIDs[pbNum(inner, 1)] = true
				if pbText(inner, 15) != "exec-synthetic" {
					t.Fatal("wrong exec correlation")
				}
			}
		}
	}
	if !resultIDs[7] || !resultIDs[8] || len(resultIDs) != 2 {
		t.Fatalf("wrong submitted execs: %v", resultIDs)
	}
	tr.push(nativeTestFinal("synthetic final"))
	done := nativeReadEvents(t, continued)
	rr.Delivered()
	if done[len(done)-1].stop != "stop" || done[len(done)-1].usage != nil {
		t.Fatal("Run total leaked as current-step context usage")
	}
	if starts.Load() != 1 || ends.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("physical runs: start=%d end=%d open=%d", starts.Load(), ends.Load(), opens.Load())
	}
	end := <-endEvents
	if end.usage == nil || end.internalCalls != 1 || end.internalSteps != 2 {
		t.Fatalf("authoritative Run accounting lost: %+v", end)
	}
	ns.mu.Lock()
	owner := ns.entries[r.RunID]
	ns.mu.Unlock()
	owner.mu.Lock()
	secret := owner.receipt.token
	owner.mu.Unlock()
	if secret != "" {
		t.Fatal("terminal replay owner retained credential")
	}
}

func TestNativeContinuationValidationBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name   string
		change func(*nativeInput)
	}{
		{"account", func(in *nativeInput) { in.Account = "another-account" }},
		{"model", func(in *nativeInput) { in.Model.ID = "other-model" }},
		{"parameters", func(in *nativeInput) { in.Model.Params = []parameter{{"context", "256k"}} }},
		{"owner scope", func(in *nativeInput) { in.OwnerScope = "different-cache-key" }},
		{"system", func(in *nativeInput) { in.Chat.Messages[0]["content"] = "changed instructions" }},
		{"call name", func(in *nativeInput) {
			object(object(array(in.Chat.Messages[2]["tool_calls"])[0])["function"])["name"] = "write"
		}},
		{"call args", func(in *nativeInput) {
			object(object(array(in.Chat.Messages[2]["tool_calls"])[0])["function"])["arguments"] = `{"path":"other"}`
		}},
		{"call id", func(in *nativeInput) { object(array(in.Chat.Messages[2]["tool_calls"])[0])["id"] = "changed" }},
		{"partial batch", func(in *nativeInput) { in.Chat.Messages = in.Chat.Messages[:len(in.Chat.Messages)-1] }},
		{"duplicate result", func(in *nativeInput) { in.Chat.Messages[4]["tool_call_id"] = in.Chat.Messages[3]["tool_call_id"] }},
		{"unknown result", func(in *nativeInput) { in.Chat.Messages[3]["tool_call_id"] = "unknown" }},
		{"new input interleaved", func(in *nativeInput) { in.Chat.Messages[3]["role"] = "user" }},
		{"schema property", func(in *nativeInput) {
			object(in.Tools[0].Schema["properties"])["new-property"] = map[string]any{"type": "string"}
		}},
		{"property named description", func(in *nativeInput) { delete(object(in.Tools[0].Schema["properties"]), "description") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, tr, opens := nativeTestPool(t)
			next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 2)
			before := len(tr.snapshot())
			tc.change(&next)
			_, _, err := ns.Start(context.Background(), next)
			if err == nil || errorStatus(err) != 409 {
				t.Fatalf("want typed 409, got %v", err)
			}
			if opens.Load() != 1 || len(tr.snapshot()) != before {
				t.Fatal("invalid continuation wrote or opened a replacement Run")
			}
		})
	}
}

func TestNativeDescriptionOnlyToolChangesRemainCompatible(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	for _, raw := range next.Chat.Tools {
		f := object(raw["function"])
		f["description"] = "new prose"
		object(object(f["parameters"])["properties"])["path"].(map[string]any)["description"] = "new nested prose"
	}
	next.Tools = toolsOf(next.Chat)
	next.Conversation, _ = conversation(next.Chat, next.Tools)
	events, r, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestFinal("done"))
	_ = nativeReadEvents(t, events)
	r.Delivered()
}

func TestNativeSingleReservationAndBoundedReplay(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	ns.limits.ReplayTTL = 35 * time.Millisecond
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	type result struct {
		events <-chan runEvent
		r      nativeReceipt
		err    error
	}
	results := make(chan result, 2)
	barrier := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { <-barrier; ev, r, e := ns.Start(context.Background(), next); results <- result{ev, r, e} }()
	}
	close(barrier)
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	if first.err != nil || second.err == nil || errorStatus(second.err) != 409 {
		t.Fatalf("reservation results: %v %v", first.err, second.err)
	}
	tr.push(nativeTestFinal("final-once"))
	original := nativeReadEvents(t, first.events)
	first.r.Delivered()
	before := len(tr.snapshot())
	replay, r, err := ns.Start(context.Background(), next)
	if err != nil || !r.Replay {
		t.Fatalf("exact retry did not replay: %v", err)
	}
	got := nativeReadEvents(t, replay)
	r.Delivered()
	if got[0].text != original[0].text || got[len(got)-1].accounting["status"] != "replayed" || len(tr.snapshot()) != before || opens.Load() != 1 {
		t.Fatal("retry resubmitted results or lost its replay marker")
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, err = ns.Start(context.Background(), next); err == nil || errorStatus(err) != 409 {
		t.Fatalf("expired replay was accepted: %v", err)
	}
}

func TestNativeUncertainResultWriteCannotBeResubmitted(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	tr.mu.Lock()
	tr.failSend = true
	tr.mu.Unlock()
	events, _, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	out := nativeReadEvents(t, events)
	if out[len(out)-1].err == nil || !strings.Contains(out[len(out)-1].err.Error(), "uncertain") {
		t.Fatalf("lost uncertain outcome: %+v", out)
	}
	before := len(tr.snapshot())
	if _, _, err = ns.Start(context.Background(), next); err == nil || errorStatus(err) != 409 {
		t.Fatalf("uncertain exec accepted again: %v", err)
	}
	if opens.Load() != 1 || len(tr.snapshot()) != before {
		t.Fatal("uncertain exec was written twice")
	}
}

func TestNativeCancellationTTLAndShutdownCloseTransport(t *testing.T) {
	for _, mode := range []string{"inference cancellation", "undelivered handoff cancellation", "pending ttl", "lifetime", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			ns, tr, _ := nativeTestPool(t)
			if mode == "pending ttl" {
				ns.limits.PendingTTL = 25 * time.Millisecond
			}
			if mode == "lifetime" {
				ns.limits.Lifetime = 25 * time.Millisecond
			}
			var ends atomic.Int32
			ns.OnEnd = func(nativeReceipt, runEvent, error) { ends.Add(1) }
			ctx, cancel := context.WithCancel(context.Background())
			events, r, err := ns.Start(ctx, nativeTestInput())
			if err != nil {
				t.Fatal(err)
			}
			if mode != "inference cancellation" {
				tr.push(nativeTestCall(1, "read"))
				tr.push(nativeTestCount(1))
				_ = nativeReadEvents(t, events)
				if mode != "undelivered handoff cancellation" {
					r.Delivered()
				}
			}
			switch mode {
			case "inference cancellation", "undelivered handoff cancellation":
				cancel()
			case "shutdown":
				_ = ns.Close()
			}
			nativeWait(t, func() bool { return tr.closed.Load() == 1 && ends.Load() == 1 && !ns.Busy() })
			cancel()
			if ends.Load() != 1 {
				t.Fatal("duplicate OnEnd")
			}
		})
	}
}

func TestNativeStrictProtocolNeverSealsIncompleteBatch(t *testing.T) {
	tests := []struct {
		name   string
		frames []nativeFakeFrame
	}{
		{"malformed protobuf", []nativeFakeFrame{{data: []byte{0x0a, 0xff}}}},
		{"malformed inner wire", []nativeFakeFrame{{data: protobuf(nil).bytes(1, protobuf(nil).uint(27, 1))}}},
		{"EOF after call", []nativeFakeFrame{{data: nativeTestCall(1, "read")}, {err: io.EOF}}},
		{"incomplete declared batch", []nativeFakeFrame{{data: nativeTestCount(2)}, {data: nativeTestCall(1, "read")}, {end: true, data: []byte("{}")}}},
		{"conflicting count", []nativeFakeFrame{{data: nativeTestCount(2)}, {data: nativeTestCount(3)}}},
		{"unsupported interaction query", []nativeFakeFrame{{data: protobuf(nil).bytes(7, nil)}}},
		{"terminal with pending calls", []nativeFakeFrame{{data: nativeTestCall(1, "read")}, {data: nativeTestFinal("invalid final")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, tr, _ := nativeTestPool(t)
			events, _, err := ns.Start(context.Background(), nativeTestInput())
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range tc.frames {
				tr.frames <- f
			}
			out := nativeReadEvents(t, events)
			if len(out) == 0 || out[len(out)-1].err == nil {
				t.Fatalf("bad protocol accepted: %+v", out)
			}
			for _, ev := range out {
				if ev.tool != nil || ev.stop == "tool_calls" {
					t.Fatal("incomplete tool batch escaped")
				}
			}
			if tr.closed.Load() != 1 {
				t.Fatal("failed protocol left its transport open")
			}
		})
	}
}

func TestNativeWatchdogIdleHeartbeatQuietAndParkedPause(t *testing.T) {
	for _, mode := range []string{"idle", "heartbeat-only", "parked"} {
		t.Run(mode, func(t *testing.T) {
			ns, tr, _ := nativeTestPool(t)
			ns.limits.Idle = 35 * time.Millisecond
			ns.limits.Quiet = 75 * time.Millisecond
			ns.limits.PendingTTL = time.Second
			events, r, err := ns.Start(context.Background(), nativeTestInput())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "parked" {
				tr.push(nativeTestCall(1, "read"))
				tr.push(nativeTestCount(1))
				out := nativeReadEvents(t, events)
				r.Delivered()
				time.Sleep(120 * time.Millisecond)
				if tr.closed.Load() != 0 {
					t.Fatal("quiet clock ran while waiting for caller tools")
				}
				next := nativeContinuation(t, nativeTestInput(), out)
				continued, rr, e := ns.Start(context.Background(), next)
				if e != nil {
					t.Fatal(e)
				}
				tr.push(nativeTestFinal("done"))
				_ = nativeReadEvents(t, continued)
				rr.Delivered()
				return
			}
			stop := make(chan struct{})
			if mode == "heartbeat-only" {
				go func() {
					ticker := time.NewTicker(8 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-tr.done:
							return
						case <-ticker.C:
							tr.push(nativeTestUpdate(13, nil))
						}
					}
				}()
			}
			out := nativeReadEvents(t, events)
			close(stop)
			if out[len(out)-1].err == nil || !strings.Contains(out[len(out)-1].err.Error(), "quiet") {
				t.Fatalf("watchdog did not fail closed: %+v", out)
			}
		})
	}
}

func TestNativeAdmissionFailureAndCapacity(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		ns, _, opens := nativeTestPool(t)
		var ends atomic.Int32
		ns.OnStart = func(nativeReceipt) error { return failure(409, "synthetic admission rejected") }
		ns.OnEnd = func(nativeReceipt, runEvent, error) { ends.Add(1) }
		_, _, err := ns.Start(context.Background(), nativeTestInput())
		if err == nil || opens.Load() != 0 || ends.Load() != 0 || ns.Busy() {
			t.Fatal("failed admission opened or ended an unadmitted Run")
		}
	})
	t.Run("capacity", func(t *testing.T) {
		ns, _, opens := nativeTestPool(t)
		ns.limits.PerAccount = 1
		_, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = ns.Start(context.Background(), nativeTestInput())
		if err == nil || errorStatus(err) != 503 || opens.Load() != 1 {
			t.Fatalf("account cap did not reject: %v", err)
		}
	})
	t.Run("result bytes", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		ns.limits.RetainedBytes = 2048
		next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
		next.Chat.Messages[len(next.Chat.Messages)-1]["content"] = strings.Repeat("x", 3000)
		before := len(tr.snapshot())
		_, _, err := ns.Start(context.Background(), next)
		if err == nil || len(tr.snapshot()) != before {
			t.Fatal("oversized results reached transport")
		}
	})
}

func TestNativeImportsLegacyHandlesButRejectsExpiredNewHandles(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	in := nativeTestInput()
	in.Chat.Messages = append(in.Chat.Messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_cursor_0123456789abcdef0123", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": "call_cursor_0123456789abcdef0123", "content": "old result"})
	in.Conversation, _ = conversation(in.Chat, in.Tools)
	events, _, err := ns.Start(context.Background(), in)
	if err != nil {
		t.Fatalf("legacy stateless history rejected: %v", err)
	}
	tr.push(nativeTestFinal("imported"))
	_ = nativeReadEvents(t, events)
	in.Chat.Messages[len(in.Chat.Messages)-1]["tool_call_id"] = "call_cursor_0123456789abcdef0123456789abcdef_0_0"
	if _, _, err = ns.Start(context.Background(), in); err == nil || errorStatus(err) != 409 || opens.Load() != 1 {
		t.Fatal("expired correlated handle opened a replacement")
	}
}

func TestNativeResultWriteErrorsPreserveTransportStatusWithoutResubmission(t *testing.T) {
	for _, status := range []int{401, 429} {
		for _, phase := range []int{1, 2} {
			t.Run(fmt.Sprintf("status%d-write%d", status, phase), func(t *testing.T) {
				ns, tr, opens := nativeTestPool(t)
				next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
				tr.mu.Lock()
				tr.failSend = true
				tr.failAt = phase
				tr.sendError = failure(status, "synthetic upstream denial")
				tr.mu.Unlock()
				events, _, err := ns.Start(context.Background(), next)
				if err != nil {
					t.Fatal(err)
				}
				out := nativeReadEvents(t, events)
				if len(out) == 0 || errorStatus(out[len(out)-1].err) != status {
					t.Fatalf("transport status lost: %+v", out)
				}
				before := len(tr.snapshot())
				if _, _, err = ns.Start(context.Background(), next); err == nil || errorStatus(err) != 409 || len(tr.snapshot()) != before || opens.Load() != 1 {
					t.Fatal("denied/uncertain native result was resubmitted")
				}
			})
		}
	}
}

func TestNativeCloseWaitsForEndHookAndUnconsumedReaders(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ns.OnEnd = func(nativeReceipt, runEvent, error) { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := ns.Start(ctx, nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "unconsumed first")))
	tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "unconsumed second")))
	cancel() // OnEnd runs in context.AfterFunc, outside the engine worker WG.
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("end hook did not start")
	}
	closed := make(chan struct{})
	go func() { _ = ns.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while OnEnd still held its journal ownership")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join its unconsumed readers and end hook")
	}
}

func TestNativeCancellationWinsOverLateDeliveredAcknowledgement(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	events, r, err := ns.Start(ctx, nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestCall(1, "read"))
	tr.push(nativeTestCount(1))
	_ = nativeReadEvents(t, events)
	cancel()
	r.Delivered() // The AfterFunc need not have been scheduled yet.
	nativeWait(t, func() bool { return tr.closed.Load() == 1 && !ns.Busy() })
}

func TestNativeCloseJoinsUnconsumedBackgroundReader(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	_, _, err := ns.Start(context.Background(), nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "unconsumed first")))
	tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "unconsumed second")))
	closed := make(chan struct{})
	go func() { _ = ns.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("unconsumed subscriber leaked past store shutdown")
	}
}

func TestNativeExpiredPendingDeadlineRejectsBeforeDelayedTimerCallback(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	next, r, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	ns.mu.Lock()
	owner := ns.entries[r.RunID]
	ns.mu.Unlock()
	owner.mu.Lock()
	owner.pendingTimer.Stop()
	owner.segment.deadline = time.Now().Add(-time.Second)
	owner.mu.Unlock()
	before := len(tr.snapshot())
	_, _, err := ns.Start(context.Background(), next)
	if err == nil || errorStatus(err) != 409 || len(tr.snapshot()) != before || opens.Load() != 1 {
		t.Fatal("expired result raced a delayed TTL callback into a write")
	}
}

func TestNativeThinkingIsProgressForQuietWatchdog(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	ns.limits.Idle = 35 * time.Millisecond
	ns.limits.Quiet = 65 * time.Millisecond
	events, _, err := ns.Start(context.Background(), nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "thinking progress")))
		time.Sleep(12 * time.Millisecond)
	}
	tr.push(nativeTestFinal("thinking completed"))
	out := nativeReadEvents(t, events)
	if out[len(out)-1].err != nil || out[len(out)-1].stop != "stop" {
		t.Fatal("thinking incorrectly counted as heartbeat-only silence")
	}
}

func TestNativeAssistantMismatchDiagnosticsContainOnlyStructuralMetadata(t *testing.T) {
	for _, mode := range []string{"missing", "text", "reasoning", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			ns, tr, _ := nativeTestPool(t)
			next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
			private := "private rewritten content must not be logged"
			want := ""
			switch mode {
			case "missing":
				next.Chat.Messages = append(next.Chat.Messages[:2], next.Chat.Messages[3:]...)
				want = "assistant tool-call message is missing"
			case "text":
				next.Chat.Messages[2]["content"] = private
				want = fmt.Sprintf("assistant text changed: received_bytes=%d expected_bytes=0", len(private))
			case "reasoning":
				next.Chat.Messages[2]["reasoning_content"] = private
				want = fmt.Sprintf("assistant reasoning changed: received_bytes=%d expected_bytes=%d exact_unavailable_sentinel=false", len(private), len("synthetic reasoning"))
			case "unavailable":
				next.Chat.Messages[2]["reasoning_content"] = "[reasoning unavailable]"
				want = fmt.Sprintf("assistant reasoning changed: received_bytes=%d expected_bytes=%d exact_unavailable_sentinel=true", len("[reasoning unavailable]"), len("synthetic reasoning"))
			}
			_, _, err := ns.Start(context.Background(), next)
			if err == nil || errorStatus(err) != 409 || !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong structural diagnostic: %v", err)
			}
			if strings.Contains(err.Error(), private) || strings.Contains(err.Error(), "synthetic reasoning") || strings.Contains(err.Error(), "[reasoning unavailable]") {
				t.Fatal("assistant content leaked in mismatch diagnostic")
			}
		})
	}
}

func TestNativeCPAResponsesReasoningDisplayRoundtrip(t *testing.T) {
	cases := []struct {
		name, format, mode string
		allowed            bool
	}{
		{"responses exact CPA blocks", "openai-response", "combined", true},
		{"responses raw nonstream aggregate", "openai-response", "raw", true},
		{"chat cannot use CPA block rewrite", "openai", "combined", false},
		{"unknown format cannot use CPA block rewrite", "", "combined", false},
		{"reasoning block changed", "openai-response", "reasoning", false},
		{"arbitrary newlines rejected", "openai-response", "extra separator", false},
		{"visible text changed", "openai-response", "text", false},
		{"tool args changed", "openai-response", "calls", false},
		{"past reasoning remains strict", "openai-response", "past history", false},
		{"source format remains bound", "openai-response", "format", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, tr, opens := nativeTestPool(t)
			in := nativeTestInput()
			in.SourceFormat = tc.format
			in.Chat.Messages = append(in.Chat.Messages, map[string]any{"role": "assistant", "content": "previous text", "reasoning_content": "previous\n\nreasoning"}, map[string]any{"role": "user", "content": "current input"})
			in.Conversation, _ = conversation(in.Chat, in.Tools)
			events, r, err := ns.Start(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			// Empty text does not close a CPA reasoning block. Nonempty visible
			// text does, and the next reasoning block stays separate through tools.
			tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "R")))
			tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "")))
			tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "1")))
			tr.push(nativeTestUpdate(1, protobuf(nil).text(1, "visible text")))
			tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "R2")))
			tr.push(nativeTestCall(1, "read"))
			tr.push(nativeTestCount(1))
			out := nativeReadEvents(t, events)
			r.Delivered()
			next := nativeContinuation(t, in, out)
			current := len(in.Chat.Messages)
			next.Chat.Messages[current]["reasoning_content"] = "R1\n\nR2"
			switch tc.mode {
			case "raw":
				next.Chat.Messages[current]["reasoning_content"] = "R1R2"
			case "reasoning":
				next.Chat.Messages[current]["reasoning_content"] = "R1\n\nchanged R2"
			case "extra separator":
				next.Chat.Messages[current]["reasoning_content"] = "R1\n\n\nR2"
			case "text":
				next.Chat.Messages[current]["content"] = "changed visible text"
			case "calls":
				object(object(array(next.Chat.Messages[current]["tool_calls"])[0])["function"])["arguments"] = `{"path":"changed"}`
			case "past history":
				next.Chat.Messages[2]["reasoning_content"] = "previousreasoning"
			case "format":
				next.SourceFormat = "openai"
			}
			before := len(tr.snapshot())
			continued, rr, err := ns.Start(context.Background(), next)
			if !tc.allowed {
				if err == nil || errorStatus(err) != 409 || len(tr.snapshot()) != before || opens.Load() != 1 {
					t.Fatalf("invalid display/history reached native exec: %v", err)
				}
				return
			}
			if err != nil || !rr.Resumed || opens.Load() != 1 {
				t.Fatalf("known CPA display rewrite rejected: %v", err)
			}
			tr.push(nativeTestFinal("continued safely"))
			done := nativeReadEvents(t, continued)
			rr.Delivered()
			if done[len(done)-1].err != nil {
				t.Fatalf("native continuation failed: %+v", done)
			}
		})
	}
}

func TestNativeCPAResponsesUnavailablePlaceholderOnlyForAbsentReasoning(t *testing.T) {
	for _, format := range []string{"openai-response", "openai", ""} {
		for _, hasReasoning := range []bool{false, true} {
			t.Run(fmt.Sprintf("format-%s/reasoning-%t", format, hasReasoning), func(t *testing.T) {
				ns, tr, _ := nativeTestPool(t)
				in := nativeTestInput()
				in.SourceFormat = format
				events, r, err := ns.Start(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				if hasReasoning {
					tr.push(nativeTestUpdate(4, protobuf(nil).text(1, "actual reasoning")))
				}
				tr.push(nativeTestCall(1, "read"))
				tr.push(nativeTestCount(1))
				out := nativeReadEvents(t, events)
				r.Delivered()
				next := nativeContinuation(t, in, out)
				next.Chat.Messages[len(in.Chat.Messages)]["reasoning_content"] = "[reasoning unavailable]"
				continued, rr, err := ns.Start(context.Background(), next)
				allowed := format == "openai-response" && !hasReasoning
				if !allowed {
					if err == nil || errorStatus(err) != 409 {
						t.Fatalf("placeholder hid actual reasoning: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				tr.push(nativeTestFinal("done"))
				_ = nativeReadEvents(t, continued)
				rr.Delivered()
			})
		}
	}
}

func TestNativeCPAResponsesReasoningCombinerUsesKnownBlockSemantics(t *testing.T) {
	events := []runEvent{{reasoning: "same "}, {text: "visible"}, {reasoning: "same"}, {tool: map[string]any{"id": "call"}}}
	if got := nativeCPAResponsesReasoning(events); got != "same " {
		t.Fatalf("equal-trimmed block dedup changed: %q", got)
	}
	events = []runEvent{{reasoning: " R1 "}, {text: "visible"}, {reasoning: " R2 "}, {tool: map[string]any{"id": "call"}}}
	if got := nativeCPAResponsesReasoning(events); got != " R1 \n\n R2 " {
		t.Fatalf("block content was trimmed or normalized: %q", got)
	}
}
