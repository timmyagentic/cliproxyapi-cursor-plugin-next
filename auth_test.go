package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func invokeNative(t *testing.T, p *pluginRuntime, method string, req any, host hostCaller) map[string]any {
	t.Helper()
	if req == nil {
		req = map[string]any{}
	}
	var result map[string]any
	raw := dispatch(p, method, encodeJSON(req), host)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestNativeRegistrationAndErrorsWithoutNode(t *testing.T) {
	t.Setenv("CURSOR_PLUGIN_NODE", "/node-does-not-exist")
	p := newTestRuntime(t)
	result := invokeNative(t, p, "plugin.register", nil, nil)
	if result["ok"] != true {
		t.Fatal(result)
	}
	if str(object(object(result["result"])["metadata"])["Version"]) != version {
		t.Fatal("wrong version")
	}
	for _, method := range []string{"executor.count_tokens", "executor.http_request", "quota.reset"} {
		result := invokeNative(t, p, method, map[string]any{}, nil)
		if num(object(result["error"])["http_status"]) != 501 {
			t.Fatal(result)
		}
	}
	p.stop()
	if invokeNative(t, p, "model.static", map[string]any{}, nil)["ok"] != false {
		t.Fatal("shutdown accepted work")
	}
}
func TestNativeAuthStorageAndProxyPolicy(t *testing.T) {
	p := newTestRuntime(t)
	stored := map[string]any{"type": "cursor", "label": "Personal", "prefix": "work", "disabled": true, "cursor_auth": map[string]any{"type": "api", "key": "fixture-key"}}
	result := invokeNative(t, p, "auth.parse", map[string]any{"RawJSON": encodeJSON(stored), "FileName": "personal.json"}, nil)
	record := object(object(result["result"])["Auth"])
	if record["FileName"] != "personal.json" || record["Prefix"] != "work" || record["Disabled"] != true {
		t.Fatal(record)
	}
	raw, _ := base64.StdEncoding.DecodeString(str(record["StorageJSON"]))
	parsed, _ := jsonObject(raw)
	if !reflect.DeepEqual(parsed, stored) {
		t.Fatalf("storage changed: %s", raw)
	}
	other := invokeNative(t, p, "auth.parse", map[string]any{"RawJSON": []byte(`{"type":"codex"}`)}, nil)
	if object(other["result"])["Handled"] != false {
		t.Fatal(other)
	}
	_, _, err := storedCredential(rpcRequest{StorageJSON: []byte(`{"type":"cursor"}`)})
	if errorStatus(err) != 401 {
		t.Fatal(err)
	}
	if checkProxy(rpcRequest{}, map[string]any{"proxy_url": "socks5://account:1080"}) == nil {
		t.Fatal("silently ignored account proxy")
	}
}
func TestNativeLoginPKCEPendingSuccessAndExpiry(t *testing.T) {
	p := newTestRuntime(t)
	now := time.Unix(1800000000, 0)
	p.now = func() time.Time { return now }
	h := newMockHost(t)
	s := p.newScope(h.call, "login")
	defer s.close()
	var polled *url.URL
	polls := 0
	h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
		if strings.Contains(req.URL, "/auth/poll?") {
			polled, _ = url.Parse(req.URL)
			polls++
			if polls == 1 {
				return mockResponse{Status: 404, Body: []byte(`{}`)}, nil
			}
			return mockResponse{Status: 200, Body: []byte(`{"accessToken":"fixture-token","refreshToken":""}`)}, nil
		}
		return mockResponse{Status: 200, Body: []byte(`{"email":"fixture@example.invalid"}`)}, nil
	}
	started, err := p.startLogin(s, rpcRequest{})
	if err != nil {
		t.Fatal(err)
	}
	start := started.(map[string]any)
	u, _ := url.Parse(str(start["URL"]))
	state := str(start["State"])
	pending, err := p.pollLogin(s, rpcRequest{State: state})
	if err != nil || pending.(map[string]any)["Status"] != "pending" {
		t.Fatalf("unexpected pending: %v %v", pending, err)
	}
	result, err := p.pollLogin(s, rpcRequest{State: state})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(polled.Query().Get("verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != u.Query().Get("challenge") {
		t.Fatal("PKCE mismatch")
	}
	if strings.Contains(state, polled.Query().Get("verifier")) {
		t.Fatal("state exposed PKCE verifier")
	}
	if result.(map[string]any)["Status"] != "success" {
		t.Fatal(result)
	}
	repeated, err := p.pollLogin(s, rpcRequest{State: state})
	if err != nil || !reflect.DeepEqual(repeated, result) || polls != 2 {
		t.Fatal("login poll was not idempotent")
	}
	now = now.Add(16 * time.Minute)
	expired, _ := p.pollLogin(s, rpcRequest{State: state})
	if expired.(map[string]any)["Status"] != "error" {
		t.Fatal("login did not expire")
	}
}
func TestNativeAPIKeyExchangeAndTokenExpiry(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	s := p.newScope(h.call, "key")
	defer s.close()
	exchanges := 0
	h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
		if !strings.HasSuffix(req.URL, "exchange_user_api_key") {
			t.Errorf("unexpected endpoint %s", req.URL)
		}
		exchanges++
		return mockResponse{Status: 200, Body: []byte(`{"accessToken":"exchanged-token"}`)}, nil
	}
	req := rpcRequest{StorageJSON: []byte(`{"type":"cursor","cursor_auth":{"type":"api","key":"key"}}`)}
	for i := 0; i < 2; i++ {
		token, err := p.token(s, req)
		if err != nil || token != "exchanged-token" {
			t.Fatalf("exchange failed %q %v", token, err)
		}
	}
	if exchanges != 1 {
		t.Fatal("API token was not cached")
	}
	expired := rpcRequest{StorageJSON: []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"expired","expires":1}}`)}
	if _, err := p.token(s, expired); errorStatus(err) != 401 {
		t.Fatal("expired token accepted")
	}
}
func TestNativeConcurrentAuthIsolation(t *testing.T) {
	p := newTestRuntime(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('A' + i))
			req := map[string]any{"RawJSON": encodeJSON(map[string]any{"type": "cursor", "cursor_auth": map[string]any{"type": "api", "key": key}})}
			result := invokeNative(t, p, "auth.parse", req, nil)
			stored := object(object(object(result["result"])["Auth"])["Metadata"])
			if object(stored["cursor_auth"])["key"] != key {
				t.Error("credentials crossed calls")
			}
		}(i)
	}
	wg.Wait()
}
