package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
	// Exercise the hard cap without discarding any existing audit information.
	j.records = append(j.records, make([]usageRecord, maxUsageJournalRecords-len(j.records))...)
	if _, err := j.Begin("account", "conversation", "capacity", "grok-4.7", now); !errors.Is(err, errUsageJournalCapacity) || len(j.records) != maxUsageJournalRecords {
		t.Fatal("capacity silently dropped audit records or grew without a bound", err)
	}
}

func TestUsageJournalPersistenceFailureDoesNotCommitClaim(t *testing.T) {
	j := testUsageJournal(t, true)
	r := journalClosed(t, j, "account", "run", time.Now().Add(-time.Minute))
	original := j.path
	j.path = filepath.Join(j.path, "missing", "cannot-write")
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err == nil || j.Records()[0].Usage != nil {
		t.Fatal("failed durable write committed an in-memory event claim")
	}
	j.path = original
	if err := j.Complete(r.ID, journalUsage(100, 90, 1, 10, "event"), "dashboard"); err != nil {
		t.Fatal("retry after failed durable write could not claim its event", err)
	}
}

func TestUsageJournalRejectsCorruptionAndInsecurePaths(t *testing.T) {
	for _, raw := range []string{`{`, `{"version":2,"records":[]}`, `{"version":1,"records":[],"token":"forbidden"}`, `{"version":1,"records":[]} {}`} {
		dir := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "usage.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if j, err := newUsageJournal(path); err == nil {
			_ = j.Shutdown()
			t.Fatal("corrupt snapshot was accepted")
		}
	}
	if runtime.GOOS != "windows" {
		dir := filepath.Join(t.TempDir(), "public")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if j, err := newUsageJournal(filepath.Join(dir, "usage.json")); err == nil {
			_ = j.Shutdown()
			t.Fatal("journal accepted a nonprivate directory")
		}
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
	if j, err := newUsageJournal(path); err == nil {
		_ = j.Shutdown()
		t.Fatal("two processes acquired the same journal")
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
	if reopened, err := newUsageJournal(path); err == nil {
		_ = reopened.Shutdown()
		t.Fatal("duplicate persisted event claims were accepted")
	}
}
