package main

import (
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func imageContinuationProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || req.Host != "8.8.8.8:80" {
			t.Error("media fetch did not use its pinned explicit route")
			w.WriteHeader(400)
			return
		}
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestExecutorNativeContinuationDoesNotRefetchExpiredHistoricalImages(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(testImageBase64)
	var fetches atomic.Int32
	var expired atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fetches.Add(1)
		if expired.Load() {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer origin.Close()
	proxy := imageContinuationProxy(t, origin.Listener.Addr().String())
	route, err := newNativeTransportPool(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestRuntime(t)
	ns, tr, opens := nativeTestPool(t)
	p.nativeSessions, p.nativePool = ns, route
	host := newSyntheticHost(t, "media-continuation")
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}
	execute := func(runtime *pluginRuntime, messages []map[string]any) map[string]any {
		return invokeNative(t, runtime, "executor.execute", map[string]any{
			"host_callback_id": host.context, "StorageJSON": syntheticCredentials(), "Format": "openai",
			"Model": "cursor/fixture-model", "AuthID": "synthetic-media-account",
			"Metadata": map[string]any{"canonical_session_id": "synthetic-media-session"},
			"Payload":  encodeJSON(map[string]any{"messages": messages, "tools": tools}),
		}, host.call)
	}
	messageOf := func(result map[string]any) map[string]any {
		t.Helper()
		if result["ok"] != true {
			t.Fatalf("execution failed: %v", result)
		}
		raw, _ := base64.StdEncoding.DecodeString(str(object(result["result"])["Payload"]))
		body, parseErr := jsonObject(raw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		return object(object(array(body["choices"])[0])["message"])
	}
	user := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "read the file for this image"},
		map[string]any{"type": "image_url", "image_url": "http://8.8.8.8/signed.png?signature=synthetic"},
	}}
	tr.push(nativeTestCall(1, "read"))
	tr.push(nativeTestCount(1))
	assistant := messageOf(execute(p, []map[string]any{user}))
	call := object(array(assistant["tool_calls"])[0])
	result := map[string]any{"role": "tool", "tool_call_id": call["id"], "content": "verified synthetic file result"}
	continuation := []map[string]any{user, assistant, result}
	if fetches.Load() != 1 || opens.Load() != 1 {
		t.Fatal("initial Run did not import its image exactly once")
	}
	expired.Store(true)

	unknownID := "call_cursor_" + strings.Repeat("0", 32) + "_1_0"
	unknownCall := copyMap(call)
	unknownCall["id"] = unknownID
	unknownAssistant := copyMap(assistant)
	unknownAssistant["tool_calls"] = []any{unknownCall}
	unknownResult := copyMap(result)
	unknownResult["tool_call_id"] = unknownID
	unknown := []map[string]any{user, unknownAssistant, unknownResult}
	response := execute(p, unknown)
	if num(object(response["error"])["http_status"]) != 400 || fetches.Load() != 1 {
		t.Fatalf("unknown handle was masked by historical media preparation: %v, fetches=%d", response, fetches.Load())
	}

	ns.mu.Lock()
	owner := ns.handles[str(call["id"])].owner
	ns.mu.Unlock()
	continued := make(chan map[string]any, 1)
	go func() { continued <- execute(p, continuation) }()
	nativeWait(t, func() bool {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		return owner.state == "running"
	})
	tr.push(nativeTestFinal("continued without downloading the expired image"))
	var continuedResponse map[string]any
	select {
	case continuedResponse = <-continued:
	case <-time.After(2 * time.Second):
		t.Fatal("resumed request did not complete")
	}
	final := messageOf(continuedResponse)
	if fetches.Load() != 1 || opens.Load() != 1 || !strings.Contains(str(final["content"]), "continued") {
		t.Fatal("a valid native continuation rebuilt media or opened a second Run")
	}
	newTurn := append(append([]map[string]any{}, continuation...), final, map[string]any{"role": "user", "content": "look at the image again"})
	response = execute(p, newTurn)
	if num(object(response["error"])["http_status"]) != 400 || fetches.Load() != 2 || opens.Load() != 1 {
		t.Fatalf("new ordinary turn failed to prepare complete media before opening: %v, fetches=%d", response, fetches.Load())
	}

	stateless := newTestRuntime(t)
	response = execute(stateless, []map[string]any{user})
	if num(object(response["error"])["http_status"]) != 400 || !strings.Contains(str(object(response["error"])["message"]), "explicit native transport route") {
		t.Fatal("host mode silently skipped unsupported history media")
	}
	response = execute(stateless, unknown)
	if num(object(response["error"])["http_status"]) != 400 || fetches.Load() != 2 {
		t.Fatal("lost native owner was replayed/prepared as a host-mode request")
	}
}
