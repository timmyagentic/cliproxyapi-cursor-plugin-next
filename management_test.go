package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestManagementUsageRoutesKeepPrivateDataAuthenticated(t *testing.T) {
	p := newTestRuntime(t)
	registered := p.registerManagement(rpcRequest{BasePath: "/v8/management/", ResourceBasePath: "/v8/resource/plugins/" + pluginID + "/"}).(map[string]any)
	routes := registered["routes"].([]any)
	for _, expected := range []map[string]any{
		{"Method": "POST", "Path": "/v8/management/plugins/" + pluginID + "/api-key"},
		{"Method": "GET", "Path": "/v8/management/plugins/" + pluginID + "/accounting"},
		{"Method": "GET", "Path": "/v8/management/plugins/" + pluginID + "/accounting/runs"},
	} {
		found := false
		for _, route := range routes {
			found = found || reflect.DeepEqual(route, expected)
		}
		if !found {
			t.Fatalf("missing authenticated management route %v", expected)
		}
	}
	resources := registered["resources"].([]any)
	if len(resources) != 2 || object(resources[0])["Path"] != "/v8/resource/plugins/"+pluginID+"/login" || object(resources[1])["Path"] != "/v8/resource/plugins/"+pluginID+"/usage" || object(resources[1])["Menu"] != "Cursor 用量" {
		t.Fatalf("login/usage menu registration changed: %v", resources)
	}

	now := time.Now().UTC()
	record, err := p.journal.Begin("private-account-sentinel", "private-conversation-sentinel", "private-run-sentinel", "private-model-sentinel", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.journal.Close(record.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := p.journal.Complete(record.ID, journalUsage(100, 80, 1, 20, "private-event-sentinel"), "turn_ended"); err != nil {
		t.Fatal(err)
	}
	before := p.journal.Health()
	result := invokeNative(t, p, "management.handle", map[string]any{"Method": "GET", "Path": "/v8/resource/plugins/" + pluginID + "/usage"}, nil)
	response := object(result["result"])
	body, err := base64.StdEncoding.DecodeString(str(response["Body"]))
	if err != nil || num(response["StatusCode"]) != 200 || !strings.Contains(string(body), `data-endpoint="/v8/management/plugins/`+pluginID+`/accounting/runs"`) || !strings.Contains(string(body), "Cursor 用量") {
		t.Fatalf("usage resource did not render its configured endpoint: %v", err)
	}
	for _, private := range []string{record.ID, record.Account, record.Conversation, record.RunID, record.Model, "private-event-sentinel"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("public resource embedded journal data %q", private)
		}
	}
	for _, forbidden := range []string{"innerHTML", "localStorage.setItem", "sessionStorage", "console.log"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("usage resource contains unsafe data/auth handling %q", forbidden)
		}
	}
	result = invokeNative(t, p, "management.handle", map[string]any{"Method": "GET", "Path": "/v8/management/plugins/" + pluginID + "/accounting/runs"}, nil)
	body, err = base64.StdEncoding.DecodeString(str(object(result["result"])["Body"]))
	var data struct {
		Scope string           `json:"scope"`
		Rows  []map[string]any `json:"rows"`
	}
	if err != nil || json.Unmarshal(body, &data) != nil || data.Scope != "run" || len(data.Rows) != 1 || data.Rows[0]["model"] != "private-model-sentinel" {
		t.Fatalf("authenticated runs route did not return the run snapshot: %v", err)
	}
	if !reflect.DeepEqual(before, p.journal.Health()) {
		t.Fatal("usage resource or runs route mutated the journal")
	}
	for _, request := range []map[string]any{
		{"Method": "GET", "Path": "/v8/resource/plugins/" + pluginID + "/accounting/runs"},
		{"Method": "POST", "Path": "/v8/management/plugins/" + pluginID + "/accounting/runs"},
	} {
		result := invokeNative(t, p, "management.handle", request, nil)
		if num(object(result["result"])["StatusCode"]) != 404 {
			t.Fatal("run data was exposed through an unexpected route")
		}
	}
}
