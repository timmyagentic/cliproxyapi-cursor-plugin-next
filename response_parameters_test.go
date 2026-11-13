package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoverResponseServiceTierPreservesHostPolicy(t *testing.T) {
	tests := []struct {
		name, payload, original, source, want string
		enabled, fail                         bool
	}{
		{"disabled", `{}`, `{"input":"test","service_tier":"priority"}`, "openai-response", "", false, false},
		{"fast", `{}`, `{"input":"test","service_tier":"fast"}`, "openai-response", "fast", true, false},
		{"default", `{}`, `{"input":"test","service_tier":"default"}`, "openai-response", "default", true, false},
		{"source detection", `{}`, `{"input":"test","service_tier":"priority"}`, "openai", "priority", true, false},
		{"effective default", `{"service_tier":"default"}`, `{"input":"test","service_tier":"priority"}`, "openai-response", "default", true, false},
		{"effective null", `{"service_tier":null}`, `{"input":"test","service_tier":"priority"}`, "openai-response", "", true, false},
		{"effective empty", `{"service_tier":""}`, `{"input":"test","service_tier":"priority"}`, "openai-response", "", true, false},
		{"chat untouched", `{}`, `{"messages":[],"service_tier":"priority"}`, "openai", "", true, false},
		{"absent original", `{}`, ``, "openai-response", "", true, false},
		{"absent tier", `{}`, `{"input":"test"}`, "openai-response", "", true, false},
		{"null tier", `{}`, `{"input":"test","service_tier":null}`, "openai-response", "", true, false},
		{"invalid type", `{}`, `{"input":"test","service_tier":12}`, "openai-response", "", true, true},
		{"unsupported tier", `{}`, `{"input":"test","service_tier":"private-value-canary"}`, "openai-response", "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chat chatRequest
			if err := json.Unmarshal([]byte(tt.payload), &chat); err != nil {
				t.Fatal(err)
			}
			err := recoverResponseServiceTier(rpcRequest{Payload: []byte(tt.payload), OriginalRequest: []byte(tt.original), SourceFormat: tt.source}, &chat, tt.enabled)
			if (err != nil) != tt.fail || chat.ServiceTier != tt.want {
				t.Fatalf("tier=%q err=%v; want tier=%q failure=%v", chat.ServiceTier, err, tt.want, tt.fail)
			}
			if err != nil && strings.Contains(err.Error(), "private-value-canary") {
				t.Fatal("error echoed an unsupported request value")
			}
		})
	}
}

func TestResponseTierRecoveryIsExplicitConfiguration(t *testing.T) {
	p := newRuntime()
	defer p.stop()
	if p.recoverResponseTier {
		t.Fatal("recovery enabled by default")
	}
	if err := p.configureAccounting([]byte("recover-response-service-tier: true\n")); err != nil {
		t.Fatal(err)
	}
	if !p.recoverResponseTier {
		t.Fatal("explicit recovery opt-in was not applied")
	}
	if err := p.configureAccounting([]byte("recover-response-service-tier: false\n")); err != nil {
		t.Fatal(err)
	}
	if p.recoverResponseTier {
		t.Fatal("recovery could not be disabled")
	}
}
