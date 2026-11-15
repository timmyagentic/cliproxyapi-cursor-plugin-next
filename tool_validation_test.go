package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func policyToolCall(name string, args any) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func TestStrictToolSchemaValidatesWithoutRepairOrExternalReads(t *testing.T) {
	var external atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { external.Add(1) }))
	defer server.Close()
	schema := map[string]any{
		"type": "object", "required": []any{"items"}, "additionalProperties": false,
		"$defs": map[string]any{"item": map[string]any{"type": "object", "required": []any{"count", "kind"}, "additionalProperties": false,
			"properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}, "kind": map[string]any{"enum": []any{"safe", "public"}}}}},
		"properties": map[string]any{"items": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"$ref": "#/$defs/item"}}},
	}
	tool := chatTool{Name: "check", Schema: schema, Strict: true}
	validator, err := compileToolSchema(tool)
	if err != nil {
		t.Fatal(err)
	}
	tool.validator = validator
	valid := `{"items":[{"count":2,"kind":"safe"}]}`
	call := policyToolCall("check", valid)
	if err := validateToolOutputBatch("required", nil, []chatTool{tool}, []map[string]any{call}); err != nil {
		t.Fatal(err)
	}
	if object(call["function"])["arguments"] != valid {
		t.Fatal("validator rewrote tool arguments")
	}
	for _, args := range []string{`{}`, `{"items":[]}`, `{"items":[{"count":"2","kind":"safe"}]}`, `{"items":[{"count":0,"kind":"safe"}]}`, `{"items":[{"count":2,"kind":"wrong"}]}`, `{"items":[{"count":2,"kind":"safe","extra":true}]}`, `{"items":[{"count":2,"kind":"safe"}],"extra":true}`, `not-json`} {
		if err := validateToolOutputBatch(nil, nil, []chatTool{tool}, []map[string]any{policyToolCall("check", args)}); errorStatus(err) != 502 {
			t.Fatalf("invalid strict arguments accepted: %s (%v)", args, err)
		}
	}
	tool.Schema = map[string]any{"$ref": server.URL + "/SYNTHETIC_SECRET"}
	tool.validator = nil
	if _, err := compileToolSchema(tool); errorStatus(err) != 400 || strings.Contains(err.Error(), "SYNTHETIC_SECRET") {
		t.Fatalf("external schema was not safely rejected: %v", err)
	}
	if external.Load() != 0 {
		t.Fatal("schema compilation performed external I/O")
	}
}

func TestToolChoiceAndParallelOutputContracts(t *testing.T) {
	noParallel := false
	tools := []chatTool{{Name: "read"}, {Name: "write"}}
	forced := map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
	for _, tc := range []struct {
		name     string
		choice   any
		parallel *bool
		calls    []map[string]any
	}{
		{"required", "required", nil, nil},
		{"forced missing", forced, nil, nil},
		{"forced wrong", forced, nil, []map[string]any{policyToolCall("write", `{}`)}},
		{"forced repeated", forced, nil, []map[string]any{policyToolCall("read", `{}`), policyToolCall("read", `{}`)}},
		{"none", "none", nil, []map[string]any{policyToolCall("read", `{}`)}},
		{"parallel false", nil, &noParallel, []map[string]any{policyToolCall("read", `{}`), policyToolCall("write", `{}`)}},
		{"unknown", nil, nil, []map[string]any{policyToolCall("unknown", `{}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateToolOutputBatch(tc.choice, tc.parallel, tools, tc.calls); errorStatus(err) != 502 {
				t.Fatalf("invalid output accepted: %v", err)
			}
		})
	}
	if err := validateToolOutputBatch(forced, &noParallel, tools, []map[string]any{policyToolCall("read", `{"untouched":true}`)}); err != nil {
		t.Fatal(err)
	}
}

func outputPolicyFrame(id uint64, name, path string) []byte {
	args := protobuf(nil).text(1, "magpie-"+name).text(3, fmt.Sprintf("call_%d", id)).text(4, "magpie").text(5, name).text(9, "magpie")
	args = args.bytes(2, protobuf(nil).text(1, "path").bytes(2, pbValue(path)))
	exec := protobuf(nil).uint(1, id).text(15, fmt.Sprintf("exec-%d", id)).bytes(11, args)
	return wireFrame(wireBytes(2, exec), 0)
}

func TestHostOutputPolicyRejectsWholeBatchBeforeAnyToolEscapes(t *testing.T) {
	noParallel := false
	state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}, parallelToolCalls: &noParallel}
	frames := append(outputPolicyFrame(1, "read", "one"), outputPolicyFrame(2, "read", "two")...)
	frames = append(frames, wireFrame(wireBytes(1, wireBytes(27, testUint(1, 2))), 0)...)
	var published []runEvent
	err := state.decode(bytes.NewReader(frames), func([]byte) error { return nil }, func(ev runEvent) bool { published = append(published, ev); return true })
	if errorStatus(err) != 502 || len(published) != 0 {
		t.Fatalf("partial forbidden batch escaped: err=%v events=%+v", err, published)
	}
	state = runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}, toolChoice: "required"}
	published = nil
	err = state.decode(bytes.NewReader(syntheticReply()), func([]byte) error { return nil }, func(ev runEvent) bool { published = append(published, ev); return true })
	if errorStatus(err) != 502 {
		t.Fatalf("required missing call accepted: %v", err)
	}
	for _, ev := range published {
		if ev.tool != nil || ev.stop != "" {
			t.Fatal("a success terminal was emitted for invalid output")
		}
	}
}

func TestRequestControlsAndStrictPreparation(t *testing.T) {
	for _, control := range []string{`"temperature":0.1`, `"top_p":0.5`, `"max_tokens":100`, `"stop":["END"]`, `"response_format":{"type":"json_schema"}`, `"n":2`, `"audio":{"voice":"test"}`} {
		var chat chatRequest
		if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"hi"}],`+control+`}`), &chat); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareTools(chat); errorStatus(err) != 400 {
			t.Fatalf("unsupported control silently accepted: %s (%v)", control, err)
		}
	}
	var chat chatRequest
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":null,"top_p":1,"max_tokens":0,"response_format":{"type":"text"},"n":1,"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"read","strict":true,"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}}]}`), &chat); err != nil {
		t.Fatal(err)
	}
	tools, err := prepareTools(chat)
	if err != nil || len(tools) != 1 || tools[0].validator == nil {
		t.Fatalf("valid Codex-style controls rejected: %v", err)
	}
	before := encodeJSON(chat.Messages)
	if err := validateToolArguments(tools[0], map[string]any{"path": "ok"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, encodeJSON(chat.Messages)) {
		t.Fatal("request preparation changed history")
	}
}
