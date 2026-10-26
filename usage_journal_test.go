package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testUsageJournal(t *testing.T, persistent bool) *usageJournal {
	t.Helper()
	path := ""
	if persistent {
		path = filepath.Join(t.TempDir(), "private", "usage.json")
	}
	j, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Shutdown() })
	return j
}

func journalUsage(input, cached, write, output int64, event string) map[string]any {
	return map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": cached, "cache_write_tokens": write},
		"completion_tokens_details": map[string]any{"reasoning_tokens": int64(3)},
		"cursor_usage_event_id":     digest(event),
		// These adversarial fields must never reach the metadata-only snapshot.
		"authorization": "synthetic-never-write-token", "content": "synthetic-never-write-conversation",
	}
}

func journalClosed(t *testing.T, j *usageJournal, account, run string, at time.Time) usageRecord {
	t.Helper()
	r, err := j.Begin(account, "synthetic-conversation", run, "grok-4.7", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Close(r.ID, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUsageJournalRestartAndIndependentLateMeasurement(t *testing.T) {
	j := testUsageJournal(t, true)
	path := j.path
	now := time.Now().Add(-time.Minute)
	first := journalClosed(t, j, "synthetic-account-credential", "run-1", now)
	if err := j.MarkUnknown(first.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	if err := j.Shutdown(); err != nil {
		t.Fatal(err)
	}
	j, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Shutdown() })
	pending := j.Pending("synthetic-account-credential", "")
	if len(pending) != 1 || pending[0].ID != first.ID || pending[0].Reason != "timeout" || pending[0].Usage != nil || pending[0].Ended != now.Add(time.Second).UTC() {
		t.Fatalf("pending identity changed across restart: %+v", pending)
	}
	second := journalClosed(t, j, "synthetic-account-credential", "run-2", now.Add(10*time.Second))
	if err = j.Complete(second.ID, journalUsage(185670, 181266, 4400, 2240, "event-2"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	if err = j.CompleteDeferred(first.ID, journalUsage(181270, 177533, 3735, 1913, "event-1"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	records := j.Records()
	if records[0].Status != "deferred" || records[1].Status != "measured" || records[1].Usage.PromptTokens != 185670 || records[0].Usage.PromptTokens != 181270 {
		t.Fatalf("late usage contaminated current context: %+v", records)
	}
	if len(j.Pending("synthetic-account-credential", "")) != 0 {
		t.Fatal("completed events remained payable")
	}
	if err = j.CompleteDeferred(first.ID, journalUsage(181270, 177533, 3735, 1913, "event-1"), "dashboard"); err != nil {
		t.Fatal("same completion retry was not idempotent", err)
	}
	if err = j.Complete(first.ID, journalUsage(181270, 177533, 3735, 1913, "different-event"), "dashboard"); err == nil {
		t.Fatal("a completed record was assigned another measurement")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-account-credential", "synthetic-never-write-token", "synthetic-never-write-conversation", "authorization", "content"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("journal persisted forbidden material %q", forbidden)
		}
	}
	if runtime.GOOS != "windows" {
		for _, check := range []struct {
			path string
			mode os.FileMode
		}{{filepath.Dir(path), 0700}, {path, 0600}, {path + ".lock", 0600}} {
			info, err := os.Stat(check.path)
			if err != nil || info.Mode().Perm() != check.mode {
				t.Fatalf("journal permissions changed: %v", err)
			}
		}
	}
}

func TestUsageJournalConcurrentClaimAndAccountIsolation(t *testing.T) {
	j := testUsageJournal(t, false)
	now := time.Now().Add(-time.Minute)
	a := journalClosed(t, j, "account-a", "run-a", now)
	b := journalClosed(t, j, "account-a", "run-b", now)
	c := journalClosed(t, j, "account-b", "run-a", now)
	u := journalUsage(140260, 140160, 0, 669, "same-event")
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, id := range []string{a.ID, b.ID} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); errors <- j.Complete(id, u, "dashboard") }(id)
	}
	wg.Wait()
	close(errors)
	success, claimed := 0, 0
	for err := range errors {
		if err == nil {
			success++
		} else if err == errUsageEventClaimed {
			claimed++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || claimed != 1 {
		t.Fatalf("one event was assigned more than once: %d success, %d claimed", success, claimed)
	}
	if err := j.Complete(c.ID, u, "dashboard"); err != nil {
		t.Fatal("another account could not claim its own event", err)
	}
	if len(j.Pending("account-a", "")) != 1 || len(j.Pending("account-b", "")) != 0 || len(j.Pending("unrelated-account", "")) != 0 {
		t.Fatal("pending data crossed account boundaries")
	}
}

func TestUsageJournalUnknownAndMeasuredZeroDiffer(t *testing.T) {
	j := testUsageJournal(t, false)
	r := journalClosed(t, j, "account", "unknown", time.Now().Add(-time.Minute))
	if err := j.MarkUnknown(r.ID, "permission"); err != nil {
		t.Fatal(err)
	}
	if j.Records()[0].Usage != nil {
		t.Fatal("unknown usage was represented as measured zero")
	}
	zero := journalUsage(0, 0, 0, 0, "zero-event")
	zero["completion_tokens_details"] = map[string]any{"reasoning_tokens": 0}
	if err := j.CompleteDeferred(r.ID, zero, "dashboard"); err != nil {
		t.Fatal(err)
	}
	if j.Records()[0].Usage == nil || j.Records()[0].Usage.PromptTokens != 0 || j.Records()[0].Status != "deferred" {
		t.Fatal("authoritative zero was lost")
	}
	for _, reason := range []string{"401: synthetic-sensitive-body", "Bearer sensitive", ""} {
		if j.MarkUnknown(r.ID, reason) == nil {
			t.Fatal("arbitrary reason was accepted")
		}
	}
}

func TestUsageJournalRejectsInvalidOrPartialMeasurements(t *testing.T) {
	for _, name := range []string{"missing-input", "missing-cache", "fractional", "negative", "cache-overflow", "wrong-total", "missing-event", "unknown-source"} {
		t.Run(name, func(t *testing.T) {
			j := testUsageJournal(t, false)
			r := journalClosed(t, j, "account", "run", time.Now().Add(-time.Minute))
			u := journalUsage(100, 90, 1, 10, "event")
			source := "dashboard"
			switch name {
			case "missing-input":
				delete(u, "prompt_tokens")
			case "missing-cache":
				delete(object(u["prompt_tokens_details"]), "cached_tokens")
			case "fractional":
				u["prompt_tokens"] = 100.5
			case "negative":
				u["completion_tokens"] = -1
			case "cache-overflow":
				object(u["prompt_tokens_details"])["cache_write_tokens"] = 11
			case "wrong-total":
				u["total_tokens"] = 111
			case "missing-event":
				delete(u, "cursor_usage_event_id")
			case "unknown-source":
				source = "synthetic-sensitive-body"
			}
			if j.Complete(r.ID, u, source) == nil || j.Records()[0].Usage != nil {
				t.Fatal("invalid usage became a measurement")
			}
		})
	}
}

func TestUsageJournalSnapshotIsolationExpiryAndCapacity(t *testing.T) {
	j := testUsageJournal(t, false)
	now := time.Now()
	old := journalClosed(t, j, "account", "expired", now.Add(-8*24*time.Hour))
	for n := 0; n < 40; n++ {
		journalClosed(t, j, "account", fmt.Sprintf("run-%d", n), now.Add(-time.Minute))
	}
	pending := j.Pending("account", "")
	if len(pending) != 40 {
		t.Fatal("pending selection hid later records from fair reconciliation")
	}
	for _, r := range pending {
		if r.ID == old.ID {
			t.Fatal("expired pending usage was polled again")
		}
	}
	if j.Records()[0].Reason != "expired" || j.Records()[0].Usage != nil {
		t.Fatal("expiry discarded the unknown audit record")
	}
	if len(j.Pending("account", pending[0].ID)) != 39 || j.Pending("account", pending[0].ID)[0].ID == pending[0].ID {
		t.Fatal("current request was included in old pending work")
	}
	r := pending[0]
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	copy := j.Records()
	copy[1].Usage.PromptTokens = 99999
	*copy[1].Usage.ReasoningTokens = 99999
	if j.Records()[1].Usage.PromptTokens != 100 || *j.Records()[1].Usage.ReasoningTokens != 3 {
		t.Fatal("exporter mutated owned measurements")
	}
	// Memory-only storage cannot archive; continue with explicit loss health.
	j.capacity = len(j.records)
	if _, err := j.Begin("account", "conversation", "capacity", "grok-4.7", now); err != nil || len(j.records) != j.capacity || j.Health().LostRecords == 0 {
		t.Fatal("capacity failed execution or hid memory audit loss", err)
	}
}

func TestUsageJournalPersistenceFailurePreservesUnverifiedObservation(t *testing.T) {
	j := testUsageJournal(t, true)
	r := journalClosed(t, j, "account", "run", time.Now().Add(-time.Minute))
	original := j.path
	j.path = filepath.Join(j.path, "missing", "cannot-write")
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err == nil || j.Records()[0].Usage != nil || len(j.Records()[0].Observations) != 1 || j.Records()[0].Observations[0].Kind != "unverified" || j.Health().State != "degraded" {
		t.Fatal("unreadable claim storage was represented as verified ownership", err)
	}
	j.path = original
	if health := j.Flush(); health.State != "healthy" || health.SpoolRecords != 0 {
		t.Fatal("recovered storage did not flush the bounded spool", health)
	}
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err != nil {
		t.Fatal("fresh observation after storage recovery was not accepted", err)
	}
}

func TestUsageJournalClaimAbsenceRequiresAvailableDirectoryStructure(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		wantError bool
	}{
		{"missing-shard", false},
		{"missing-leaf", false},
		{"missing-root", true},
		{"root-is-file", true},
		{"shard-is-file", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			j := testUsageJournal(t, true)
			event := digest("synthetic-directory-check-event")
			root := j.path + ".claims"
			shard := filepath.Dir(j.claimPath(journalAccountHash("account"), event))
			switch scenario.name {
			case "missing-leaf":
				if err := os.Mkdir(shard, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing-root", "root-is-file":
				if err := os.Rename(root, root+".saved"); err != nil {
					t.Fatal(err)
				}
				if scenario.name == "root-is-file" {
					if err := os.WriteFile(root, []byte("synthetic obstruction"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "shard-is-file":
				if err := os.WriteFile(shard, []byte("synthetic obstruction"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			owner, err := j.ClaimOwner("account", event)
			if owner != "" || (err != nil) != scenario.wantError {
				t.Fatalf("directory availability changed ownership semantics: owner=%q err=%v", owner, err)
			}
			if scenario.wantError && j.Health().Reason != "claim_unavailable" {
				t.Fatal("unavailable ownership evidence was not reported")
			}
		})
	}
}

func TestUsageJournalMissingSegmentDirectorySpoolsAndRecovers(t *testing.T) {
	j := testUsageJournal(t, true)
	segments := j.path + ".segments"
	saved := segments + ".saved"
	if err := os.Rename(segments, saved); err != nil {
		t.Fatal(err)
	}
	r := journalClosed(t, j, "account", "missing-segments", time.Now().Add(-time.Minute))
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "missing-segments-event"), "dashboard"); err != nil {
		t.Fatal("WAL unavailability changed valid claim ownership", err)
	}
	if j.blocked || j.Health().Reason != "write_failed" || j.Health().SpoolRecords == 0 {
		t.Fatal("recoverable WAL outage was quarantined", j.Health())
	}
	if err := os.Rename(saved, segments); err != nil {
		t.Fatal(err)
	}
	if health := j.Flush(); health.State != "healthy" || health.SpoolRecords != 0 {
		t.Fatal("restored WAL directory did not recover", health)
	}
}

func TestUsageJournalRevisionsRemainProvisionalAndReplaceOneEvent(t *testing.T) {
	j := testUsageJournal(t, true)
	now := time.Now().UTC()
	j.now = func() time.Time { return now }
	r := journalClosed(t, j, "account", "revision-run", now.Add(-time.Minute))
	first := journalUsage(100, 90, 1, 10, "stable-event")
	if err := j.ObserveSnapshot(r.ID, first, "dashboard", false, false); err != nil {
		t.Fatal(err)
	}
	sequence := j.Health().Sequence
	now = now.Add(time.Second)
	if err := j.ObserveSnapshot(r.ID, first, "dashboard", false, false); err != nil {
		t.Fatal(err)
	}
	if j.Health().Sequence != sequence {
		t.Fatal("same-value sample wrote another durable update")
	}
	now = now.Add(time.Second)
	if err := j.ObserveSnapshot(r.ID, first, "dashboard", false, true); err != nil {
		t.Fatal(err)
	}
	before, _ := j.Record(r.ID)
	if before.Revision != 1 || before.Samples != 3 || !before.Stable || !before.Provisional || before.FirstUsage.PromptTokens != 100 {
		t.Fatal("stable sampling was mislabeled as final", before)
	}
	now = now.Add(time.Second)
	second := journalUsage(150, 140, 2, 20, "stable-event")
	if err := j.ObserveSnapshot(r.ID, second, "dashboard", false, false); err != nil {
		t.Fatal(err)
	}
	after, _ := j.Record(r.ID)
	if after.Revision != 2 || after.Usage.PromptTokens != 150 || after.FirstUsage.PromptTokens != 100 || after.Stable || len(after.Observations) != 2 || after.Observations[1].Kind != "revision" {
		t.Fatal("revision was frozen, summed, or lost first evidence", after)
	}
	if err := j.MarkUnknown(r.ID, "upstream_error"); err != nil {
		t.Fatal(err)
	}
	failed, _ := j.Record(r.ID)
	if failed.Usage.PromptTokens != 150 || failed.FirstUsage.PromptTokens != 100 || failed.Reason != "upstream_error" || failed.Revision != 2 {
		t.Fatal("failed recheck discarded prior evidence")
	}
	if err := j.Complete(r.ID, journalUsage(170, 160, 1, 21, "conflicting-event"), "dashboard"); err != errUsageEventConflict {
		t.Fatal("different event was silently substituted", err)
	}
	conflicted, _ := j.Record(r.ID)
	if conflicted.Usage.PromptTokens != 150 || conflicted.Revision != 2 || conflicted.Conflicts != 1 || conflicted.Observations[len(conflicted.Observations)-1].Kind != "conflict" {
		t.Fatal("event conflict was not separately retained")
	}
	j.Flush()
	path := j.path
	_ = j.Shutdown()
	reopened, err := newUsageJournal(path)
	if err != nil || reopened.Health().State != "healthy" {
		t.Fatal("revised journal did not restart cleanly", err)
	}
	defer reopened.Shutdown()
	persisted, _ := reopened.Record(r.ID)
	if persisted.Revision != 2 || persisted.Usage.PromptTokens != 150 || persisted.FirstUsage.PromptTokens != 100 || persisted.Conflicts != 1 || !persisted.Provisional {
		t.Fatal("revision evidence did not survive restart")
	}
}

func TestUsageJournalCapacityArchivesAndKeepsCrossSegmentClaims(t *testing.T) {
	j := testUsageJournal(t, true)
	j.capacity = 3
	now := time.Now().UTC()
	var first usageRecord
	for n := 0; n < 7; n++ {
		r := journalClosed(t, j, "account", fmt.Sprintf("archive-run-%d", n), now.Add(time.Duration(n-60)*time.Second))
		if n == 0 {
			first = r
		}
		if err := j.Complete(r.ID, journalUsage(int64(100+n), 90, 1, 10, fmt.Sprintf("archive-event-%d", n)), "dashboard"); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			j.segmentBytes = maxUsageSegmentBytes
		}
	}
	if len(j.Records()) != 3 || j.Health().State != "healthy" || j.Health().Segments < 2 {
		t.Fatal("capacity aborted execution or grew without a bound", j.Health())
	}
	archived, ok := j.Record(first.ID)
	if !ok || archived.Usage.PromptTokens != 100 {
		t.Fatal("archival discarded old audit evidence")
	}
	owner, err := j.ClaimOwner("account", digest("archive-event-0"))
	if err != nil || owner != first.ID {
		t.Fatal("archived event claim was forgotten", err)
	}
	current := journalClosed(t, j, "account", "duplicate-archive-event", now)
	if err := j.Complete(current.ID, journalUsage(100, 90, 1, 10, "archive-event-0"), "dashboard"); err != errUsageEventClaimed {
		t.Fatal("old archived event was billed again", err)
	}
	path := j.path
	j.Flush()
	_ = j.Shutdown()
	reopened, err := newUsageJournal(path)
	if err != nil || reopened.Health().State != "healthy" {
		t.Fatal("archive failed restart", err)
	}
	defer reopened.Shutdown()
	if owner, err := reopened.ClaimOwner("account", digest("archive-event-0")); err != nil || owner != first.ID {
		t.Fatal("restart lost archived ownership", err)
	}
	if archived, ok := reopened.Record(first.ID); !ok || archived.Usage.PromptTokens != 100 {
		t.Fatal("restart lost archived record")
	}
}

func TestUsageJournalEvictedUnresolvedWindowBlocksNewAttributionAcrossRestart(t *testing.T) {
	j := testUsageJournal(t, true)
	j.capacity = 2
	now := time.Now().Add(-time.Minute)
	known := journalClosed(t, j, "account", "known-claim", now)
	if err := j.Complete(known.ID, journalUsage(100, 90, 1, 10, "known-event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	_ = journalClosed(t, j, "account", "unknown-window", now.Add(time.Second))
	_ = journalClosed(t, j, "account", "newer-window", now.Add(2*time.Second))
	_ = journalClosed(t, j, "account", "newest-window", now.Add(3*time.Second))
	if !j.Health().AttributionIncomplete {
		t.Fatal("evicted unresolved window silently lost attribution evidence")
	}
	if owner, err := j.ClaimOwner("account", digest("known-event")); err != nil || owner != known.ID {
		t.Fatal("conservative fallback lost a known archived claim", err)
	}
	if owner, err := j.ClaimOwner("account", digest("unseen-event")); err == nil || owner != "" {
		t.Fatal("new event attributed after unresolved window was evicted")
	}
	path := j.path
	j.Flush()
	_ = j.Shutdown()
	reopened, err := newUsageJournal(path)
	if err != nil || !reopened.Health().AttributionIncomplete {
		t.Fatal("restart erased incomplete attribution state", err)
	}
	defer reopened.Shutdown()
	if _, err := reopened.ClaimOwner("account", digest("unseen-after-restart")); err == nil {
		t.Fatal("restart allowed unproven event ownership")
	}
}

func TestUsageJournalV1UpgradePreservesOriginalAndSeedsClaims(t *testing.T) {
	memory := testUsageJournal(t, false)
	r := journalClosed(t, memory, "account", "legacy-run", time.Now().Add(-time.Minute))
	if err := memory.Complete(r.ID, journalUsage(100, 90, 1, 10, "legacy-event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	legacy, _ := memory.Record(r.ID)
	legacy.Sequence, legacy.Revision, legacy.Samples = 0, 0, 0
	legacy.Provisional, legacy.Stable, legacy.FirstUsage, legacy.Observations = false, false, nil, nil
	legacy.ObservedAt, legacy.ReviewedAt = time.Time{}, time.Time{}
	raw, _ := json.Marshal(usageJournalSnapshot{Version: 1, Records: []usageRecord{legacy}})
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	j, err := newUsageJournal(path)
	if err != nil || j.Health().State != "healthy" {
		t.Fatal("v1 journal could not migrate", err, j.Health())
	}
	defer j.Shutdown()
	backup, err := os.ReadFile(filepath.Join(path+".segments", "import-v1.json"))
	if err != nil || string(backup) != string(raw) {
		t.Fatal("migration altered original v1 bytes")
	}
	j.capacity = 1
	if _, err := j.Begin("account", "conversation", "new-after-v1", "grok-4.7", time.Now()); err != nil {
		t.Fatal(err)
	}
	if owner, err := j.ClaimOwner("account", digest("legacy-event")); err != nil || owner != r.ID {
		t.Fatal("v1 event lost its persistent claim on eviction", err)
	}
	upgraded, ok := j.Record(r.ID)
	if !ok || upgraded.Revision != 1 || !upgraded.Provisional || upgraded.FirstUsage.PromptTokens != 100 {
		t.Fatal("migration lost provisional first observation")
	}
}

func TestUsageJournalBoundedSpoolReportsLossAndFlushesAfterRecovery(t *testing.T) {
	j := testUsageJournal(t, true)
	j.spoolCapacity = 3
	segments := j.path + ".segments"
	saved := segments + ".saved"
	if err := os.Rename(segments, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segments, []byte("synthetic obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	for n := 0; n < 5; n++ {
		r := journalClosed(t, j, "account", fmt.Sprintf("spooled-%d", n), now)
		if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, fmt.Sprintf("spooled-event-%d", n)), "dashboard"); err != nil {
			t.Fatal("spool failure aborted model accounting", err)
		}
	}
	health := j.Health()
	if health.State != "degraded" || health.SpoolRecords > 3 || health.SpoolBytes > maxUsageSpoolBytes || health.DroppedObservations == 0 {
		t.Fatal("spool grew unbounded or hid dropped evidence", health)
	}
	if err := os.Remove(segments); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, segments); err != nil {
		t.Fatal(err)
	}
	health = j.Flush()
	if health.SpoolRecords != 0 || health.State != "degraded" || health.Reason != "audit_loss" || health.DroppedObservations == 0 {
		t.Fatal("recovery did not flush or concealed prior loss", health)
	}
	for _, record := range j.Records() {
		if record.Usage == nil || record.Usage.PromptTokens != 100 || !record.Provisional {
			t.Fatal("recovery lost current provisional observations")
		}
	}
	path := j.path
	_ = j.Shutdown()
	reopened, err := newUsageJournal(path)
	if err != nil || reopened.Health().DroppedObservations != health.DroppedObservations || reopened.Health().State != "degraded" {
		t.Fatal("restart hid historical loss", err)
	}
	defer reopened.Shutdown()
}

func TestUsageJournalReviewableRotatesFourIncludingCurrentAndDeferred(t *testing.T) {
	j := testUsageJournal(t, false)
	now := time.Now().Add(-time.Minute)
	for n := 0; n < 11; n++ {
		r := journalClosed(t, j, "account", fmt.Sprintf("review-%d", n), now)
		var err error
		if n%2 == 0 {
			err = j.Complete(r.ID, journalUsage(100, 90, 1, 10, fmt.Sprintf("review-event-%d", n)), "dashboard")
		} else {
			err = j.CompleteDeferred(r.ID, journalUsage(100, 90, 1, 10, fmt.Sprintf("review-event-%d", n)), "dashboard")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for n := 0; n < 3; n++ {
		batch := j.Reviewable("account", "")
		if len(batch) > 4 {
			t.Fatal("review batch would skip unseen candidates when truncated")
		}
		for _, r := range batch {
			seen[r.ID] = true
		}
	}
	if len(seen) != 11 {
		t.Fatal("review cursor starved earlier observations", len(seen))
	}
	if len(j.Reviewable("another-account", "")) != 0 {
		t.Fatal("review crossed account boundaries")
	}
}

func TestUsageJournalIdempotenceDoesNotWriteAgain(t *testing.T) {
	j := testUsageJournal(t, true)
	r := journalClosed(t, j, "account", "idempotent-run", time.Now().Add(-time.Minute))
	usage := journalUsage(100, 90, 1, 10, "idempotent-event")
	if err := j.Complete(r.ID, usage, "dashboard"); err != nil {
		t.Fatal(err)
	}
	j.Flush()
	sequence := j.Health().Sequence
	before, _ := os.ReadFile(j.path)
	if err := j.Close(r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete(r.ID, usage, "dashboard"); err != nil {
		t.Fatal(err)
	}
	j.Flush()
	after, _ := os.ReadFile(j.path)
	if j.Health().Sequence != sequence || string(before) != string(after) {
		t.Fatal("idempotent callbacks triggered another checkpoint/write")
	}
}

func TestUsageJournalRevisionHistoryBoundKeepsOlderEvidenceInSegments(t *testing.T) {
	j := testUsageJournal(t, true)
	now := time.Now().UTC()
	j.now = func() time.Time { return now }
	r := journalClosed(t, j, "account", "many-revisions", now.Add(-time.Minute))
	for n := 0; n < 40; n++ {
		now = now.Add(time.Second)
		if err := j.ObserveSnapshot(r.ID, journalUsage(int64(100+n), 90, 1, 10, "same-revised-event"), "dashboard", false, false); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := j.Record(r.ID)
	if len(current.Observations) != maxUsageHistory || current.HistoryArchived != 8 || current.Revision != 40 || current.Samples != 1 || current.FirstUsage.PromptTokens != 100 || current.Usage.PromptTokens != 139 {
		t.Fatal("history was unbounded or revision counters accumulated", current)
	}
	oldestFound := false
	if err := j.walkLog(func(entry usageLogEntry) error {
		if entry.Record.ID == r.ID && entry.Record.Revision == 1 && entry.Record.Usage.PromptTokens == 100 {
			oldestFound = true
		}
		return nil
	}); err != nil || !oldestFound {
		t.Fatal("bounded active history discarded older audit evidence", err)
	}
}

func TestUsageJournalUnsafeSegmentIsNotOpenedOrOverwritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege is not assumed")
	}
	j := testUsageJournal(t, true)
	r, err := j.Begin("account", "conversation", "unsafe-segment-run", "grok-4.7", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	name := j.segmentPath(j.segment)
	if err := os.Rename(name, name+".saved"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(j.path), "synthetic-untouched")
	const original = "synthetic bytes must remain unchanged"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(r.ID, time.Now()); err != nil {
		t.Fatal("unsafe storage aborted model execution", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original || j.Health().Reason != "unsafe_path" {
		t.Fatal("unsafe segment was opened before validation")
	}
}

func TestUsageJournalModelPathDoesNotScanArchive(t *testing.T) {
	j := testUsageJournal(t, true)
	// A deliberately unreadable historical import would fail an archive scan.
	legacy := filepath.Join(j.path+".segments", "import-v1.json")
	if err := os.WriteFile(legacy, []byte("synthetic invalid historical fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	r := journalClosed(t, j, "account", "hot-path-run", time.Now().Add(-time.Minute))
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "hot-path-event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	if len(j.Reviewable("account", "")) != 1 || j.Health().Reason == "archive_unavailable" {
		t.Fatal("model hot path scanned historical archive")
	}
	if _, ok := j.Record(digest("synthetic-absent-record")); ok || j.Health().Reason != "archive_unavailable" {
		t.Fatal("explicit offline archive lookup did not surface corruption")
	}
}

func TestUsageJournalRejectsCorruptionAndInsecurePaths(t *testing.T) {
	for _, raw := range []string{`{`, `{"version":3,"records":[]}`, `{"version":1,"records":[],"token":"forbidden"}`, `{"version":1,"records":[]} {}`} {
		dir := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "usage.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		j, err := newUsageJournal(path)
		if err != nil || j.Health().State != "degraded" {
			t.Fatal("corruption did not degrade accounting")
		}
		_ = j.Shutdown()
		preserved, _ := os.ReadFile(path)
		if string(preserved) != raw {
			t.Fatal("corrupt original was overwritten")
		}
	}
	if runtime.GOOS != "windows" {
		dir := filepath.Join(t.TempDir(), "public")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		j, err := newUsageJournal(filepath.Join(dir, "usage.json"))
		if err != nil || j.Health().Reason != "unsafe_path" {
			t.Fatal("unsafe path did not degrade accounting")
		}
		_ = j.Shutdown()
	}
}

func TestUsageJournalChildLockHolder(t *testing.T) {
	path := os.Getenv("CURSOR_JOURNAL_TEST_CHILD")
	if path == "" {
		return
	}
	j, err := newUsageJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Begin("synthetic-child-account", "synthetic-conversation", "child-run", "grok-4.7", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	fmt.Println("journal-lock-ready")
	select {}
}

func TestUsageJournalProcessCrashReleasesLockAndRetainsUnknownAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageJournalChildLockHolder$")
	cmd.Env = append(os.Environ(), "CURSOR_JOURNAL_TEST_CHILD="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "journal-lock-ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child could not acquire journal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child journal setup timed out")
	}
	if j, err := newUsageJournal(path); err != nil || j.Health().Reason != "already_owned" {
		t.Fatal("second process did not refuse shared persistent ownership")
	} else {
		_ = j.Shutdown()
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	j, err := newUsageJournal(path)
	if err != nil {
		t.Fatal("OS did not release the crashed process's journal lock", err)
	}
	defer j.Shutdown()
	if len(j.Pending("synthetic-child-account", "")) != 0 {
		t.Fatal("abandoned Run with unknown close time became eligible for exact matching")
	}
	records := j.Records()
	if len(records) != 1 || records[0].Reason != "interrupted" || records[0].Status != "unknown" || records[0].Ended != records[0].Started || records[0].Usage != nil {
		t.Fatalf("abandoned Run was not retained as unknown: %+v", records)
	}
	if err := j.CompleteDeferred(records[0].ID, journalUsage(100, 90, 1, 10, "cannot-match-interrupted"), "dashboard"); err == nil {
		t.Fatal("an interrupted Run with unknown close time was relabeled exact")
	}
	if _, err = os.Stat(path + ".lock"); err != nil {
		t.Fatal("the stable lock inode was removed")
	}
}

func TestUsageJournalReloadRejectsDuplicateEventClaims(t *testing.T) {
	j := testUsageJournal(t, true)
	r1 := journalClosed(t, j, "account", "run-1", time.Now().Add(-time.Minute))
	r2 := journalClosed(t, j, "account", "run-2", time.Now().Add(-time.Minute))
	if err := j.Complete(r1.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err != nil {
		t.Fatal(err)
	}
	records := j.Records()
	records[1].Usage, records[1].EventID, records[1].Source, records[1].Status = records[0].Usage, records[0].EventID, records[0].Source, records[0].Status
	if records[1].ID != r2.ID {
		t.Fatal("fixture identity changed")
	}
	path := j.path
	_ = j.Shutdown()
	raw, _ := json.Marshal(usageJournalSnapshot{Version: 1, Records: records})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := newUsageJournal(path); err != nil || reopened.Health().State != "degraded" {
		t.Fatal("duplicate persisted claims did not degrade accounting")
	} else {
		_ = reopened.Shutdown()
	}
}
