package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

const maxUsageJournalRecords = 10000
const maxUsageJournalBytes = 16 << 20
const usagePendingLifetime = 7 * 24 * time.Hour

var errUsageJournal = errors.New("usage journal unavailable, invalid, or already owned")
var errUsageJournalCapacity = errors.New("usage journal capacity reached; retained records require offline archival")
var errUsageEventClaimed = errors.New("usage event was already assigned to another Run")

// usageMeasurement contains only measured counters. Nil Usage in a record means
// unknown, whereas a nonnil all-zero measurement is an authoritative zero.
type usageMeasurement struct {
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens,omitempty"`
}

type usageRecord struct {
	ID                string            `json:"id"`
	Account           string            `json:"account_hash"`
	Conversation      string            `json:"conversation_id"`
	RunID             string            `json:"run_id"`
	Model             string            `json:"model"`
	Started           time.Time         `json:"started"`
	Ended             time.Time         `json:"ended,omitempty"`
	Status            string            `json:"status"`
	Reason            string            `json:"reason,omitempty"`
	Usage             *usageMeasurement `json:"usage,omitempty"`
	EventID           string            `json:"event_id,omitempty"`
	Source            string            `json:"source,omitempty"`
	InternalToolCalls int               `json:"internal_tool_calls,omitempty"`
}

type usageJournalSnapshot struct {
	Version int           `json:"version"`
	Records []usageRecord `json:"records"`
}

// A journal is process-owned until Shutdown. A persistent path must have its
// own private directory. The advisory lock is held on a separate stable inode,
// never unlinked, and is released by the OS on process exit (including crashes).
// Windows callers must additionally restrict the directory ACL to their user;
// Go's file permission bits do not implement Windows ACL policy.
type usageJournal struct {
	mu      sync.Mutex
	path    string
	lock    *os.File
	closed  bool
	records []usageRecord
	now     func() time.Time
}

func newUsageJournal(path string) (*usageJournal, error) {
	j := &usageJournal{now: time.Now}
	if path == "" {
		return j, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errUsageJournal
	}
	dir := filepath.Dir(absolute)
	if err = os.MkdirAll(dir, 0700); err != nil || !privateUsagePath(dir, true) {
		return nil, errUsageJournal
	}
	// Resolve existing parent symlinks before opening the lock, so two aliases
	// of the same directory cannot bypass exclusive ownership.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errUsageJournal
	}
	j.path = filepath.Join(dir, filepath.Base(absolute))
	if info, statErr := os.Lstat(j.path); statErr == nil {
		if !info.Mode().IsRegular() || !privateUsagePath(j.path, false) {
			return nil, errUsageJournal
		}
	} else if !os.IsNotExist(statErr) {
		return nil, errUsageJournal
	}
	lockPath := j.path + ".lock"
	if info, statErr := os.Lstat(lockPath); statErr == nil && !info.Mode().IsRegular() {
		return nil, errUsageJournal
	}
	j.lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errUsageJournal
	}
	if !privateUsagePath(lockPath, false) || lockUsageJournal(j.lock) != nil {
		_ = j.lock.Close()
		return nil, errUsageJournal
	}
	fail := func() (*usageJournal, error) { _ = j.Shutdown(); return nil, errUsageJournal }
	file, err := os.Open(j.path)
	if os.IsNotExist(err) {
		if err = j.persist(nil); err != nil {
			return fail()
		}
		return j, nil
	}
	if err != nil {
		return fail()
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxUsageJournalBytes+1))
	_ = file.Close()
	if readErr != nil || len(raw) > maxUsageJournalBytes {
		return fail()
	}
	var snapshot usageJournalSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(new(any)) != io.EOF || snapshot.Version != 1 || len(snapshot.Records) > maxUsageJournalRecords {
		return fail()
	}
	ids, claims := map[string]bool{}, map[string]bool{}
	for n := range snapshot.Records {
		r := &snapshot.Records[n]
		if !validUsageRecord(*r) || ids[r.ID] {
			return fail()
		}
		ids[r.ID] = true
		if r.EventID != "" {
			claim := r.Account + ":" + r.EventID
			if claims[claim] {
				return fail()
			}
			claims[claim] = true
		}
		if r.Status == "running" {
			// A checkpoint is evidence, never a native Run restart handle. Its
			// close time was lost, so never infer a broad window ending at restart.
			// Keep it for audit; automatic reconciliation must skip this record.
			r.Status, r.Reason, r.Ended = "unknown", "interrupted", r.Started
		}
	}
	j.records = snapshot.Records
	j.expire(j.records)
	if err = j.persist(j.records); err != nil {
		return fail()
	}
	return j, nil
}

func privateUsagePath(path string, directory bool) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() != directory {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0
}

