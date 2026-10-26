package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type syntheticStream struct {
	body  []byte
	ready <-chan struct{}
	read  bool
}

type syntheticHost struct {
	t        *testing.T
	mu       sync.Mutex
	next     int
	streams  map[string]*syntheticStream
	runs     map[string]chan struct{}
	requests []string
	chunks   []byte
	closed   chan struct{}
	context  string
}

func newSyntheticHost(t *testing.T, callbackID string) *syntheticHost {
	return &syntheticHost{t: t, streams: make(map[string]*syntheticStream), runs: make(map[string]chan struct{}), closed: make(chan struct{}), context: callbackID}
}

func hostResult(value any) []byte {
	raw, _ := json.Marshal(value)
	return envelope(message{Result: raw})
}

func wireBytes(field int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field*8+2))
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func wireFrame(value []byte, flags byte) []byte {
	out := make([]byte, 5)
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:], uint32(len(value)))
	return append(out, value...)
}

func syntheticReply() []byte {
	text := wireFrame(wireBytes(1, wireBytes(1, wireBytes(1, []byte("host-routed reply")))), 0)
	usage := wireFrame(wireBytes(1, wireBytes(14, []byte{8, 10, 16, 5})), 0)
	return append(append(text, usage...), wireFrame([]byte(`{}`), 2)...)
}

func (h *syntheticHost) call(method string, raw []byte) ([]byte, error) {
	var req struct {
		CallbackID  string              `json:"host_callback_id"`
		URL         string              `json:"url"`
		Headers     map[string][]string `json:"headers"`
		Body        []byte              `json:"body"`
		StreamID    string              `json:"stream_id"`
		OperationID string              `json:"operation_id"`
		Payload     []byte              `json:"payload"`
		Error       string              `json:"error"`
		WireProfile struct {
			HTTP1 bool `json:"http1_only"`
		} `json:"wire_profile"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.CallbackID != h.context {
		return nil, fmt.Errorf("lost host callback context: %q", req.CallbackID)
	}
	switch method {
	case "host.http.operation_open":
		h.mu.Lock()
		h.next++
		id := fmt.Sprint(h.next)
		h.mu.Unlock()
		return hostResult(map[string]any{"operation_id": id}), nil
	case "host.http.do_stream":
		if !req.WireProfile.HTTP1 {
			return nil, fmt.Errorf("missing host HTTP/1.1 profile")
		}
		h.mu.Lock()
		h.requests = append(h.requests, req.URL)
		h.next++
		id := fmt.Sprint(h.next)
		body := []byte(`{}`)
		var ready <-chan struct{}
		contentType := "application/json"
		switch {
		case strings.HasSuffix(req.URL, "GetCurrentPeriodUsage"):
			body = []byte(`{"billingCycleEnd":"1793491200000","planUsage":{"autoPercentUsed":12.5,"apiPercentUsed":40,"totalPercentUsed":20}}`)
		case strings.HasSuffix(req.URL, "GetPlanInfo"):
			body = []byte(`{"planInfo":{"planName":"Pro"}}`)
		case strings.HasSuffix(req.URL, "GetHardLimit"):
			body = []byte(`{"noUsageBasedAllowed":true}`)
		case strings.HasSuffix(req.URL, "AvailableModels"):
			body = []byte(`{"models":[{"name":"fixture-model","displayName":"Fixture"}]}`)
		case strings.HasSuffix(req.URL, "GetUsableModels"):
			body = []byte(`{"models":[{"modelId":"fixture-model"}]}`)
		case strings.HasSuffix(req.URL, "RunSSE"):
			contentType = "application/connect+proto"
			requestID := req.Headers["X-Request-Id"][0]
			ch := h.runs[requestID]
			if ch == nil {
				ch = make(chan struct{})
				h.runs[requestID] = ch
			}
			ready = ch
			body = syntheticReply()
		case strings.HasSuffix(req.URL, "BidiAppend"):
			requestID := req.Headers["X-Request-Id"][0]
			ch := h.runs[requestID]
			if ch == nil {
				ch = make(chan struct{})
				h.runs[requestID] = ch
			}
			select {
			case <-ch:
			default:
				close(ch)
			}
			if len(req.Body) == 0 {
				h.t.Error("BidiAppend lost binary request body")
			}
		}
		h.streams[id] = &syntheticStream{body: body, ready: ready}
		h.mu.Unlock()
		return hostResult(map[string]any{"status_code": 200, "headers": map[string][]string{"content-type": {contentType}}, "stream_id": id}), nil
	case "host.http.stream_read":
		h.mu.Lock()
		stream := h.streams[req.StreamID]
		if stream == nil {
			h.mu.Unlock()
			return hostResult(map[string]any{"done": true}), nil
		}
		already := stream.read
		stream.read = true
		h.mu.Unlock()
		if already {
			return hostResult(map[string]any{"done": true}), nil
		}
		if stream.ready != nil {
			select {
			case <-stream.ready:
			case <-time.After(5 * time.Second):
				return nil, fmt.Errorf("BidiAppend did not run concurrently with RunSSE read")
			}
		}
		return hostResult(map[string]any{"payload": stream.body}), nil
	case "host.http.cancel", "host.http.stream_close":
		if req.StreamID != "" {
			h.mu.Lock()
			delete(h.streams, req.StreamID)
			h.mu.Unlock()
		}
		return hostResult(map[string]any{}), nil
	case "host.stream.emit":
		h.mu.Lock()
		h.chunks = append(h.chunks, req.Payload...)
		h.mu.Unlock()
		return hostResult(map[string]any{}), nil
	case "host.stream.close":
		if req.Error != "" {
			h.t.Errorf("stream ended in error: %s", req.Error)
		}
		close(h.closed)
		return hostResult(map[string]any{}), nil
	default:
		return nil, fmt.Errorf("unexpected host callback %s", method)
	}
}

func syntheticCredentials() string {
	return base64.StdEncoding.EncodeToString([]byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"synthetic-host-token","refresh":"none","expires":9999999999999}}`))
}

