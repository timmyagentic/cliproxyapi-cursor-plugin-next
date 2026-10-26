package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func revisionUsage(event string, stamp time.Time, input int64) map[string]any {
	return map[string]any{"cursor_usage_event_id": digest(event), "cursor_usage_timestamp": stamp.UnixMilli(), "prompt_tokens": input, "completion_tokens": int64(7), "total_tokens": input + 7, "prompt_tokens_details": map[string]any{"cached_tokens": int64(0), "cache_write_tokens": int64(0)}}
}

func TestTurnEndedAggregateIsAlsoWithheldFromContext(t *testing.T) {
	p := newTestRuntime(t)
	account, cid := digest("synthetic-native-account"), "synthetic-native-cid"
	start := time.Now().Add(-time.Minute)
	r, err := p.journal.Begin(account, cid, "synthetic-native-run", "grok-4.7", start)
	if err != nil {
		t.Fatal(err)
	}
	end := start.Add(time.Second)
	if err = p.journal.Close(r.ID, end); err != nil {
		t.Fatal(err)
	}
	host := newMockHost(t)
	s := p.newScope(host.call, "synthetic-native-aggregate")
	defer s.close()
	terminal := p.accountUsage(s, "synthetic", account, r.ID, cid, "grok-4.7", start, end, runEvent{stop: "stop", usage: revisionUsage("unused", start, 6200), internalCalls: 2, internalSteps: 1})
	if terminal.usage != nil || terminal.accounting["state"] != "context_unknown" {
		t.Fatal("native cumulative Run total leaked as current context")
	}
	records := p.journal.Records()
	if len(records) != 1 || records[0].Usage == nil || records[0].Usage.PromptTokens != 6200 || records[0].InternalSteps != 1 {
		t.Fatal("native Run observation or completed internal batch evidence lost")
	}
}

