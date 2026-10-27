package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeBlobGetPutCheckpointAndContentAddressedProtection(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	in := nativeTestInput()
	events, r, err := ns.Start(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	original := in.Conversation.Messages[0]
	key := digest(string(original))
	hash, _ := hex.DecodeString(key)
	tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 101).bytes(2, protobuf(nil).bytes(1, hash))))
	// A server-owned ID is opaque, not necessarily SHA256(data).
	opaque := []byte("opaque-fixed-root")
	value := []byte("synthetic server state")
	tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 102).bytes(3, protobuf(nil).bytes(1, opaque).bytes(2, value))))
	tr.push(protobuf(nil).bytes(3, protobuf(nil).bytes(1, opaque)))
	tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 103).bytes(2, protobuf(nil).bytes(1, opaque))))
	tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 104).bytes(2, protobuf(nil).bytes(1, []byte("missing")))))
	nativeWait(t, func() bool { return len(tr.snapshot()) >= 4 })
	got := map[uint64][]byte{}
	missing := false
	for _, b := range tr.snapshot() {
		fs, _ := pbFields(b)
		f := pbGet(fs, 3)
		if f == nil {
			continue
		}
		kv, _ := pbFields(f.data)
		if g := pbGet(kv, 2); g != nil {
			result, _ := pbFields(g.data)
			if value := pbGet(result, 1); value != nil {
				got[pbNum(kv, 1)] = value.data
			}
			if pbGet(result, 2) != nil {
				missing = true
			}
		}
	}
	if !bytes.Equal(got[101], original) || !bytes.Equal(got[103], value) || !missing {
		t.Fatalf("blob roundtrip wrong: %v missing=%v", got, missing)
	}
	ns.mu.Lock()
	owner := ns.entries[r.RunID]
	ns.mu.Unlock()
	owner.mu.Lock()
	retained := append([]byte(nil), owner.checkpoint...)
	owner.mu.Unlock()
	if len(retained) == 0 {
		t.Fatal("checkpoint not retained")
	}
	tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 105).bytes(3, protobuf(nil).bytes(1, hash).bytes(2, []byte("changed bootstrap bytes")))))
	out := nativeReadEvents(t, events)
	if len(out) == 0 || out[len(out)-1].err == nil || !strings.Contains(out[len(out)-1].err.Error(), "bootstrap blob changed") {
		t.Fatalf("bootstrap mutation accepted: %+v", out)
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) >= 5 })
	last := tr.snapshot()[4]
	fs, _ := pbFields(last)
	ack, _ := pbFields(pbGet(fs, 3).data)
	set, _ := pbFields(pbGet(ack, 3).data)
	if pbGet(set, 1) == nil {
		t.Fatal("rejected blob put did not send a failure acknowledgement")
	}
}

func TestNativeProtocolRetentionAndStrictArgumentLimits(t *testing.T) {
	t.Run("malformed checkpoint", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		events, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		tr.push(protobuf(nil).bytes(3, []byte{0x0a, 0xff}))
		out := nativeReadEvents(t, events)
		if out[len(out)-1].err == nil {
			t.Fatal("malformed checkpoint accepted")
		}
	})
	t.Run("blob cap", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		ns.limits.BlobBytes = 4096
		events, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		tr.push(protobuf(nil).bytes(4, protobuf(nil).uint(1, 1).bytes(3, protobuf(nil).bytes(1, []byte("server-root")).bytes(2, make([]byte, ns.limits.BlobBytes)))))
		out := nativeReadEvents(t, events)
		if out[len(out)-1].err == nil {
			t.Fatal("blob cap not enforced")
		}
	})
	t.Run("output cap", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		ns.limits.RetainedBytes = 2048
		events, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		tr.push(nativeTestUpdate(1, protobuf(nil).text(1, strings.Repeat("x", 3000))))
		out := nativeReadEvents(t, events)
		if len(out) != 1 || out[0].err == nil {
			t.Fatal("oversized output escaped retained bound")
		}
	})
	t.Run("MCP envelope", func(t *testing.T) {
		tests := [][]byte{
			protobuf(nil).text(5, "read").text(5, "write"),
			protobuf(nil).bytes(5, []byte{0xff}),
			protobuf(nil).text(5, "read").bytes(2, protobuf(nil).text(1, "path").uint(2, 1)),
			protobuf(nil).text(5, "read").bytes(2, protobuf(nil).text(1, "path").bytes(2, pbValue("a"))).bytes(2, protobuf(nil).text(1, "path").bytes(2, pbValue("b"))),
			protobuf(nil).text(5, "read").bytes(7, nil),
		}
		for i, raw := range tests {
			if _, _, err := nativeMCPArguments(raw, 1<<20); err == nil {
				t.Fatalf("malformed MCP case %d accepted", i)
			}
		}
		if _, _, err := nativeMCPArguments(make([]byte, 1<<20+1), 1<<20); err == nil {
			t.Fatal("argument byte cap ignored")
		}
	})
}

