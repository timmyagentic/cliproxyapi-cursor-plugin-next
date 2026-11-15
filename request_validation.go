package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Validate only controls that this executor would otherwise silently ignore.
// Host routing/streaming/metadata fields remain host-owned. Null and neutral
// forms emitted by compatibility clients do not add a new generation constraint.
func validateRequestControls(chat chatRequest) error {
	for _, key := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "stop", "seed", "frequency_penalty", "presence_penalty", "logit_bias", "logprobs", "top_logprobs", "response_format", "n", "modalities", "audio", "prediction", "web_search_options"} {
		raw, ok := chat.rawFields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return failure(400, "Invalid generation control %q", key)
		}
		neutral := false
		switch key {
		case "n", "top_p", "temperature":
			neutral = value == float64(1)
		case "frequency_penalty", "presence_penalty", "top_logprobs", "max_tokens", "max_completion_tokens":
			neutral = value == float64(0)
		case "logprobs":
			neutral = value == false
		case "stop":
			neutral = value == "" || (array(value) != nil && len(array(value)) == 0)
		case "logit_bias":
			neutral = object(value) != nil && len(object(value)) == 0
		case "response_format":
			v := object(value)
			neutral = len(v) == 1 && str(v["type"]) == "text"
		case "modalities":
			v := array(value)
			neutral = len(v) == 1 && v[0] == "text"
		}
		if !neutral {
			return failure(400, "Cursor does not implement generation control %q", key)
		}
	}
	return nil
}

func prepareTools(chat chatRequest) ([]chatTool, error) {
	if err := validateRequestControls(chat); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, raw := range chat.Tools {
		f := object(raw["function"])
		name := str(f["name"])
		if str(raw["type"]) != "function" || strings.TrimSpace(name) == "" {
			return nil, failure(400, "Cursor requires named function tools after host translation")
		}
		if names[name] {
			return nil, failure(400, "Duplicate function tool name %q", name)
		}
		names[name] = true
		if value, exists := f["strict"]; exists && value != nil {
			if _, ok := value.(bool); !ok {
				return nil, failure(400, "Tool strict must be a boolean")
			}
		}
		if value, exists := f["parameters"]; exists && value != nil && object(value) == nil {
			return nil, failure(400, "Function tool parameters must be a JSON Schema object")
		}
	}
	switch choice := chat.ToolChoice.(type) {
	case nil:
	case string:
		if choice != "auto" && choice != "none" && choice != "required" {
			return nil, failure(400, "Unsupported tool_choice")
		}
		if choice == "required" && len(names) == 0 {
			return nil, failure(400, "tool_choice required needs at least one function tool")
		}
	case map[string]any:
		name := str(object(choice["function"])["name"])
		if str(choice["type"]) != "function" || !names[name] {
			return nil, failure(400, "Forced tool_choice must name a declared function tool")
		}
	default:
		return nil, failure(400, "Invalid tool_choice")
	}
	tools := toolsOf(chat)
	for i := range tools {
		if tools[i].Strict {
			validator, err := compileToolSchema(tools[i])
			if err != nil {
				return nil, err
			}
			tools[i].validator = validator
		}
	}
	return tools, nil
}