func TestDistinctClaimMakesOverlappingWindowAttributable(t *testing.T) {
	p := newTestRuntime(t)
	account, cid := digest("synthetic-overlap-account"), "synthetic-overlap-cid"
	start := time.Now().Add(-time.Minute)
	old, err := p.journal.Begin(account, cid, "synthetic-old", "grok-4.7", start)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.Close(old.ID, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	candidate := revisionUsage("synthetic-current-event", start.Add(time.Second), 200)
	if p.unambiguousUsage(account, cid, "synthetic-current", candidate) {
		t.Fatal("unresolved overlap became exact")
	}
	if err = p.journal.Complete(old.ID, revisionUsage("synthetic-old-event", start, 100), "dashboard"); err != nil {
		t.Fatal(err)
	}
	if !p.unambiguousUsage(account, cid, "synthetic-current", candidate) {
		t.Fatal("distinct already-owned event did not disambiguate overlap")
	}
	candidate["cursor_usage_event_id"] = digest("synthetic-old-event")
	if p.unambiguousUsage(account, cid, "synthetic-current", candidate) {
		t.Fatal("claimed event was reused")
	}
}

func TestSettledRecordIsRevisitedAndRevisedWithoutDoubleCharge(t *testing.T) {
	p := newTestRuntime(t)
	p.usagePolicy.Wait = time.Second
	p.usagePolicy.Ask = time.Second
	p.usagePolicy.Stable = time.Nanosecond
	p.usagePolicy.Samples = 3
	p.usagePolicy.Interval = time.Nanosecond
	account, cid := digest("synthetic-revision-account"), "synthetic-revision-cid"
	start := time.Now().Add(-time.Minute)
	record, err := p.journal.Begin(account, cid, "synthetic-revision-run", "grok-4.7", start)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.journal.Close(record.ID, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Dashboard hashes its raw event ID with this namespace.
	first := revisionUsage("cursor-dashboard-event\x00synthetic-revision-event", start, 100)
	host := newMockHost(t)
	var polls atomic.Int32
	host.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
		if !strings.HasSuffix(req.URL, "GetFilteredUsageEvents") {
			t.Fatal("unexpected callback")
		}
		polls.Add(1)
		return mockResponse{Status: 200, Body: encodeJSON(map[string]any{"usageEventsDisplay": []any{map[string]any{"conversationId": cid, "model": "grok-4.7", "timestamp": start.UnixMilli(), "usageEventId": "synthetic-revision-event", "tokenUsage": map[string]any{"inputTokens": 250, "outputTokens": 7}}}})}, nil
	}
	// Read the event identity from the same parser used in production; the
	// namespace is deliberately not asserted as an external API contract.
	response, _ := host.respond(context.Background(), mockRequest{URL: "GetFilteredUsageEvents"})
	parsed, err := jsonObject(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := fetchUsagePages(context.Background(), usageWindow{cid, "grok-4.7", start, start.Add(time.Second)}, func(context.Context, map[string]any) (map[string]any, error) { return parsed, nil }, p.usagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	first["cursor_usage_event_id"] = usage["cursor_usage_event_id"]
	if err = p.journal.Complete(record.ID, first, "dashboard"); err != nil {
		t.Fatal(err)
	}
	polls.Store(0)
	s := p.newScope(host.call, "synthetic-revision")
	defer s.close()
	p.reconcileUsage(s, "synthetic", account, "")
	records := p.journal.Records()
	if polls.Load() < 3 || len(records) != 1 || records[0].Usage.PromptTokens != 250 || records[0].Revision < 2 || !records[0].Stable {
		t.Fatalf("settled record was frozen or revision accepted without stable sampling: polls=%d records=%+v", polls.Load(), records)
	}
	if records[0].FirstUsage == nil || records[0].FirstUsage.PromptTokens != 100 {
		t.Fatal("first observation lost")
	}
}

func TestAccountingStorageFailureDoesNotFailToolReply(t *testing.T) {
	p := newTestRuntime(t)
	journalPath := filepath.Join(t.TempDir(), "private", "usage.json")
	// A directory at the file path is unsafe on every supported OS.
	if err := os.MkdirAll(journalPath, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := newUsageJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.journal.Shutdown()
	p.journal = journal
	host := newMockHost(t)
	fixture := &accountingFixture{runs: map[string]*accountingFixtureRun{}, now: time.Now}
	host.respond = fixture.respond
	body := executeAccountingRound(t, p, host, false, []map[string]any{{"role": "user", "content": "Synthetic storage failure continuation test"}}, accountingTools())
	choice := object(array(body["choices"])[0])
	if choice["finish_reason"] != "tool_calls" || len(array(object(choice["message"])["tool_calls"])) != 2 {
		t.Fatal("journal failure changed a successful model tool reply")
	}
	accounting := object(body["cursor_usage"])
	if body["usage"] != nil || accounting["state"] != "unknown" || object(accounting["journal"])["state"] != "degraded" {
		t.Fatal("storage failure was not visible as degraded/unknown")
	}
	host.assertClosed(t)
}

func TestAccountingHealthRouteContainsNoIdentities(t *testing.T) {
	p := newTestRuntime(t)
	p.registerManagement(rpcRequest{BasePath: "/v8/management", ResourceBasePath: "/v8/resource/plugins/" + pluginID})
	if _, err := p.journal.Begin("synthetic-account-secret", "synthetic-private-cid", "synthetic-private-run", "grok-4.7", time.Now()); err != nil {
		t.Fatal(err)
	}
	result := invokeNative(t, p, "management.handle", map[string]any{"Method": "GET", "Path": "/v8/management/plugins/" + pluginID + "/accounting"}, nil)
	body, err := base64.StdEncoding.DecodeString(str(object(result["result"])["Body"]))
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonObject(body)
	if err != nil || object(value["journal"])["state"] != "memory" || num(object(value["journal"])["active_records"]) != 1 {
		t.Fatal("health response is unavailable")
	}
	for _, forbidden := range []string{"synthetic-account-secret", "synthetic-private-cid", "synthetic-private-run", "account_hash", "conversation_id"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatal("health endpoint leaked record identity")
		}
	}
}

func TestNativeUsageSurvivesIndependentJournalFailure(t *testing.T) {
	p := newTestRuntime(t)
	p.journal.blocked = true
	p.journal.storageReason = "corrupt_checkpoint"
	p.journal.path = filepath.Join(t.TempDir(), "synthetic-blocked.json")
	account, cid := digest("synthetic-known-native"), "synthetic-known-native-cid"
	start := time.Now().Add(-time.Second)
	record, err := p.journal.Begin(account, cid, "synthetic-native", "grok-4.7", start)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	_ = p.journal.Close(record.ID, end)
	s := p.newScope(nil, "")
	defer s.close()
	terminal := p.accountUsage(s, "synthetic", account, record.ID, cid, "grok-4.7", start, end, runEvent{stop: "stop", usage: revisionUsage("synthetic-native", start, 1234)})
	input, present := usageInteger(terminal.usage["prompt_tokens"])
	if !present || input != 1234 || terminal.accounting["journal_recorded"] != false || p.journal.Health().State != "degraded" {
		t.Fatal("a storage problem erased valid directly observed native context")
	}
}