func TestNativeToolResultImagesErrorsAndUnsupportedResults(t *testing.T) {
	limits := defaultNativeLimits()
	raw, err := nativeEncodeResult(map[string]any{"content": []any{map[string]any{"type": "text", "text": "synthetic"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,c3ludGhldGlj"}}}, "is_error": true}, limits)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := pbFields(raw)
	success, _ := pbFields(pbGet(fs, 1).data)
	if pbNum(success, 2) != 1 {
		t.Fatal("tool failure flag not forwarded")
	}
	parts := 0
	for _, f := range success {
		if f.number == 1 {
			parts++
		}
	}
	if parts != 2 {
		t.Fatal("tool image/text parts lost")
	}
	for _, content := range []any{nil, 42, []any{map[string]any{"type": "image_url", "image_url": "https://example.invalid/image.png"}}, []any{map[string]any{"type": "image_url", "image_url": "data:image/png;base64,!"}}, []any{map[string]any{"type": "file", "data": "x"}}} {
		if _, err = nativeEncodeResult(map[string]any{"content": content}, limits); err == nil || errorStatus(err) != 409 {
			t.Fatalf("unsupported result accepted: %v", content)
		}
	}
}

func TestNativeRetransmissionAndServerExecCancellation(t *testing.T) {
	t.Run("exact retransmission", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		events, r, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		call := nativeTestCall(1, "read")
		tr.push(call)
		tr.push(call)
		tr.push(nativeTestCount(1))
		out := nativeReadEvents(t, events)
		r.Delivered()
		n := 0
		for _, e := range out {
			if e.tool != nil {
				n++
			}
		}
		if n != 1 {
			t.Fatal("retransmission duplicated a tool")
		}
	})
	t.Run("changed retransmission", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		events, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		tr.push(nativeTestCall(1, "read"))
		tr.push(nativeTestCall(1, "write"))
		out := nativeReadEvents(t, events)
		if out[len(out)-1].err == nil {
			t.Fatal("changed retransmission accepted")
		}
	})
	t.Run("server cancels parked exec", func(t *testing.T) {
		ns, tr, opens := nativeTestPool(t)
		next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
		tr.push(protobuf(nil).bytes(5, protobuf(nil).bytes(1, protobuf(nil).uint(1, 1))))
		nativeWait(t, func() bool { return tr.closed.Load() == 1 })
		before := len(tr.snapshot())
		_, _, err := ns.Start(context.Background(), next)
		if err == nil || errorStatus(err) != 409 || len(tr.snapshot()) != before || opens.Load() != 1 {
			t.Fatal("cancelled exec accepted late results")
		}
	})
}

func TestNativeAdmissionShutdownRaceAndReceiptRedaction(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	var endCalls atomic.Int32
	ns.OnStart = func(nativeReceipt) error { close(entered); <-release; return nil }
	ns.OnEnd = func(r nativeReceipt, ev runEvent, err error) {
		endCalls.Add(1)
		raw, e := json.Marshal(r)
		if e != nil || bytes.Contains(raw, []byte("synthetic-credential")) {
			t.Error("receipt cannot be safely marshalled")
		}
		close(ended)
	}
	startDone := make(chan struct{})
	go func() { defer close(startDone); _, _, _ = ns.Start(context.Background(), nativeTestInput()) }()
	<-entered
	closeDone := make(chan struct{})
	go func() { _ = ns.Close(); close(closeDone) }()
	close(release)
	select {
	case <-startDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown wedged opening Run")
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for admission and worker")
	}
	<-ended
	if opens.Load() > 1 || endCalls.Load() != 1 {
		t.Fatal("admission shutdown raced into duplicated lifecycle")
	}
	_ = tr
}

// Cursor's proto3 encoder omits request_id when it is zero. This is the
// first context/KV exchange of a real Run, not malformed/missing identity.
func TestNativeProto3OmittedZeroRequestIDs(t *testing.T) {
	t.Run("context and KV", func(t *testing.T) {
		ns, tr, _ := nativeTestPool(t)
		events, _, err := ns.Start(context.Background(), nativeTestInput())
		if err != nil {
			t.Fatal(err)
		}
		tr.push(protobuf(nil).bytes(2, protobuf(nil).bytes(10, nil)))
		tr.push(protobuf(nil).bytes(4, protobuf(nil).bytes(3, protobuf(nil).bytes(1, []byte("empty-server-root")))))
		nativeWait(t, func() bool { return len(tr.snapshot()) >= 3 })
		tr.push(nativeTestFinal("default-zero IDs accepted"))
		out := nativeReadEvents(t, events)
		if out[len(out)-1].err != nil || out[len(out)-1].stop != "stop" {
			t.Fatalf("valid proto3 zero rejected: %+v", out)
		}
	})
	t.Run("external zero exec is correlated on resume", func(t *testing.T) {
		ns, tr, opens := nativeTestPool(t)
		in := nativeTestInput()
		events, r, err := ns.Start(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := pbFields(nativeTestCall(0, "read"))
		exec, _ := pbFields(pbGet(full, 2).data)
		var withoutID protobuf
		for _, f := range exec {
			if f.number != 1 {
				withoutID = withoutID.bytes(f.number, f.data)
			}
		}
		tr.push(protobuf(nil).bytes(2, withoutID))
		tr.push(nativeTestCount(1))
		out := nativeReadEvents(t, events)
		r.Delivered()
		next := nativeContinuation(t, in, out)
		continued, rr, err := ns.Start(context.Background(), next)
		if err != nil {
			t.Fatal(err)
		}
		nativeWait(t, func() bool { return len(tr.snapshot()) >= 2 })
		fs, _ := pbFields(tr.snapshot()[0])
		result, _ := pbFields(pbGet(fs, 2).data)
		if pbNum(result, 1) != 0 || pbGet(result, 11) == nil {
			t.Fatal("default-zero exec result lost correlation")
		}
		tr.push(nativeTestFinal("zero exec finished"))
		done := nativeReadEvents(t, continued)
		rr.Delivered()
		if done[len(done)-1].err != nil || opens.Load() != 1 {
			t.Fatal("zero exec was rebuilt")
		}
	})
}

func TestNativeUnknownScalarAndOpaqueFieldsAreForwardCompatible(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	events, _, err := ns.Start(context.Background(), nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	tr.push(protobuf(nil).uint(99, 7).bytes(100, []byte("opaque non-protobuf extension")))
	tr.push(protobuf(nil).bytes(1, protobuf(nil).uint(90, 1).double(91, 1.5).bytes(92, []byte("opaque extension"))))
	tr.push(protobuf(nil).bytes(1, protobuf(nil).uint(13, 1)))
	tr.push(nativeTestFinal("future fields skipped"))
	out := nativeReadEvents(t, events)
	if len(out) == 0 || out[len(out)-1].err != nil || out[len(out)-1].stop != "stop" {
		t.Fatalf("unknown valid protobuf field rejected: %+v", out)
	}
}
