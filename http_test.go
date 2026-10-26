package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

type mockRequest struct {
	URL, Method string
	Body        []byte
	Headers     http.Header
	Operation   string `json:"operation_id"`
	Stream      string `json:"stream_id"`
	Callback    string `json:"host_callback_id"`
}
type mockResponse struct {
	Status int
	Body   []byte
	Read   func(context.Context) ([]byte, bool, error)
}
type mockOperation struct {
	ctx    context.Context
	cancel context.CancelFunc
}
type mockStream struct {
	op       mockOperation
	response mockResponse
	read     bool
}
type mockHost struct {
	mu         sync.Mutex
	next       int
	operations map[string]mockOperation
	streams    map[string]*mockStream
	requests   []mockRequest
	respond    func(context.Context, mockRequest) (mockResponse, error)
}

func newMockHost(t *testing.T) *mockHost {
	h := &mockHost{operations: map[string]mockOperation{}, streams: map[string]*mockStream{}}
	h.respond = func(context.Context, mockRequest) (mockResponse, error) {
		return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
	}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, op := range h.operations {
			op.cancel()
		}
	})
	return h
}
func (h *mockHost) call(method string, raw []byte) ([]byte, error) {
	var req mockRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Callback == "" {
		return nil, fmt.Errorf("missing host callback context")
	}
	switch method {
	case "host.http.operation_open":
		h.mu.Lock()
		h.next++
		id := fmt.Sprint(h.next)
		ctx, cancel := context.WithCancel(context.Background())
		h.operations[id] = mockOperation{ctx, cancel}
		h.mu.Unlock()
		return hostResult(map[string]any{"operation_id": id}), nil
	case "host.http.do_stream":
		h.mu.Lock()
		op, exists := h.operations[req.Operation]
		h.requests = append(h.requests, req)
		h.mu.Unlock()
		if !exists {
			return nil, context.Canceled
		}
		response, err := h.respond(op.ctx, req)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.next++
		id := fmt.Sprint(h.next)
		h.streams[id] = &mockStream{op: op, response: response}
		h.mu.Unlock()
		return hostResult(map[string]any{"status_code": response.Status, "headers": http.Header{"Content-Type": {"application/json"}}, "stream_id": id}), nil
	case "host.http.stream_read":
		h.mu.Lock()
		stream := h.streams[req.Stream]
		if stream == nil {
			h.mu.Unlock()
			return nil, context.Canceled
		}
		already := stream.read
		stream.read = true
		h.mu.Unlock()
		if stream.response.Read != nil {
			body, done, err := stream.response.Read(stream.op.ctx)
			if err != nil {
				return nil, err
			}
			return hostResult(map[string]any{"payload": body, "done": done}), nil
		}
		if already {
			return hostResult(map[string]any{"done": true}), nil
		}
		return hostResult(map[string]any{"payload": stream.response.Body}), nil
	case "host.http.cancel":
		h.mu.Lock()
		op := h.operations[req.Operation]
		delete(h.operations, req.Operation)
		h.mu.Unlock()
		if op.cancel != nil {
			op.cancel()
		}
		return hostResult(map[string]any{}), nil
	case "host.http.stream_close":
		h.mu.Lock()
		delete(h.streams, req.Stream)
		h.mu.Unlock()
		return hostResult(map[string]any{}), nil
	default:
		return nil, fmt.Errorf("unexpected callback %s", method)
	}
}
func (h *mockHost) assertClosed(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.operations) != 0 || len(h.streams) != 0 {
		t.Errorf("HTTP handles leaked: %d operations, %d streams", len(h.operations), len(h.streams))
	}
}

func TestHTTPBodyOnDemandAndClose(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	s := p.newScope(h.call, "ctx")
	defer s.close()
	reads := 0
	h.respond = func(context.Context, mockRequest) (mockResponse, error) {
		return mockResponse{Status: 429, Read: func(context.Context) ([]byte, bool, error) { reads++; return []byte("limited"), true, nil }}, nil
	}
	response, err := s.do(s.ctx, http.MethodPost, "https://api2.cursor.sh/test", headers("synthetic"), []byte{0, 255})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatal("response was prefetched")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "limited" || response.StatusCode != 429 {
		t.Fatalf("invalid response %d %q", response.StatusCode, body)
	}
	h.assertClosed(t)
	if h.requests[0].Headers.Get("Authorization") != "Bearer synthetic" {
		t.Fatal("lost canonical auth header")
	}
}
func TestHTTPCancellationBeforeHead(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	s := p.newScope(h.call, "ctx")
	defer s.close()
	started := make(chan struct{})
	h.respond = func(ctx context.Context, _ mockRequest) (mockResponse, error) {
		close(started)
		<-ctx.Done()
		return mockResponse{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.do(ctx, http.MethodGet, "https://api2.cursor.sh/head", headers(""), nil)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("head cancellation stalled")
	}
	h.assertClosed(t)
}
func TestHTTPCancellationDuringSilentRead(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	s := p.newScope(h.call, "ctx")
	defer s.close()
	started := make(chan struct{})
	h.respond = func(context.Context, mockRequest) (mockResponse, error) {
		return mockResponse{Status: 200, Read: func(ctx context.Context) ([]byte, bool, error) {
			close(started)
			<-ctx.Done()
			return nil, true, ctx.Err()
		}}, nil
	}
	response, err := s.do(s.ctx, http.MethodGet, "https://api2.cursor.sh/silent", headers(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(response.Body); done <- err }()
	<-started
	s.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent cancellation succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read cancellation stalled")
	}
	h.assertClosed(t)
}
func TestHTTPCallbackContextAndStatusValidation(t *testing.T) {
	p := newTestRuntime(t)
	s := p.newScope(func(method string, raw []byte) ([]byte, error) {
		var req mockRequest
		_ = json.Unmarshal(raw, &req)
		if req.Callback != "trusted" {
			t.Fatal("lost callback context")
		}
		return errorEnvelope(failure(403, "host refused")), nil
	}, "trusted")
	defer s.close()
	_, err := s.do(s.ctx, http.MethodGet, "https://api2.cursor.sh/test", headers(""), nil)
	if errorStatus(err) != 403 {
		t.Fatalf("status lost: %v", err)
	}
}
