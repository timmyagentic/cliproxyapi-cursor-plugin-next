package main

import (
	"bytes"
	"encoding/json"
)

// This opt-in repairs a known host translation loss. It cannot distinguish a
// dropped field from an intentional host filter, so it is disabled by default.
func (p *pluginRuntime) recoverResponseServiceTier(req rpcRequest, chat *chatRequest) error {
	p.mu.Lock()
	enabled := p.recoverResponseTier
	p.mu.Unlock()
	return recoverResponseServiceTier(req, chat, enabled)
}

func recoverResponseServiceTier(req rpcRequest, chat *chatRequest, enabled bool) error {
	if !enabled || nativeSourceFormat(req) != "openai-response" {
		return nil
	}
	// Presence matters: null and an empty string can be the host's final policy.
	if _, exists := chat.rawFields["service_tier"]; exists {
		return nil
	}
	if len(req.OriginalRequest) == 0 {
		return nil
	}
	if len(req.OriginalRequest) > maxWireBytes {
		return failure(413, "Original Responses request exceeds size limit")
	}
	var original struct {
		Tier json.RawMessage `json:"service_tier"`
	}
	if json.Unmarshal(req.OriginalRequest, &original) != nil {
		return failure(400, "Invalid original Responses request")
	}
	if len(original.Tier) == 0 || bytes.Equal(bytes.TrimSpace(original.Tier), []byte("null")) {
		return nil
	}
	var tier string
	if json.Unmarshal(original.Tier, &tier) != nil {
		return failure(400, "Responses service_tier must be a string or null")
	}
	switch tier {
	case "auto", "default", "fast", "priority":
		chat.ServiceTier = tier
		return nil
	default:
		return failure(400, "Cursor supports service_tier auto, default, fast, or priority")
	}
}
