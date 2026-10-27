package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountingFeedRevisionsSurviveArchiveAndRestart(t *testing.T) {
	j := testUsageJournal(t, true)
	j.capacity = 1
	first := journalClosed(t, j, "synthetic-account", "run-1", time.Now().Add(-time.Minute))
	if err := j.SetRunMetadata(first.ID, "grok-4.7-fast"); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete(first.ID, journalUsage(100, 40, 0, 10, "event-1"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	journalClosed(t, j, "synthetic-account", "run-2", time.Now().Add(-time.Second))
	// The old Run left RAM. Its measurement must still be available to a new collector.
	var after uint64
	latest := map[string]map[string]any{}
	for n := 0; n < 20; n++ {
		page, err := j.AccountingChanges(after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Records {
			latest[str(row["id"])] = row
		}
		if page.HasMore && page.NextSequence <= after {
			t.Fatal("cursor did not advance")
		}
		after = page.NextSequence
		if !page.HasMore {
			break
		}
	}
	if latest[first.ID]["requested_model"] != "grok-4.7-fast" || object(latest[first.ID]["usage"])["prompt_tokens"] != int64(100) {
		t.Fatalf("archived Run missing: %v", latest[first.ID])
	}
	path := j.path
	instance := j.instanceID
	if err := j.Shutdown(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	page, err := reopened.AccountingChanges(after, 2)
	if err != nil || page.HasMore || len(page.Records) != 0 || page.InstanceID != instance {
		t.Fatalf("replayed old charges after restart: %+v %v", page, err)
	}
}

func TestAccountingFeedNewJournalHasDifferentEpoch(t *testing.T) {
	a, b := testUsageJournal(t, true), testUsageJournal(t, true)
	for n := 0; n < 3; n++ {
		journalClosed(t, b, "account", fmt.Sprintf("run-%d", n), time.Now().Add(-time.Minute))
	}
	first, err := a.AccountingChanges(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.AccountingChanges(0, 1)
	if err != nil || !isUsageHash(first.InstanceID) || first.InstanceID == second.InstanceID {
		t.Fatalf("journal switch shared a cursor epoch: %+v %+v %v", first, second, err)
	}
}

func TestAccountingLiveSnapshotsStayProvisionalAndDoNotMaskTerminal(t *testing.T) {
	j := testUsageJournal(t, true)
	r, err := j.Begin("account", "synthetic-conversation", "run", "grok-4.7", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	live := journalUsage(100, 60, 0, 10, "live-event")
	if err := j.ObserveLive(r.ID, live); err != nil {
		t.Fatal(err)
	}
	current, _ := j.Record(r.ID)
	if current.Status != "running" || current.Usage != nil || current.LiveUsage == nil {
		t.Fatal("active usage became a completed request measurement")
	}
	page, err := j.AccountingChanges(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	row := page.Records[len(page.Records)-1]
	if row["ended"] != nil || row["status"] != "running" || object(row["usage"])["completion_tokens"] != int64(10) {
		t.Fatalf("bad live public record: %+v", row)
	}
	if err := j.Close(r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A lower authoritative terminal amount replaces the active snapshot.
	if err := j.Complete(r.ID, journalUsage(90, 50, 0, 9, "irrelevant-terminal-id"), "turn_ended"); err != nil {
		t.Fatal(err)
	}
	if err := j.ObserveLive(r.ID, live); err == nil {
		t.Fatal("late live result overwrote terminal")
	}
	current, _ = j.Record(r.ID)
	if current.Usage.PromptTokens != 90 || object(publicAccountingRecord(current)["usage"])["prompt_tokens"] != int64(90) {
		t.Fatal("terminal revision not authoritative")
	}
	other, _ := j.Begin("account", "other-conversation", "other-run", "grok-4.7", time.Now())
	if err := j.ObserveLive(other.ID, live); err == nil {
		t.Fatal("one Dashboard event assigned twice")
	}
}

func TestAccountingFeedPreservesNullAndPrivateMetadata(t *testing.T) {
	u, err := dashboardUsage(map[string]any{"inputTokens": 10, "cacheReadTokens": 50, "outputTokens": 4})
	if err != nil {
		t.Fatal(err)
	}
	u["cursor_usage_event_id"] = digest("event")
	j := testUsageJournal(t, true)
	r := journalClosed(t, j, "synthetic-secret-account", "synthetic-run", time.Now().Add(-time.Second))
	if err := j.Complete(r.ID, u, "dashboard"); err != nil {
		t.Fatal(err)
	}
	page, err := j.AccountingChanges(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	row := page.Records[len(page.Records)-1]
	counts := object(row["usage"])
	if counts["cache_write_tokens"] != nil || counts["prompt_tokens"] != nil || counts["total_tokens"] != nil || counts["cached_tokens"] != int64(50) || counts["completion_tokens"] != int64(4) {
		t.Fatalf("unobserved fields became zero: %+v", counts)
	}
	raw := string(encodeJSON(page))
	for _, secret := range []string{"synthetic-secret-account", "synthetic-conversation", "synthetic-run", "authorization", "content", j.path, "event_id", "account_hash"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("feed leaked %q", secret)
		}
	}
}

func TestAccountingFeedPageValidationAndReference(t *testing.T) {
	j := testUsageJournal(t, false)
	if _, err := j.AccountingChanges(0, 0); err == nil {
		t.Fatal("invalid limit accepted")
	}
	if _, err := j.AccountingChanges(1, 1); err == nil {
		t.Fatal("ahead cursor accepted")
	}
	for _, state := range []string{"pending", "context_unknown", "unknown"} {
		ref := nativeUsageAccounting(map[string]any{"continuity": "native", "state": state, "record_id": digest("run"), "replay": true})
		if ref["scope"] != "run" || ref["provider"] != pluginID || ref["replay"] != true {
			t.Fatal(ref)
		}
	}
	if nativeUsageAccounting(map[string]any{"continuity": "stateless", "record_id": digest("run")}) != nil {
		t.Fatal("host request acquired native reference")
	}
	p := newTestRuntime(t)
	p.registerManagement(rpcRequest{})
	for _, q := range []url.Values{{"limit": {"0"}}, {"limit": {"101"}}, {"after_sequence": {"-1"}}, {"after_sequence": {"1", "2"}}} {
		value, err := p.handleManagement(nil, rpcRequest{Method: "GET", Path: "/v0/management/plugins/" + pluginID + "/accounting", Query: q})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(value)
		if !strings.Contains(string(raw), `"StatusCode":400`) {
			t.Fatalf("bad query %v: %s", q, raw)
		}
	}
}

func TestAccountingFeedRestartPublishesInterruptedRevision(t *testing.T) {
	j := testUsageJournal(t, true)
	r, err := j.Begin("account", "conversation", "run-before-crash", "grok", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	page, err := j.AccountingChanges(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	path, cursor := j.path, page.NextSequence
	if err := j.Shutdown(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	page, err = reopened.AccountingChanges(cursor, 100)
	if err != nil || len(page.Records) != 1 || page.Records[0]["id"] != r.ID || page.Records[0]["status"] != "unknown" || page.NextSequence <= cursor {
		t.Fatalf("restart transition was invisible: %+v %v", page, err)
	}
}

func TestAccountingLegacyFeedRetainsUpgradedUncertainty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).UTC()
	u, err := measurementFromUsage(journalUsage(100, 40, 0, 10, "legacy-event"))
	if err != nil {
		t.Fatal(err)
	}
	r := usageRecord{ID: digest("legacy-id"), Account: journalAccountHash("account"), Conversation: "conversation", RunID: "run", Model: "grok", Started: at, Ended: at.Add(time.Second), Status: "measured", Usage: u, EventID: digest("legacy-event"), Source: "dashboard"}
	if err := os.WriteFile(path, encodeJSON(usageJournalSnapshot{Version: 1, Records: []usageRecord{r}}), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Shutdown()
	page, err := j.AccountingChanges(0, 100)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("legacy feed failed: %+v %v", page, err)
	}
	current, _ := j.Record(r.ID)
	if string(encodeJSON(page.Records[0])) != string(encodeJSON(publicAccountingRecord(current))) {
		t.Fatalf("legacy uncertainty changed: %+v != %+v", page.Records[0], current)
	}
}

func TestAccountingFeedRotatedSegmentsDoNotSkipPages(t *testing.T) {
	j := testUsageJournal(t, true)
	for n := 0; n < 80; n++ {
		journalClosed(t, j, "account", fmt.Sprintf("run-%d", n), time.Now().Add(-time.Minute))
	}
	// Force rotation on the next append without generating megabytes of fixtures.
	j.mu.Lock()
	j.segmentBytes = maxUsageSegmentBytes
	j.mu.Unlock()
	journalClosed(t, j, "account", "last-run", time.Now().Add(-time.Second))
	var after uint64
	for n := 0; n < 100; n++ {
		page, err := j.AccountingChanges(after, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Records {
			sequence := row["sequence"].(uint64)
			if sequence != after+1 {
				t.Fatalf("skipped sequence %d -> %d", after, sequence)
			}
			after = sequence
		}
		if !page.HasMore {
			break
		}
	}
	if after != j.sequence {
		t.Fatalf("missed archive: %d != %d", after, j.sequence)
	}
}
