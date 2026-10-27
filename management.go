package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

//go:embed assets/login.html
var loginHTML string

var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

func (p *pluginRuntime) registerManagement(req rpcRequest) any {
	base := strings.TrimRight(req.BasePath, "/")
	if base == "" {
		base = "/v0/management"
	}
	resource := strings.TrimRight(req.ResourceBasePath, "/")
	if resource == "" {
		resource = "/v0/resource/plugins/" + pluginID
	}
	p.mu.Lock()
	p.managementBase, p.resourceBase = base, resource
	p.mu.Unlock()
	return map[string]any{
		"routes": []any{
			map[string]any{"Method": "POST", "Path": base + "/plugins/" + pluginID + "/api-key"},
			map[string]any{"Method": "GET", "Path": base + "/plugins/" + pluginID + "/accounting"},
		},
		"resources": []any{map[string]any{"Path": resource + "/login", "Menu": "Cursor", "Description": "Cursor API Key 登录"}},
	}
}

func managementResponse(status int, contentType string, body []byte) any {
	return map[string]any{"StatusCode": status, "Headers": http.Header{
		"Content-Type": {contentType}, "Cache-Control": {"no-store"},
		"X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"},
	}, "Body": body}
}

func (p *pluginRuntime) handleManagement(s *requestScope, req rpcRequest) (any, error) {
	p.mu.Lock()
	base, resource := p.managementBase, p.resourceBase
	p.mu.Unlock()
	if base == "" || resource == "" {
		return nil, failure(503, "Cursor management routes are not registered")
	}
	endpoint := base + "/plugins/" + pluginID + "/api-key"
	if req.Method == http.MethodGet && req.Path == base+"/plugins/"+pluginID+"/accounting" {
		// CPA authenticates this route. The bounded feed contains opaque Run
		// keys and measurements, never credentials or conversation contents.
		after, limit := uint64(0), maxAccountingPage
		if values, ok := req.Query["after_sequence"]; ok {
			if len(values) != 1 {
				return managementResponse(400, "application/json", encodeJSON(map[string]any{"error": "invalid accounting cursor"})), nil
			}
			var err error
			after, err = strconv.ParseUint(values[0], 10, 53)
			if err != nil {
				return managementResponse(400, "application/json", encodeJSON(map[string]any{"error": "invalid accounting cursor"})), nil
			}
		}
		if values, ok := req.Query["limit"]; ok {
			if len(values) != 1 {
				return managementResponse(400, "application/json", encodeJSON(map[string]any{"error": "invalid accounting limit"})), nil
			}
			var err error
			limit, err = strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > maxAccountingPage {
				return managementResponse(400, "application/json", encodeJSON(map[string]any{"error": "invalid accounting limit"})), nil
			}
		}
		p.mu.Lock()
		journal := p.journal
		active := len(p.usageActive)
		p.mu.Unlock()
		page, err := journal.AccountingChanges(after, limit)
		if err != nil {
			return managementResponse(503, "application/json", encodeJSON(map[string]any{"error": "accounting page unavailable", "journal": journal.Health()})), nil
		}
		out, _ := jsonObject(encodeJSON(page))
		out["journal"], out["active_runs"], out["version"] = journal.Health(), active, version
		return managementResponse(200, "application/json", encodeJSON(out)), nil
	}
	if req.Method == http.MethodGet && req.Path == resource+"/login" {
		var body bytes.Buffer
		if err := loginTemplate.Execute(&body, map[string]string{"Endpoint": endpoint, "Logo": pluginLogo}); err != nil {
			return nil, err
		}
		return managementResponse(200, "text/html; charset=utf-8", body.Bytes()), nil
	}
	if req.Method != http.MethodPost || req.Path != endpoint {
		return managementResponse(404, "application/json", []byte(`{"error":"Not found"}`)), nil
	}
	// CPA authenticates this management route before dispatching it. The public
	// resource above only serves the form and never reads or saves credentials.
	status, body := p.importAPIKey(s, req.Body)
	return managementResponse(status, "application/json", encodeJSON(body)), nil
}

func (p *pluginRuntime) importAPIKey(s *requestScope, body []byte) (int, map[string]any) {
	var input struct {
		Key   string `json:"api_key"`
		Label string `json:"label"`
	}
	if len(body) > 16384 || json.Unmarshal(body, &input) != nil {
		return 400, map[string]any{"error": "Invalid API key request"}
	}
	input.Key = strings.TrimSpace(input.Key)
	input.Label = strings.TrimSpace(input.Label)
	if input.Key == "" || strings.ContainsAny(input.Key, "\r\n\t ") {
		return 400, map[string]any{"error": "Enter a Cursor API key without whitespace"}
	}
	auth := credential{Type: "api", Key: input.Key}
	record := authRecord(auth, "", input.Label, nil)
	storage := encodeJSON(record["Metadata"])
	if _, err := p.token(s, rpcRequest{StorageJSON: storage}); err != nil {
		// Do not reflect upstream responses, which may contain credentials.
		status := errorStatus(err)
		if status == 401 || status == 403 {
			return status, map[string]any{"error": "Cursor rejected this API key"}
		}
		return 502, map[string]any{"error": "Could not validate the API key with Cursor; check CPA proxy and network settings"}
	}
	if err := s.call("host.auth.save", map[string]any{"name": record["FileName"], "json": json.RawMessage(storage)}, nil); err != nil {
		return 502, map[string]any{"error": "CPA could not save the Cursor credential"}
	}
	return 200, map[string]any{"status": "ok", "name": record["FileName"]}
}
