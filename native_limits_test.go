package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNativeLimitsConfigurationPreservesDefaultsAndBounds(t *testing.T) {
	defaults, err := parseNativeConfig(nil)
	if err != nil || defaults.ToolResultTimeoutSeconds != 120 || defaults.RunLifetimeSeconds != 900 {
		t.Fatalf("native defaults changed: %+v %v", defaults, err)
	}
	for _, tc := range []struct {
		name, yaml string
		wantError  bool
	}{
		{"minimum", "native-tool-result-timeout-seconds: 15\nnative-run-lifetime-seconds: 60\n", false},
		{"maximum", "native-tool-result-timeout-seconds: 900\nnative-run-lifetime-seconds: 3600\n", false},
		{"zero tool timeout", "native-tool-result-timeout-seconds: 0\n", true},
		{"negative tool timeout", "native-tool-result-timeout-seconds: -1\n", true},
		{"short tool timeout", "native-tool-result-timeout-seconds: 14\n", true},
		{"long tool timeout", "native-tool-result-timeout-seconds: 901\n", true},
		{"short Run", "native-run-lifetime-seconds: 59\n", true},
		{"long Run", "native-run-lifetime-seconds: 3601\n", true},
		{"tool exceeds Run", "native-tool-result-timeout-seconds: 120\nnative-run-lifetime-seconds: 60\n", true},
		{"tool equals Run", "native-tool-result-timeout-seconds: 120\nnative-run-lifetime-seconds: 120\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseNativeConfig([]byte(tc.yaml))
			if (err != nil) != tc.wantError || (err != nil && errorStatus(err) != 400) {
				t.Fatalf("unexpected config result: %v", err)
			}
		})
	}
}

func TestNativeConfiguredLimitsApplyWithoutPartialFailedUpdates(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	config := []byte("transport: native\nnative-proxy-url: direct\nnative-tool-result-timeout-seconds: 300\nnative-run-lifetime-seconds: 1800\n")
	if err := p.configureAccounting(config); err != nil {
		t.Fatal(err)
	}
	owner, pool, cfg := p.nativeSessions, p.nativePool, p.nativeConfig
	if owner.limits.PendingTTL != 5*time.Minute || owner.limits.Lifetime != 30*time.Minute {
		t.Fatal("configured native limits did not reach new Runs")
	}
	if err := p.configureAccounting(config); err != nil || p.nativeSessions != owner || p.nativePool != pool {
		t.Fatal("identical native configuration replaced the owner store")
	}
	if err := p.configureAccounting([]byte("native-tool-result-timeout-seconds: 0\n")); errorStatus(err) != 400 {
		t.Fatal("invalid timeout was accepted")
	}
	if p.nativeSessions != owner || p.nativePool != pool || p.nativeConfig != cfg {
		t.Fatal("failed native limit parsing partially replaced live configuration")
	}
}

func TestNativePublishedToolDeadlineIncludesRunProcessingReserve(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	ns.limits.Lifetime = 2 * time.Second
	ns.limits.PendingTTL = 3 * time.Second
	_, receipt, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	deadline := receipt.PendingDeadline()
	want := receipt.RunDeadline.Add(-nativeResumeBuffer(ns.limits.Lifetime))
	if !deadline.Equal(want) || !deadline.Before(receipt.RunDeadline) {
		t.Fatalf("advertised tool deadline exceeds resumable lifetime: got %s want %s", deadline, want)
	}
	info := nativeDeadlineMetadata(receipt)
	if info["tool_result_deadline"] != deadline.UTC().Format(time.RFC3339Nano) || info["run_expires_at"] != receipt.RunDeadline.UTC().Format(time.RFC3339Nano) {
		t.Fatal("deadline observability did not report the actual admission bounds")
	}
	if receipt.ToolResultTimeoutSeconds != 3 {
		t.Fatal("configured timeout missing from receipt")
	}
}

func TestNativeNearDeadlineDoesNotPublishUnusableTools(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	ns.limits.Lifetime = 2 * time.Second
	events, receipt, err := ns.Start(context.Background(), nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	ns.mu.Lock()
	owner := ns.entries[receipt.RunID]
	ns.mu.Unlock()
	owner.mu.Lock()
	owner.expires = time.Now().Add(50 * time.Millisecond)
	owner.mu.Unlock()
	tr.push(nativeTestCall(1, "read"))
	tr.push(nativeTestCount(1))
	out := nativeReadEvents(t, events)
	for _, ev := range out {
		if ev.tool != nil || ev.stop == "tool_calls" {
			t.Fatal("caller received a tool batch after its effective deadline")
		}
	}
	last := out[len(out)-1].err
	if nativeErrorCode(last) != "cursor_native_deadline" || errorStatus(last) != 410 || !strings.Contains(last.Error(), "no new tool batch was delivered") {
		t.Fatalf("near-deadline rejection lacks its recovery reason: %v", last)
	}
}

func TestNativeExpiryDuringOpenPreservesPermanentDeadline(t *testing.T) {
	tr := newNativeFakeTransport()
	ns := newNativeSessions(context.Background(), func(ctx context.Context, _, _, _ string, _ []byte) (cursorRunTransport, error) {
		<-ctx.Done()
		return tr, nil // Simulate an Open completing just after cancellation won.
	})
	defer ns.Close()
	ns.limits.Lifetime = 25 * time.Millisecond
	_, _, err := ns.Start(context.Background(), nativeTestInput())
	if errorStatus(err) != 410 || nativeErrorCode(err) != "cursor_native_deadline" || !strings.Contains(err.Error(), "run_lifetime") {
		t.Fatalf("late Open replaced the original permanent deadline: %v", err)
	}
	if tr.closed.Load() != 1 {
		t.Fatal("transport opened after its Run deadline was not closed")
	}
}
