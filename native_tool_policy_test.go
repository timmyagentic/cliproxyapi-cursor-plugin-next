package main

import (
	"context"
	"testing"
)

func TestNativeToolPolicyFailureNeverPublishesOrParksInvalidBatch(t *testing.T) {
	for _, scenario := range []string{"required", "parallel", "forced", "strict"} {
		t.Run(scenario, func(t *testing.T) {
			ns, tr, _ := nativeTestPool(t)
			in := nativeTestInput()
			switch scenario {
			case "required":
				in.Chat.ToolChoice = "required"
			case "parallel":
				parallel := false
				in.Chat.ParallelToolCalls = &parallel
			case "forced":
				in.Chat.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
			case "strict":
				function := object(in.Chat.Tools[0]["function"])
				function["strict"] = true
				function["parameters"] = map[string]any{"type": "object", "required": []any{"path"}, "properties": map[string]any{"path": map[string]any{"type": "integer"}}}
			}
			var err error
			in.Tools, err = prepareTools(in.Chat)
			if err != nil {
				t.Fatal(err)
			}
			events, receipt, err := ns.Start(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "required" {
				tr.push(nativeTestFinal("text without required tool"))
			} else {
				tr.push(nativeTestCall(1, "read"))
				count := 1
				if scenario == "parallel" || scenario == "forced" {
					tr.push(nativeTestCall(2, "read"))
					count = 2
				}
				tr.push(nativeTestCount(count))
			}
			out := nativeReadEvents(t, events)
			failed := false
			for _, ev := range out {
				if ev.tool != nil || ev.stop != "" {
					t.Fatalf("invalid batch escaped as successful output: %+v", ev)
				}
				failed = failed || errorStatus(ev.err) == 502 && ev.err != nil
			}
			if !failed {
				t.Fatal("tool contract failure was not visible")
			}
			ns.mu.Lock()
			owner := ns.entries[receipt.RunID]
			handles := len(ns.handles)
			ns.mu.Unlock()
			if handles != 0 {
				t.Fatal("invalid tools acquired usable continuation handles")
			}
			if owner != nil {
				owner.mu.Lock()
				state := owner.state
				owner.mu.Unlock()
				if state != "failed" && state != "finishing" {
					t.Fatalf("invalid owner was retained as %s", state)
				}
			}
		})
	}
}
