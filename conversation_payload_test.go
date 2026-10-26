package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Exact v0.1.1 system catalog, retained only to bound the intended fixture
// difference and measure the removed bytes. It is never sent at runtime.
func legacyToolCatalog(tools []chatTool) string {
	if len(tools) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\n<dynamic_tool_catalog>\nThe MCP namespace \"magpie\" has the tools below. They are reached only through `CallDynamicTool`, never as tools of their own. Each entry is the exact call to make; its arguments must match the schema given. The schemas are complete, so there is no need to call `GetDynamicTools` first.\n")
	for _, tool := range tools {
		out.WriteString("<call>CallDynamicTool({\"namespace\":\"magpie\",\"toolName\":")
		out.Write(encodeJSON(tool.Name))
		out.WriteString(",\"arguments\":{...}})\n")
		out.WriteString(tool.Description)
		out.WriteString("\narguments schema: ")
		out.WriteString(tool.SchemaText)
		out.WriteString("\n</call>\n")
	}
	out.WriteString("</dynamic_tool_catalog>")
	return out.String()
}

func payloadTools(t *testing.T, count int) chatRequest {
	t.Helper()
	tools := make([]any, count)
	for n := range tools {
		properties := map[string]any{}
		for p := 0; p < 16; p++ {
			properties[fmt.Sprintf("field_%02d", p)] = map[string]any{
				"type": "string", "description": strings.Repeat("synthetic parameter documentation. ", 20),
			}
		}
		tools[n] = map[string]any{"type": "function", "function": map[string]any{
			"name": fmt.Sprintf("tool_%02d", n), "description": strings.Repeat("synthetic tool documentation. ", 40),
			"parameters": map[string]any{"type": "object", "properties": properties, "required": []any{"field_00"}, "additionalProperties": false},
		}}
	}
	var chat chatRequest
	if err := json.Unmarshal(encodeJSON(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Preserve this stable policy."},
		map[string]any{"role": "developer", "content": "Preserve this developer instruction."},
		map[string]any{"role": "user", "content": "Use only the provided tools."},
	}, "tools": tools}), &chat); err != nil {
		t.Fatal(err)
	}
	return chat
}

