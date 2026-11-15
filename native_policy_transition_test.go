package main

import (
	"context"
	"strings"
	"testing"
)

func TestNativeRequiredThenAutoCompletesSameRun(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	in := nativeTestInput()
	in.Chat.ToolChoice = "required"
	in.Tools = toolsOf(in.Chat)
	in.Conversation, _ = conversation(in.Chat, in.Tools)
	next, _, _ := nativePark(t, ns, tr, in, 1)
	next.Chat.ToolChoice = "auto"
	next.Tools = toolsOf(next.Chat)
	events, receipt, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatalf("required -> auto with identical declared tools must resume: %v", err)
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
	tr.push(nativeTestFinal("final text after the required tool"))
	out := nativeReadEvents(t, events)
	receipt.Delivered()
	if len(out) == 0 || out[len(out)-1].err != nil || out[len(out)-1].stop != "stop" || !receipt.Resumed || opens.Load() != 1 {
		t.Fatalf("required -> auto did not finish on the same Run: %+v", out)
	}
}

func TestNativeRequiredAndForcedCanFinishWithAutoOrNone(t *testing.T) {
	for _, first := range []string{"required", "forced"} {
		for _, nextChoice := range []string{"auto", "none"} {
			t.Run(first+"-to-"+nextChoice, func(t *testing.T) {
				ns, tr, opens := nativeTestPool(t)
				in := nativeTestInput()
				if first == "forced" {
					in.Chat.Tools = in.Chat.Tools[:1]
					in.Chat.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
				} else {
					in.Chat.ToolChoice = first
				}
				in.Tools = toolsOf(in.Chat)
				in.Conversation, _ = conversation(in.Chat, in.Tools)
				next, _, _ := nativePark(t, ns, tr, in, 1)
				next.Chat.ToolChoice = nextChoice
				next.Tools = toolsOf(next.Chat)
				events, receipt, err := ns.Start(context.Background(), next)
				if err != nil {
					t.Fatalf("policy-only change rejected: %v", err)
				}
				nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
				tr.push(nativeTestFinal("final text"))
				out := nativeReadEvents(t, events)
				receipt.Delivered()
				if out[len(out)-1].err != nil || out[len(out)-1].stop != "stop" || opens.Load() != 1 {
					t.Fatalf("old policy still prevented final text: %+v", out)
				}
			})
		}
	}
}

func TestNativeNextResponseEnforcesUpdatedRequiredAndParallelPolicy(t *testing.T) {
	for _, scenario := range []string{"required", "single-call"} {
		t.Run(scenario, func(t *testing.T) {
			ns, tr, opens := nativeTestPool(t)
			next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
			if scenario == "required" {
				next.Chat.ToolChoice = "required"
			} else {
				parallel := false
				next.Chat.ParallelToolCalls = &parallel
			}
			next.Tools = toolsOf(next.Chat)
			events, _, err := ns.Start(context.Background(), next)
			if err != nil {
				t.Fatalf("same-catalog policy change rejected: %v", err)
			}
			nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
			if scenario == "required" {
				tr.push(nativeTestFinal("text that violates the new required policy"))
			} else {
				tr.push(nativeTestCall(2, "read"))
				tr.push(nativeTestCall(3, "write"))
				tr.push(nativeTestCount(2))
			}
			out := nativeReadEvents(t, events)
			for _, ev := range out {
				if ev.tool != nil || ev.stop != "" {
					t.Fatal("updated policy was not applied before publishing the next batch")
				}
			}
			if nativeErrorCode(out[len(out)-1].err) != "cursor_native_tool_output_invalid" || opens.Load() != 1 {
				t.Fatalf("updated policy was not enforced on the same Run: %+v", out)
			}
		})
	}
}

func TestNativeResultReservationBindsItsResponsePolicy(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	in := nativeTestInput()
	in.Chat.ToolChoice = "required"
	next, _, _ := nativePark(t, ns, tr, in, 1)
	next.Chat.ToolChoice = "auto"
	events, receipt, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	changed := next
	changed.Chat.ToolChoice = "none"
	changed.Tools = toolsOf(changed.Chat)
	if _, _, err = ns.Start(context.Background(), changed); errorStatus(err) != 409 {
		t.Fatal("concurrent policy change bypassed result reservation")
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
	tr.push(nativeTestFinal("auto policy output"))
	_ = nativeReadEvents(t, events)
	receipt.Delivered()
	if _, _, err = ns.Start(context.Background(), changed); errorStatus(err) != 409 || !strings.Contains(err.Error(), "different output policy") {
		t.Fatalf("cached output was reused under a different policy: %v", err)
	}
	replay, replayReceipt, err := ns.Start(context.Background(), next)
	if err != nil || !replayReceipt.Replay || opens.Load() != 1 {
		t.Fatalf("identical policy replay no longer works: %v", err)
	}
	_ = nativeReadEvents(t, replay)
	replayReceipt.Delivered()
}

func TestNativePolicyChangeCannotExpandBootstrapToolsOrHideSchemaChanges(t *testing.T) {
	for _, scenario := range []string{"expand", "schema-while-none"} {
		t.Run(scenario, func(t *testing.T) {
			ns, tr, opens := nativeTestPool(t)
			in := nativeTestInput()
			in.Chat.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
			in.Tools = toolsOf(in.Chat)
			in.Conversation, _ = conversation(in.Chat, in.Tools)
			next, _, _ := nativePark(t, ns, tr, in, 1)
			next.Chat.ToolChoice = "auto"
			if scenario == "schema-while-none" {
				next.Chat.ToolChoice = "none"
				object(next.Chat.Tools[1]["function"])["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"new": map[string]any{"type": "integer"}}}
			}
			next.Tools = toolsOf(next.Chat)
			_, _, err := ns.Start(context.Background(), next)
			if errorStatus(err) != 409 || nativeErrorCode(err) != "cursor_native_context_changed" || opens.Load() != 1 || len(tr.snapshot()) != 0 {
				t.Fatalf("changed tool capability escaped policy boundary: %v", err)
			}
			if scenario == "expand" && !strings.Contains(err.Error(), "original executable tool set") {
				t.Fatal("native tool-set expansion lacks an explicit new-turn boundary")
			}
		})
	}
}

func TestNativePolicyCanRestoreToolsFromItsOriginalCatalog(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	next.Chat.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
	next.Tools = toolsOf(next.Chat)
	events, receipt, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
	tr.push(nativeTestCall(2, "read"))
	tr.push(nativeTestCount(1))
	batch := nativeReadEvents(t, events)
	receipt.Delivered()
	if batch[len(batch)-1].stop != "tool_calls" {
		t.Fatalf("forced policy did not yield its tool: %+v", batch)
	}
	final := nativeContinuation(t, next, batch)
	final.Chat.ToolChoice = "auto"
	final.Tools = toolsOf(final.Chat)
	events, receipt, err = ns.Start(context.Background(), final)
	if err != nil {
		t.Fatalf("original native catalog could not be restored: %v", err)
	}
	nativeWait(t, func() bool { return len(tr.snapshot()) == 4 })
	tr.push(nativeTestFinal("finished"))
	out := nativeReadEvents(t, events)
	receipt.Delivered()
	if out[len(out)-1].err != nil || opens.Load() != 1 {
		t.Fatal("policy restoration changed Runs or retained the old forced policy")
	}
}
