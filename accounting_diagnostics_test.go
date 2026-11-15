package main

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAccountingDiagnosticsDistinguishesDeferredUnknownAndMeasuredZero(t *testing.T) {
	p := newTestRuntime(t)
	p.registerManagement(rpcRequest{BasePath: "/v8/management", ResourceBasePath: "/v8/resource/plugins/" + pluginID})
	now := time.Now().UTC()
	p.journal.now = func() time.Time { return now }
	unknown := journalClosed(t, p.journal, "synthetic-private-account", "synthetic-unknown-run", now.Add(-time.Minute))
	if err := p.journal.MarkUnknown(unknown.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	zero := journalClosed(t, p.journal, "synthetic-private-account", "synthetic-zero-run", now.Add(-30*time.Second))
	zeroUsage := journalUsage(0, 0, 0, 0, "synthetic-zero-event")
	delete(zeroUsage, "completion_tokens_details")
	if err := p.journal.Complete(zero.ID, zeroUsage, "turn_ended"); err != nil {
		t.Fatal(err)
	}
	late := journalClosed(t, p.journal, "synthetic-private-account", "synthetic-late-run", now.Add(-10*time.Second))
	if err := p.journal.CompleteDeferred(late.ID, journalUsage(100, 90, 1, 10, "synthetic-event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := p.journal.ObserveSnapshot(late.ID, journalUsage(150, 140, 1, 20, "synthetic-event"), "dashboard", true, false); err != nil {
		t.Fatal(err)
	}
	if err := p.journal.MarkUnknown(late.ID, "upstream_error"); err != nil {
		t.Fatal(err)
	}
	before := p.journal.Health()
	result := invokeNative(t, p, "management.handle", map[string]any{
		"Method": "GET", "Path": "/v8/management/plugins/" + pluginID + "/accounting",
	}, nil)
	body, err := base64.StdEncoding.DecodeString(str(object(result["result"])["Body"]))
	if err != nil {
		t.Fatal(err)
	}
	var value accountingManagementResponse
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	summary := value.Summary
	if summary.Scope != "active_journal_only" || summary.Records != 3 || summary.WithMeasurement != 2 || summary.WithoutMeasurement != 1 {
		t.Fatalf("unknown was confused with a zero/deferred measurement: %+v", summary)
	}
	if summary.StatusCounts["deferred"] != 1 || summary.StatusCounts["measured"] != 1 || summary.StatusCounts["unknown"] != 1 || summary.SourceCounts["turn_ended"] != 1 || summary.SourceCounts["dashboard"] != 1 {
		t.Fatalf("status/source counts lost the journal semantics: %+v", summary)
	}
	latest := summary.LatestMeasurement
	if latest == nil || latest.Usage.PromptTokens != 150 || latest.Scope != "run" || latest.Status != "deferred" || latest.Stable || !latest.Provisional || latest.Revision != 2 || latest.ReviewReason != "upstream_error" || summary.RevisedRecords != 1 {
		t.Fatalf("the latest observation was summed, dropped or called final: %+v", latest)
	}
	if summary.ReasonCounts["timeout"] != 1 || summary.ReasonCounts["upstream_error"] != 1 || value.Contract.PerInferenceContext != "unavailable" || value.Contract.MissingMeansZero || value.Contract.StableMeansFinal {
		t.Fatalf("measurement limitations are missing: %+v", value)
	}
	if !reflect.DeepEqual(before, p.journal.Health()) {
		t.Fatal("diagnostics changed the journal")
	}
	for _, forbidden := range []string{"synthetic-private", "synthetic-event", "synthetic-conversation", "account_hash", "conversation_id", "run_id", "event_id", "authorization", "content"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("diagnostics exposed identity/content field %q", forbidden)
		}
	}
}

func TestAccountingDiagnosticsDoesNotReadArchiveAndReturnsOwnedValues(t *testing.T) {
	j := testUsageJournal(t, false)
	now := time.Now().UTC()
	record := journalClosed(t, j, "account", "run", now.Add(-time.Second))
	if err := j.Complete(record.ID, journalUsage(123, 100, 1, 7, "event"), "turn_ended"); err != nil {
		t.Fatal(err)
	}
	// A management read needs only the bounded in-memory view, even when archive
	// storage is unavailable. It must not repair paths or traverse segments.
	j.path = filepath.Join(t.TempDir(), "missing-parent", "usage.json")
	j.blocked = true
	before := j.Health()
	health, summary := j.diagnosticSnapshot()
	if !reflect.DeepEqual(health, before) || summary.WithMeasurement != 1 {
		t.Fatal("unavailable archive changed the diagnostic view")
	}
	summary.LatestMeasurement.Usage.PromptTokens = 999
	_, again := j.diagnosticSnapshot()
	if again.LatestMeasurement.Usage.PromptTokens != 123 {
		t.Fatal("caller can mutate a journal observation through diagnostics")
	}
	var absent *usageJournal
	health, summary = absent.diagnosticSnapshot()
	if health.State != "unavailable" || summary.Records != 0 || summary.LatestMeasurement != nil {
		t.Fatal("missing journal was reported as measured zero")
	}
}

func TestAccountingDiagnosticsReportsEffectiveNativeLimitsWithoutProxy(t *testing.T) {
	p := newTestRuntime(t)
	if err := p.configureAccounting([]byte("transport: native\nnative-proxy-url: http://synthetic-user:synthetic-secret@127.0.0.1:9999\nnative-tool-result-timeout-seconds: 90\nnative-run-lifetime-seconds: 600\n")); err != nil {
		t.Fatal(err)
	}
	value := p.accountingDiagnostics()
	if value.Contract.EffectiveTransport != "native" || value.Contract.NativeLimits == nil || value.Contract.NativeLimits.ToolResultTimeoutSeconds != 90 || value.Contract.NativeLimits.RunLifetimeSeconds != 600 || value.Contract.PermanentNativeHandleStatus != 400 {
		t.Fatalf("effective limits/recovery contract missing: %+v", value.Contract)
	}
	encoded := string(encodeJSON(value))
	for _, forbidden := range []string{"synthetic-user", "synthetic-secret", "127.0.0.1", "native-proxy-url"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatal("diagnostics leaked proxy configuration")
		}
	}
	if err := p.configureAccounting([]byte("transport: host\n")); err != nil {
		t.Fatal(err)
	}
	value = p.accountingDiagnostics()
	if value.Contract.EffectiveTransport != "host" || value.Contract.NativeLimits != nil {
		t.Fatal("inactive native limits were reported as effective")
	}
}