func journalAccountHash(account string) string {
	if isUsageHash(account) {
		return account
	}
	return digest("Cursor usage account\x00" + account)
}

func safeUsageID(value string) bool {
	if len(value) == 0 || len(value) > 160 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/@", r)) {
			return false
		}
	}
	return true
}

func isUsageHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

func validUsageReason(reason string) bool {
	switch reason {
	case "", "unavailable", "cancelled", "timeout", "error", "restart", "interrupted", "expired", "ambiguous", "permission", "rate_limit":
		return true
	}
	return false
}

func validUsageRecord(r usageRecord) bool {
	if r.InternalToolCalls < 0 || r.InternalToolCalls > 100000 {
		return false
	}
	if !isUsageHash(r.ID) || !isUsageHash(r.Account) || !safeUsageID(r.Conversation) || !safeUsageID(r.RunID) || !safeUsageID(r.Model) || r.Started.IsZero() || !validUsageReason(r.Reason) {
		return false
	}
	if !r.Ended.IsZero() && r.Ended.Before(r.Started) {
		return false
	}
	switch r.Status {
	case "running":
		return r.Usage == nil && r.Ended.IsZero() && r.Source == "" && r.EventID == ""
	case "pending", "unknown":
		return r.Usage == nil && !r.Ended.IsZero() && r.Source == "" && r.EventID == ""
	case "measured", "deferred":
		return r.Usage != nil && !r.Ended.IsZero() && (r.Source == "dashboard" || r.Source == "turn_ended") && isUsageHash(r.EventID) && validUsageMeasurement(*r.Usage)
	}
	return false
}

func validUsageMeasurement(u usageMeasurement) bool {
	const max = int64(1<<53 - 1)
	for _, value := range []int64{u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.CachedTokens, u.CacheWriteTokens} {
		if value < 0 || value > max {
			return false
		}
	}
	return u.PromptTokens <= max-u.CompletionTokens && u.TotalTokens == u.PromptTokens+u.CompletionTokens && u.CachedTokens <= u.PromptTokens && u.CacheWriteTokens <= u.PromptTokens-u.CachedTokens && (u.ReasoningTokens == nil || *u.ReasoningTokens >= 0 && *u.ReasoningTokens <= max)
}

func (j *usageJournal) Begin(account, cid, runID, model string, at time.Time) (usageRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || account == "" || !safeUsageID(cid) || !safeUsageID(runID) || !safeUsageID(model) || at.IsZero() {
		return usageRecord{}, errUsageJournal
	}
	accountHash := journalAccountHash(account)
	id := digest(accountHash + "\x00" + runID)
	for _, r := range j.records {
		if r.ID == id {
			if r.Conversation != cid || r.Model != model {
				return usageRecord{}, errUsageJournal
			}
			return cloneUsageRecord(r), nil
		}
	}
	if len(j.records) >= maxUsageJournalRecords {
		return usageRecord{}, errUsageJournalCapacity
	}
	r := usageRecord{ID: id, Account: accountHash, Conversation: cid, RunID: runID, Model: model, Started: at.UTC(), Status: "running"}
	next := append(append([]usageRecord(nil), j.records...), r)
	j.expire(next)
	if err := j.persist(next); err != nil {
		return usageRecord{}, err
	}
	j.records = next
	return r, nil
}

func (j *usageJournal) Close(id string, at time.Time) error {
	return j.update(id, func(r *usageRecord) error {
		if at.IsZero() || at.Before(r.Started) {
			return errUsageJournal
		}
		if !r.Ended.IsZero() {
			return nil
		}
		r.Ended, r.Status = at.UTC(), "pending"
		return nil
	})
}

func (j *usageJournal) MarkUnknown(id, reason string) error {
	if reason == "" || !validUsageReason(reason) {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		if r.Status == "measured" || r.Status == "deferred" || r.Ended.IsZero() {
			return errUsageJournal
		}
		r.Status, r.Reason = "unknown", reason
		return nil
	})
}

func (j *usageJournal) SetInternalToolCalls(id string, count int) error {
	if count < 0 || count > 100000 {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		r.InternalToolCalls = count
		return nil
	})
}

// Complete records the current Run's standard response measurement only.
func (j *usageJournal) Complete(id string, usage map[string]any, source string) error {
	return j.complete(id, usage, source, false)
}

// CompleteDeferred records a late measurement as independent audit data. This
// method never adds its prompt to another Run or returns standard response usage.
func (j *usageJournal) CompleteDeferred(id string, usage map[string]any, source string) error {
	return j.complete(id, usage, source, true)
}

