package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReconciliationDoesNotStarveBehindPermanentlyMissingRuns(t *testing.T) {
	p := newTestRuntime(t)
	account := digest("synthetic-account")
	var late usageRecord
	for i := 0; i < 40; i++ {
		start := time.Now().Add(-time.Duration(100-i) * time.Minute)
		r, err := p.journal.Begin(account, fmt.Sprintf("synthetic-cid-%d", i), fmt.Sprintf("synthetic-run-%d", i), "test-model", start)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.journal.Close(r.ID, start.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		late = r
	}
	h := newMockHost(t)
	h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
		if !strings.HasSuffix(req.URL, "GetFilteredUsageEvents") {
			t.Fatal("unexpected endpoint")
		}
		return mockResponse{Status: 200, Body: encodeJSON(map[string]any{"usageEventsDisplay": []any{map[string]any{
			"conversationId": late.Conversation, "model": late.Model, "timestamp": late.Started.UnixMilli(), "usageEventId": "synthetic-late-39", "tokenUsage": map[string]any{"inputTokens": 10, "cacheReadTokens": 90, "outputTokens": 7},
		}}})}, nil
	}
	s := p.newScope(h.call, "fair-reconciliation")
	defer s.close()
	for i := 0; i < 10; i++ {
		p.reconcileUsage(s, "synthetic-token", account, "")
	}
	records := p.journal.Records()
	if records[39].Status != "deferred" || records[39].Usage.PromptTokens != 100 {
		t.Fatal("late record was starved by missing older records")
	}
	for _, r := range records[:39] {
		if r.Usage != nil {
			t.Fatal("missing record acquired another Run's count")
		}
	}
}

func TestInternalToolStepsKeepRunTotalsOutOfContextUsage(t *testing.T) {
	p := newTestRuntime(t)
	account, cid := digest("synthetic-aggregate-account"), "synthetic-aggregate-cid"
	start := time.Now().Add(-10 * time.Second)
	end := start.Add(time.Second)
	r, err := p.journal.Begin(account, cid, "synthetic-aggregate-run", "test-model", start)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.Close(r.ID, end); err != nil {
		t.Fatal(err)
	}
	h := newMockHost(t)
	h.respond = func(_ context.Context, _ mockRequest) (mockResponse, error) {
		return mockResponse{Status: 200, Body: encodeJSON(map[string]any{"usageEventsDisplay": []any{map[string]any{"conversationId": cid, "model": "test-model", "timestamp": start.UnixMilli(), "usageEventId": "synthetic-aggregate-event", "tokenUsage": map[string]any{"inputTokens": 200, "cacheReadTokens": 6000, "outputTokens": 300}}}})}, nil
	}
	s := p.newScope(h.call, "aggregate-accounting")
	defer s.close()
	terminal := p.accountUsage(s, "synthetic-token", account, r.ID, cid, "test-model", start, end, runEvent{stop: "tool_calls", internalCalls: 1})
	if terminal.usage != nil || terminal.accounting["state"] != "context_unknown" {
		t.Fatal("multi-inference Run total became current context")
	}
	record := p.journal.Records()[0]
	if record.InternalToolCalls != 1 || record.Usage == nil || record.Usage.PromptTokens != 6200 {
		t.Fatal("independent Run observation was lost")
	}
}

func TestAccountingConfigurationAndEmbeddedAttribution(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	// Register TempDir cleanup before runtime cleanup: Windows cannot remove
	// the lock file until the owning runtime has closed its file handle.
	path := filepath.Join(t.TempDir(), "private-journal", "usage.json")
	p := newTestRuntime(t)
	for _, raw := range []string{"usage-journal: relative.json", "usage-wait-ms: 500", "usage-wait-ms: 30001", "usage-journal: [invalid]"} {
		if p.configureAccounting([]byte(raw)) == nil {
			t.Fatal("invalid accounting config accepted")
		}
	}
	if err := p.configureAccounting([]byte("usage-journal: " + path + "\nusage-wait-ms: 8000\n")); err != nil {
		t.Fatal(err)
	}
	if p.journal.path == "" {
		t.Fatal("durable config ignored")
	}
	if err := p.acquireUsageRun("synthetic-account", "synthetic-cid"); err != nil {
		t.Fatal(err)
	}
	if errorStatus(p.configureAccounting(nil)) != 409 {
		t.Fatal("reconfiguration raced an active Run")
	}
	p.releaseUsageRun("synthetic-account", "synthetic-cid")
	if !strings.Contains(distributionLicense, "Copyright (c) 2026 magpie-community contributors") || !strings.Contains(distributionNotice, "ad798f3350eb92774cde939c2f2aa452f21b688e") {
		t.Fatal("distributed binary lacks upstream attribution")
	}
}
