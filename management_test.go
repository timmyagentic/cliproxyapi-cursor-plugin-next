package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManagementAPIKeyImport(t *testing.T) {
	for _, status := range []int{200, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			p := newTestRuntime(t)
			p.registerManagement(rpcRequest{})
			h := newMockHost(t)
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				if !strings.HasSuffix(req.URL, "/auth/exchange_user_api_key") {
					t.Fatalf("unexpected endpoint %s", req.URL)
				}
				if req.Headers.Get("Authorization") != "Bearer fixture-key" {
					t.Fatal("API key was not forwarded to Cursor")
				}
				return mockResponse{Status: status, Body: []byte(`{"accessToken":"fixture-token"}`)}, nil
			}
			saves := 0
			host := func(method string, raw []byte) ([]byte, error) {
				if method != "host.auth.save" {
					return h.call(method, raw)
				}
				saves++
				var saved struct {
					Name string          `json:"name"`
					JSON json.RawMessage `json:"json"`
				}
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				cred, stored, err := storedCredential(rpcRequest{StorageJSON: saved.JSON})
				if err != nil || cred.Type != "api" || cred.Key != "fixture-key" || stored["label"] != "Personal" || !strings.HasSuffix(saved.Name, ".json") {
					t.Fatalf("wrong saved credential: %v %v", stored, err)
				}
				return resultEnvelope(map[string]any{"name": saved.Name}), nil
			}
			result := invokeNative(t, p, "management.handle", map[string]any{
				"Method": "POST", "Path": "/v0/management/plugins/" + pluginID + "/api-key",
				"Body": []byte(`{"api_key":" fixture-key ","label":"Personal"}`), "host_callback_id": "import",
			}, host)
			response := object(result["result"])
			want := 502
			if status != 500 {
				want = status
			}
			if num(response["StatusCode"]) != float64(want) || (status == 200 && saves != 1) || (status != 200 && saves != 0) {
				t.Fatalf("unexpected response %v saves=%d", result, saves)
			}
			body, _ := base64.StdEncoding.DecodeString(str(response["Body"]))
			if strings.Contains(string(body), "fixture-key") || strings.Contains(string(body), "fixture-token") {
				t.Fatal("key leaked in response")
			}
		})
	}
}

func TestManagementSaveFailure(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	h.respond = func(context.Context, mockRequest) (mockResponse, error) {
		return mockResponse{Status: 200, Body: []byte(`{"accessToken":"fixture-token"}`)}, nil
	}
	s := p.newScope(func(method string, raw []byte) ([]byte, error) {
		if method == "host.auth.save" {
			return nil, fmt.Errorf("disk full")
		}
		return h.call(method, raw)
	}, "save-failure")
	defer s.close()
	status, body := p.importAPIKey(s, []byte(`{"api_key":"fixture-key"}`))
	if status != 502 || body["status"] == "ok" {
		t.Fatalf("save failure reported success: %d %v", status, body)
	}
}

func TestManagementLoginPageAndInvalidInput(t *testing.T) {
	p := newTestRuntime(t)
	p.registerManagement(rpcRequest{BasePath: "/v8/management", ResourceBasePath: "/v8/resource/plugins/" + pluginID})
	result := invokeNative(t, p, "management.handle", map[string]any{"Method": "GET", "Path": "/v8/resource/plugins/" + pluginID + "/login"}, nil)
	body, err := base64.StdEncoding.DecodeString(str(object(result["result"])["Body"]))
	if err != nil || !strings.Contains(string(body), `data-endpoint="/v8/management/plugins/`+pluginID+`/api-key"`) || !strings.Contains(string(body), "Cursor API Key") {
		t.Fatalf("wrong login page: %v %s", err, body)
	}
	s := p.newScope(func(string, []byte) ([]byte, error) {
		t.Fatal("invalid input called host")
		return nil, nil
	}, "invalid")
	defer s.close()
	for _, body := range []string{`{`, `{}`, `{"api_key":"  "}`, `{"api_key":"a\nb"}`, strings.Repeat("x", 16385)} {
		if status, _ := p.importAPIKey(s, []byte(body)); status != 400 {
			t.Fatalf("invalid input accepted: %d", status)
		}
	}
}

func TestAPIKeyLoginMetadata(t *testing.T) {
	p := newTestRuntime(t)
	s := p.newScope(nil, "")
	defer s.close()
	start, err := p.startLogin(s, rpcRequest{Metadata: map[string]any{"api_key": " fixture-key "}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.pollLogin(s, rpcRequest{State: str(start.(map[string]any)["State"])})
	if err != nil {
		t.Fatal(err)
	}
	auth := object(result.(map[string]any)["Auth"])
	cred, _, err := storedCredential(rpcRequest{StorageJSON: encodeJSON(auth["Metadata"])})
	if err != nil || cred.Type != "api" || cred.Key != "fixture-key" {
		t.Fatal("API key login did not produce API credentials")
	}
}