func measurementFromUsage(usage map[string]any) (*usageMeasurement, error) {
	u := &usageMeasurement{}
	values := []struct {
		value any
		out   *int64
	}{
		{usage["prompt_tokens"], &u.PromptTokens}, {usage["completion_tokens"], &u.CompletionTokens}, {usage["total_tokens"], &u.TotalTokens},
		{object(usage["prompt_tokens_details"])["cached_tokens"], &u.CachedTokens}, {object(usage["prompt_tokens_details"])["cache_write_tokens"], &u.CacheWriteTokens},
	}
	for _, entry := range values {
		value, ok := usageInteger(entry.value)
		if !ok {
			return nil, errUsageJournal
		}
		*entry.out = value
	}
	if raw, exists := object(usage["completion_tokens_details"])["reasoning_tokens"]; exists {
		value, ok := usageInteger(raw)
		if !ok {
			return nil, errUsageJournal
		}
		u.ReasoningTokens = &value
	}
	if !validUsageMeasurement(*u) {
		return nil, errUsageJournal
	}
	return u, nil
}

func (j *usageJournal) complete(id string, usage map[string]any, source string, deferred bool) error {
	if source == "turn-ended" {
		source = "turn_ended"
	}
	if source != "dashboard" && source != "turn_ended" {
		return errUsageJournal
	}
	measurement, err := measurementFromUsage(usage)
	if err != nil {
		return err
	}
	event := str(usage["cursor_usage_event_id"])
	if source == "dashboard" && !isUsageHash(event) {
		return errUsageJournal
	}
	if source == "turn_ended" {
		event = digest("Cursor TurnEnded\x00" + id)
	}
	return j.update(id, func(r *usageRecord) error {
		if r.Ended.IsZero() || r.Reason == "interrupted" {
			return errUsageJournal
		}
		if r.Usage != nil {
			if r.EventID == event && r.Source == source && reflect.DeepEqual(r.Usage, measurement) {
				return nil
			}
			return errUsageJournal
		}
		for _, other := range j.records {
			if other.ID != r.ID && other.Account == r.Account && other.EventID == event {
				return errUsageEventClaimed
			}
		}
		r.Usage, r.EventID, r.Source, r.Reason = measurement, event, source, ""
		r.Status = "measured"
		if deferred {
			r.Status = "deferred"
		}
		return nil
	})
}

func (j *usageJournal) update(id string, change func(*usageRecord) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errUsageJournal
	}
	next := append([]usageRecord(nil), j.records...)
	for n := range next {
		if next[n].ID != id {
			continue
		}
		if err := change(&next[n]); err != nil {
			return err
		}
		if err := j.persist(next); err != nil {
			return err
		}
		j.records = next
		return nil
	}
	return errUsageJournal
}

func (j *usageJournal) expire(records []usageRecord) {
	cutoff := j.now().Add(-usagePendingLifetime)
	for n := range records {
		r := &records[n]
		if (r.Status == "pending" || r.Status == "unknown") && r.Reason != "interrupted" && r.Ended.Before(cutoff) {
			r.Status, r.Reason = "unknown", "expired"
		}
	}
}

func cloneUsageRecord(r usageRecord) usageRecord {
	if r.Usage != nil {
		u := *r.Usage
		if u.ReasoningTokens != nil {
			value := *u.ReasoningTokens
			u.ReasoningTokens = &value
		}
		r.Usage = &u
	}
	return r
}

func (j *usageJournal) Pending(account, excludeID string) []usageRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	accountHash := journalAccountHash(account)
	cutoff := j.now().Add(-usagePendingLifetime)
	var out []usageRecord
	if j.closed {
		return out
	}
	for _, r := range j.records {
		if r.Account == accountHash && r.ID != excludeID && (r.Status == "pending" || r.Status == "unknown") && r.Reason != "expired" && r.Reason != "interrupted" && !r.Ended.Before(cutoff) {
			out = append(out, cloneUsageRecord(r))
		}
	}
	return out
}

func (j *usageJournal) Records() []usageRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]usageRecord, len(j.records))
	for n, r := range j.records {
		out[n] = cloneUsageRecord(r)
	}
	return out
}

func (j *usageJournal) persist(records []usageRecord) error {
	if j.path == "" {
		return nil
	}
	raw, err := json.Marshal(usageJournalSnapshot{Version: 1, Records: records})
	if err != nil || len(raw) > maxUsageJournalBytes {
		return errUsageJournal
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".usage-journal-")
	if err != nil {
		return errUsageJournal
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || os.Rename(name, j.path) != nil {
		return errUsageJournal
	}
	// Directory sync makes the rename durable on supporting filesystems.
	// Windows cannot sync a directory through os.File; the atomic file replace
	// still prevents a partially serialized snapshot from being accepted.
	if dir, openErr := os.Open(filepath.Dir(j.path)); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (j *usageJournal) Shutdown() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.lock != nil {
		unlockUsageJournal(j.lock)
		_ = j.lock.Close()
		j.lock = nil
	}
	return nil
}