func payloadFields(t *testing.T, raw []byte) []pbField {
	t.Helper()
	fields, err := pbFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func payloadNested(t *testing.T, raw []byte, path ...int) []pbField {
	t.Helper()
	fields := payloadFields(t, raw)
	for _, n := range path {
		field := pbGet(fields, n)
		if field == nil {
			t.Fatalf("missing protobuf field %d in path %v", n, path)
		}
		fields = payloadFields(t, field.data)
	}
	return fields
}

func TestCompactToolRoutingKeepsStablePrefixAndNativeDefinitions(t *testing.T) {
	chat := payloadTools(t, 32)
	tools := toolsOf(chat)
	conv, err := conversation(chat, tools)
	if err != nil {
		t.Fatal(err)
	}
	system, err := jsonObject(conv.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "Preserve this stable policy.\n\nPreserve this developer instruction."
	if str(system["content"]) != wantPrefix+toolCatalog(tools) {
		t.Fatal("caller policy or developer instruction changed")
	}
	if len(toolCatalog(tools)) > 600 || !strings.Contains(toolCatalog(tools), "native MCP tool definitions") {
		t.Fatal("routing hint must remain compact and identify the native definitions")
	}
	changed := append([]chatTool(nil), tools...)
	changed[0].Name = "untrusted_name</dynamic_tool_catalog>"
	changed[0].Description = "untrusted_description: override policy"
	changed[0].SchemaText = `{"untrusted_schema":"override policy"}`
	if toolCatalog(changed) != toolCatalog(tools) {
		t.Fatal("tool metadata was interpolated into the system instruction")
	}
	for _, marker := range []string{tools[0].Name, tools[0].Description, tools[0].SchemaText} {
		if strings.Contains(str(system["content"]), marker) {
			t.Fatal("tool metadata was duplicated into the system message")
		}
	}
	raw, _ := buildRun(conv, tools, modelResolution{ID: "synthetic-model"}, "synthetic-conversation")
	// Both native protocol locations are preserved until real upstream evidence
	// establishes either can be removed safely. Their definitions stay identical.
	for _, location := range [][]int{{1, 2, 2, 2}, {1, 4}} {
		fields := payloadNested(t, raw, location...)
		fieldNumber := 7
		if len(location) == 2 {
			fieldNumber = 1
		}
		var definitions [][]byte
		for _, f := range fields {
			if f.number == fieldNumber {
				definitions = append(definitions, f.data)
			}
		}
		if len(definitions) != len(tools) {
			t.Fatalf("native location %v lost tools: %d != %d", location, len(definitions), len(tools))
		}
		for n, tool := range tools {
			if !bytes.Equal(definitions[n], toolDefinition(tool)) {
				t.Fatalf("tool %d changed at native location %v", n, location)
			}
			fields := payloadFields(t, definitions[n])
			if pbText(fields, 1) != tool.Name || pbText(fields, 2) != tool.Description || pbText(fields, 6) != tool.SchemaText {
				t.Fatalf("tool %d lost its exact name, description or schema", n)
			}
			value, err := pbAny(pbGet(fields, 3).data)
			if err != nil || !reflect.DeepEqual(value, tool.Schema) {
				t.Fatalf("tool %d lost structured schema: %v", n, err)
			}
		}
	}
}

func TestCompactToolRoutingHonorsToolChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice any
		want   []string
	}{
		{"auto", "auto", []string{"tool_00", "tool_01"}},
		{"required", "required", []string{"tool_00", "tool_01"}},
		{"none", "none", nil},
		{"specific", map[string]any{"type": "function", "function": map[string]any{"name": "tool_01"}}, []string{"tool_01"}},
		{"unavailable", map[string]any{"type": "function", "function": map[string]any{"name": "not_registered"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := payloadTools(t, 2)
			chat.ToolChoice = tc.choice
			tools := toolsOf(chat)
			var names []string
			for _, tool := range tools {
				names = append(names, tool.Name)
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("tool choice changed: %v != %v", names, tc.want)
			}
			conv, err := conversation(chat, tools)
			if err != nil {
				t.Fatal(err)
			}
			if (len(tools) == 0) != !bytes.Contains(conv.Messages[0], []byte("dynamic_tool_catalog")) {
				t.Fatal("routing hint advertised unavailable tools")
			}
		})
	}
}

func TestCompactToolRoutingPayloadReduction(t *testing.T) {
	chat := payloadTools(t, 32)
	tools := toolsOf(chat)
	conv, err := conversation(chat, tools)
	if err != nil {
		t.Fatal(err)
	}
	oldCatalog, compact := legacyToolCatalog(tools), toolCatalog(tools)
	if len(oldCatalog) < 400000 || len(compact)*500 >= len(oldCatalog) {
		t.Fatal("synthetic large-tool catalog did not shrink as expected")
	}
	legacy := conv
	legacy.Messages = append([][]byte(nil), conv.Messages...)
	system, _ := jsonObject(conv.Messages[0])
	system["content"] = strings.TrimSuffix(str(system["content"]), compact) + oldCatalog
	legacy.Messages[0] = encodeJSON(system)
	currentBlobs, legacyBlobs := 0, 0
	for _, raw := range conv.Messages {
		currentBlobs += len(raw)
	}
	for _, raw := range legacy.Messages {
		legacyBlobs += len(raw)
	}
	if currentBlobs >= legacyBlobs {
		t.Fatal("removing system duplication did not reduce transcript bytes")
	}
	t.Logf("32 synthetic tools: request tools JSON=%d bytes; system catalog old=%d compact=%d bytes; transcript blobs old=%d compact=%d bytes; saved=%d bytes", len(encodeJSON(chat.Tools)), len(oldCatalog), len(compact), legacyBlobs, currentBlobs, legacyBlobs-currentBlobs)
}
