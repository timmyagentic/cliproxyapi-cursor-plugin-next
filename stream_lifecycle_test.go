package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamShutdownDrainsBlockedFinalEmitBeforeReturning(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	host := newSyntheticHost(t, "synthetic-lifecycle-context")
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var shutdownReturned atomic.Bool
	var callbacksAfterShutdown atomic.Int64
	caller := func(method string, raw []byte) ([]byte, error) {
		if shutdownReturned.Load() {
			callbacksAfterShutdown.Add(1)
		}
		if method == "host.stream.emit" {
			var fields struct{ Payload []byte }
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, err
			}
			if string(fields.Payload) == "data: [DONE]\n\n" {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
		}
		return host.call(method, raw)
	}
	req := encodeJSON(map[string]any{
		"host_callback_id": host.context, "StorageJSON": syntheticCredentials(),
		"Format": "openai", "Model": "cursor/fixture-model", "stream_id": "synthetic-lifecycle-stream",
		"Payload": []byte(`{"messages":[{"role":"user","content":"synthetic lifecycle check"}]}`),
	})
	result, err := jsonObject(dispatch(p, "executor.execute_stream", req, caller))
	if err != nil || result["ok"] != true {
		t.Fatal("synthetic stream did not start")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stream never reached its final emit")
	}
	stopped := make(chan []byte, 1)
	go func() {
		// Shutdown has its own request scope. Draining other scopes must not
		// wait for this dispatch invocation to return to its own defer.
		out := dispatch(p, "plugin.shutdown", encodeJSON(map[string]any{"host_callback_id": "synthetic-shutdown-context"}), nil)
		shutdownReturned.Store(true)
		stopped <- out
	}()
	select {
	case <-p.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not begin cancellation")
	}
	select {
	case <-stopped:
		t.Fatal("shutdown returned while a final host callback was still blocked")
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case raw := <-stopped:
		result, err := jsonObject(raw)
		if err != nil || result["ok"] != true {
			t.Fatal("shutdown failed after the callback drained")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown deadlocked with a stream worker or its own scope")
	}
	if callbacksAfterShutdown.Load() != 0 {
		t.Fatal("a host callback began after plugin shutdown returned")
	}
	if err := p.startStreamWorker(func() { callbacksAfterShutdown.Add(1) }); errorStatus(err) != 503 {
		t.Fatal("shutdown admitted another stream worker")
	}
	p.mu.Lock()
	scopeCount := len(p.scopes)
	p.mu.Unlock()
	if scopeCount != 0 {
		t.Fatal("shutdown retained request scopes")
	}
}

func TestStreamScopeConcurrentCloseWaitsForAdmittedCallbacks(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var calls atomic.Int64
	s := p.newScope(func(method string, raw []byte) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return hostResult(map[string]any{}), nil
	}, "synthetic-scope-context")
	called := make(chan error, 1)
	go func() {
		called <- s.streamCall("host.stream.emit", map[string]any{"stream_id": "synthetic-stream"}, nil)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic callback did not begin")
	}
	closed := make(chan struct{}, 2)
	for n := 0; n < 2; n++ {
		go func() { s.close(); closed <- struct{}{} }()
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("scope close did not cancel the request")
	}
	select {
	case <-closed:
		t.Fatal("concurrent close returned before an admitted callback completed")
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for n := 0; n < 2; n++ {
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent scope close deadlocked")
		}
	}
	if err := <-called; err != nil {
		t.Fatal("already admitted callback failed to drain", err)
	}
	if err := s.streamCall("host.stream.close", map[string]any{"stream_id": "synthetic-stream"}, nil); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("closed scope accepted a new callback")
	}
}

func TestStreamShutdownJoinsWorkerAfterItsCallbacksHaveFinished(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	entered, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	if err := p.startStreamWorker(func() {
		defer close(workerDone)
		close(entered)
		<-p.ctx.Done()
		// Model a worker doing final local cleanup after its last callback.
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	stopped := make(chan struct{})
	go func() { p.stop(); close(stopped) }()
	<-p.ctx.Done()
	select {
	case <-stopped:
		t.Fatal("shutdown joined callbacks but left the stream worker running")
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join the released stream worker")
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("shutdown returned before worker completion")
	}
}