func TestQuotaUsesHostHTTPCallbacks(t *testing.T) {
	w := newTestRuntime(t)
	host := newSyntheticHost(t, "quota-context")
	req, _ := json.Marshal(map[string]any{"host_callback_id": host.context, "storage_json": syntheticCredentials(), "host": map[string]any{"ProxyURL": "socks5://cpa-global-proxy:1080"}})
	raw := dispatch(w, "quota.fetch", req, host.call)
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Subscription struct {
				Plan string `json:"plan"`
			} `json:"subscription"`
			Groups []struct {
				Buckets []struct {
					Remaining float64 `json:"remainingFraction"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Result.Subscription.Plan != "Pro" {
		t.Fatalf("invalid quota: %s", raw)
	}
	if response.Result.Groups[0].Buckets[0].Remaining != 0.875 {
		t.Fatalf("wrong normalized quota: %s", raw)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 3 {
		t.Fatalf("expected period/plan/hard-limit HTTP requests, got %v", host.requests)
	}
}

func TestCursorExecutionUsesHostRunSSEAndBidiAppend(t *testing.T) {
	for _, format := range []string{"openai", "chat-completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				w := newTestRuntime(t)
				host := newSyntheticHost(t, "execute-context")
				payload := base64.StdEncoding.EncodeToString([]byte(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}]}`))
				req, _ := json.Marshal(map[string]any{"host_callback_id": host.context, "StorageJSON": syntheticCredentials(), "Format": format, "Model": "cursor/fixture-model", "Payload": payload, "stream_id": "downstream"})
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				raw := dispatch(w, method, req, host.call)
				var response struct {
					OK     bool                     `json:"ok"`
					Result struct{ Payload []byte } `json:"result"`
				}
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if !response.OK {
					t.Fatalf("execution failed: %s", raw)
				}
				if stream {
					select {
					case <-host.closed:
					case <-time.After(5 * time.Second):
						t.Fatal("stream never closed")
					}
				}
				host.mu.Lock()
				defer host.mu.Unlock()
				output := response.Result.Payload
				if stream {
					output = host.chunks
				}
				if !strings.Contains(string(output), "host-routed reply") {
					t.Fatalf("missing model reply: %s", output)
				}
				if stream && !strings.Contains(string(output), "[DONE]") {
					t.Fatal("missing stream end")
				}
				joined := strings.Join(host.requests, "\n")
				if !strings.Contains(joined, "RunSSE") || !strings.Contains(joined, "BidiAppend") {
					t.Fatalf("model bypassed host HTTP transport: %s", joined)
				}
			})
		}
	}
}

func TestMissingHostHTTPContextDoesNotUseDirectNetwork(t *testing.T) {
	w := newTestRuntime(t)
	req, _ := json.Marshal(map[string]any{"storage_json": syntheticCredentials()})
	raw := dispatch(w, "quota.fetch", req, func(string, []byte) ([]byte, error) {
		t.Error("host callback without CPA request context")
		return nil, nil
	})
	if !strings.Contains(string(raw), `"ok":false`) || !strings.Contains(string(raw), "request scope") {
		t.Fatalf("missing context did not fail: %s", raw)
	}
}
