package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxUsageJournalRecords = 10000
const maxUsageJournalBytes = 16 << 20
const usagePendingLifetime = 7 * 24 * time.Hour
const maxUsageHistory = 32
const maxUsageSpoolRecords = 512
const maxUsageSpoolBytes = 4 << 20
const maxUsageSegmentBytes = 1 << 20

var errUsageJournal = errors.New("usage journal unavailable, invalid, or already owned")
var errUsageEventClaimed = errors.New("usage event was already assigned to another Run")
var errUsageEventConflict = errors.New("usage observation has a conflicting event identity")

// Nil Usage is unknown; a nonnil zero is observed zero. Neither an observation
// nor Stable constitutes an upstream finality guarantee.
type usageMeasurement struct {
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens,omitempty"`
}
type usageObservation struct {
	Revision uint64            `json:"revision"`
	At       time.Time         `json:"at"`
	EventID  string            `json:"event_id"`
	Source   string            `json:"source"`
	Kind     string            `json:"kind"`
	Stable   bool              `json:"stable"`
	Deferred bool              `json:"deferred"`
	Usage    *usageMeasurement `json:"usage"`
}
type usageRecord struct {
	ID                string             `json:"id"`
	Account           string             `json:"account_hash"`
	Conversation      string             `json:"conversation_id"`
	RunID             string             `json:"run_id"`
	Model             string             `json:"model"`
	Started           time.Time          `json:"started"`
	Ended             time.Time          `json:"ended,omitempty"`
	Status            string             `json:"status"`
	Reason            string             `json:"reason,omitempty"`
	Usage             *usageMeasurement  `json:"usage,omitempty"`
	EventID           string             `json:"event_id,omitempty"`
	Source            string             `json:"source,omitempty"`
	InternalToolCalls int                `json:"internal_tool_calls,omitempty"`
	InternalSteps     int                `json:"internal_steps,omitempty"`
	Sequence          uint64             `json:"sequence,omitempty"`
	Provisional       bool               `json:"provisional,omitempty"`
	Stable            bool               `json:"stable,omitempty"`
	Revision          uint64             `json:"revision,omitempty"`
	Samples           uint64             `json:"samples,omitempty"`
	ObservedAt        time.Time          `json:"observed_at,omitempty"`
	ReviewedAt        time.Time          `json:"reviewed_at,omitempty"`
	FirstUsage        *usageMeasurement  `json:"first_usage,omitempty"`
	Observations      []usageObservation `json:"observations,omitempty"`
	HistoryArchived   uint64             `json:"history_archived,omitempty"`
	Conflicts         uint64             `json:"conflicts,omitempty"`
}
type usageJournalSnapshot struct {
	Version               int           `json:"version"`
	Sequence              uint64        `json:"sequence,omitempty"`
	Segment               uint64        `json:"segment,omitempty"`
	DroppedObservations   uint64        `json:"dropped_observations,omitempty"`
	LostRecords           uint64        `json:"lost_records,omitempty"`
	AttributionIncomplete bool          `json:"attribution_incomplete,omitempty"`
	Records               []usageRecord `json:"records"`
}
type usageLogEntry struct {
	Sequence              uint64      `json:"sequence"`
	Record                usageRecord `json:"record"`
	Checksum              string      `json:"checksum"`
	AttributionIncomplete bool        `json:"attribution_incomplete,omitempty"`
}
type usageJournalHealth struct {
	State                 string `json:"state"`
	Reason                string `json:"reason,omitempty"`
	ActiveRecords         int    `json:"active_records"`
	SpoolRecords          int    `json:"spool_records"`
	SpoolBytes            int    `json:"spool_bytes"`
	DroppedObservations   uint64 `json:"dropped_observations"`
	LostRecords           uint64 `json:"lost_records"`
	Segments              uint64 `json:"segments"`
	Sequence              uint64 `json:"sequence"`
	PersistedSequence     uint64 `json:"persisted_sequence"`
	CheckpointDirty       bool   `json:"checkpoint_dirty"`
	AttributionIncomplete bool   `json:"attribution_incomplete"`
}

// The separate lock inode is never unlinked. WAL segments retain older
// records/revisions while RAM and the active checkpoint remain bounded. Small
// claim digests prevent cross-segment event reuse. The spool is NOT crash-durable
// and explicitly reports backlog/loss; storage trouble must not abort a model.
type usageJournal struct {
	mu                                 sync.Mutex
	path                               string
	lock                               *os.File
	closed                             bool
	records                            []usageRecord
	now                                func() time.Time
	sequence, persisted, segment       uint64
	segmentBytes                       int64
	spool                              []usageLogEntry
	spoolBytes                         int
	dropped, lost                      uint64
	storageReason                      string
	blocked, checkpointDirty           bool
	attributionIncomplete              bool
	mutations, capacity, spoolCapacity int
	reviewNext                         map[string]int
}

func newUsageJournal(path string) (*usageJournal, error) {
	j := &usageJournal{path: path, now: time.Now, capacity: maxUsageJournalRecords, spoolCapacity: maxUsageSpoolRecords, reviewNext: map[string]int{}}
	if path != "" {
		if reason := j.openStorage(path); reason != "" {
			j.storageReason, j.blocked = reason, true
		}
	}
	return j, nil
}
func (j *usageJournal) openStorage(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "unsafe_path"
	}
	dir := filepath.Dir(absolute)
	if err = os.MkdirAll(dir, 0700); err != nil || !privateUsagePath(dir, true) {
		return "unsafe_path"
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "unsafe_path"
	}
	j.path = filepath.Join(dir, filepath.Base(absolute))
	for _, name := range []string{j.path, j.path + ".lock"} {
		if info, e := os.Lstat(name); e == nil {
			if !info.Mode().IsRegular() || !privateUsagePath(name, false) {
				return "unsafe_path"
			}
		} else if !os.IsNotExist(e) {
			return "storage_unavailable"
		}
	}
	j.lock, err = os.OpenFile(j.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "storage_unavailable"
	}
	if lockUsageJournal(j.lock) != nil {
		_ = j.lock.Close()
		j.lock = nil
		return "already_owned"
	}
	raw, err := readUsageFile(j.path)
	if err != nil && !os.IsNotExist(err) {
		return "storage_unavailable"
	}
	snapshot := usageJournalSnapshot{Version: 2}
	if err == nil && (decodeUsageJSON(raw, &snapshot) != nil || snapshot.Version != 1 && snapshot.Version != 2 || len(snapshot.Records) > maxUsageJournalRecords) {
		return "corrupt_checkpoint"
	}
	ids, claims := map[string]bool{}, map[string]bool{}
	for n := range snapshot.Records {
		r := &snapshot.Records[n]
		if !validUsageRecord(*r) || ids[r.ID] {
			return "corrupt_checkpoint"
		}
		ids[r.ID] = true
		if r.EventID != "" {
			key := r.Account + ":" + r.EventID
			if claims[key] {
				return "corrupt_checkpoint"
			}
			claims[key] = true
		}
		upgradeUsageRecord(r)
	}
	for _, suffix := range []string{".segments", ".claims"} {
		if err = os.MkdirAll(j.path+suffix, 0700); err != nil || !privateUsagePath(j.path+suffix, true) {
			return "unsafe_path"
		}
	}
	if snapshot.Version == 1 && len(raw) != 0 {
		legacy := filepath.Join(j.path+".segments", "import-v1.json")
		if existing, e := readUsageFile(legacy); e == nil {
			if !bytes.Equal(existing, raw) {
				return "conflicting_legacy_import"
			}
		} else if !os.IsNotExist(e) || writeUsageAtomic(legacy, raw) != nil {
			return "storage_unavailable"
		}
		for n := range snapshot.Records {
			snapshot.Records[n].Sequence = uint64(n + 1)
		}
		snapshot.Sequence = uint64(len(snapshot.Records))
	}
	j.records, j.sequence, j.persisted = snapshot.Records, snapshot.Sequence, snapshot.Sequence
	j.dropped, j.lost = snapshot.DroppedObservations, snapshot.LostRecords
	j.attributionIncomplete = snapshot.AttributionIncomplete
	if j.discoverSegments() != nil {
		return "corrupt_log"
	}
	if j.walkLog(func(entry usageLogEntry) error {
		j.attributionIncomplete = j.attributionIncomplete || entry.AttributionIncomplete
		if err := j.persistClaim(entry.Record); err != nil {
			return err
		}
		if entry.Sequence > snapshot.Sequence {
			j.retain(entry.Record, false)
			j.sequence, j.persisted = entry.Sequence, entry.Sequence
		}
		return nil
	}) != nil {
		return "corrupt_log"
	}
	for n := range j.records {
		r := &j.records[n]
		if r.Status == "running" {
			r.Status, r.Reason, r.Ended = "unknown", "interrupted", r.Started
		}
	}
	j.expire(j.records)
	j.checkpointDirty = true
	if j.checkpoint() != nil {
		j.storageReason = "write_failed"
	}
	return ""
}
func privateUsagePath(path string, directory bool) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() == directory && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}
func readUsageFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !privateUsagePath(path, false) {
		return nil, errUsageJournal
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxUsageJournalBytes+1))
	if err != nil || len(raw) > maxUsageJournalBytes {
		return nil, errUsageJournal
	}
	return raw, nil
}
func decodeUsageJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errUsageJournal
	}
	return nil
}
func validUsageReason(reason string) bool {
	switch reason {
	case "", "unavailable", "cancelled", "timeout", "error", "restart", "interrupted", "expired", "ambiguous", "permission", "rate_limit", "storage", "conflict", "initial_wait", "missing", "observing", "revised", "settled", "invalid", "pagination_limit", "upstream_error":
		return true
	}
	return false
}
func validUsageRecord(r usageRecord) bool {
	if r.InternalToolCalls < 0 || r.InternalToolCalls > 100000 || r.InternalSteps < 0 || r.InternalSteps > 100000 || len(r.Observations) > maxUsageHistory {
		return false
	}
	if !isUsageHash(r.ID) || !isUsageHash(r.Account) || !safeUsageID(r.Conversation) || !safeUsageID(r.RunID) || !safeUsageID(r.Model) || r.Started.IsZero() || !validUsageReason(r.Reason) || !r.Ended.IsZero() && r.Ended.Before(r.Started) {
		return false
	}
	for _, o := range r.Observations {
		if o.At.IsZero() || !isUsageHash(o.EventID) || o.Source != "dashboard" && o.Source != "turn_ended" || o.Usage == nil || !validUsageMeasurement(*o.Usage) {
			return false
		}
		switch o.Kind {
		case "observation", "revision", "conflict", "unverified":
		default:
			return false
		}
	}
	if r.FirstUsage != nil && !validUsageMeasurement(*r.FirstUsage) {
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
func cloneUsageMeasurement(u *usageMeasurement) *usageMeasurement {
	if u == nil {
		return nil
	}
	copy := *u
	if u.ReasoningTokens != nil {
		value := *u.ReasoningTokens
		copy.ReasoningTokens = &value
	}
	return &copy
}
func cloneUsageRecord(r usageRecord) usageRecord {
	r.Usage, r.FirstUsage = cloneUsageMeasurement(r.Usage), cloneUsageMeasurement(r.FirstUsage)
	r.Observations = append([]usageObservation(nil), r.Observations...)
	for n := range r.Observations {
		r.Observations[n].Usage = cloneUsageMeasurement(r.Observations[n].Usage)
	}
	return r
}
func upgradeUsageRecord(r *usageRecord) {
	if r.Usage != nil && r.Revision == 0 {
		r.Provisional, r.Revision, r.Samples = true, 1, 1
		r.ObservedAt, r.ReviewedAt = r.Ended, r.Ended
		r.FirstUsage = cloneUsageMeasurement(r.Usage)
		r.Observations = []usageObservation{{Revision: 1, At: r.Ended, EventID: r.EventID, Source: r.Source, Kind: "observation", Deferred: r.Status == "deferred", Usage: cloneUsageMeasurement(r.Usage)}}
	}
}
func (j *usageJournal) Begin(account, cid, runID, model string, at time.Time) (usageRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || account == "" || !safeUsageID(cid) || !safeUsageID(runID) || !safeUsageID(model) || at.IsZero() {
		return usageRecord{}, errUsageJournal
	}
	accountHash := journalAccountHash(account)
	id := digest(accountHash + "\x00" + runID)
	if r, ok := j.recordLocked(id); ok {
		if r.Conversation != cid || r.Model != model {
			return usageRecord{}, errUsageJournal
		}
		return r, nil
	}
	r := usageRecord{ID: id, Account: accountHash, Conversation: cid, RunID: runID, Model: model, Started: at.UTC(), Status: "running"}
	j.expire(j.records)
	j.commit(r)
	r.Sequence = j.sequence
	return cloneUsageRecord(r), nil
}
func (j *usageJournal) Close(id string, at time.Time) error {
	return j.update(id, func(r *usageRecord) error {
		if at.IsZero() || at.Before(r.Started) {
			return errUsageJournal
		}
		if r.Ended.IsZero() {
			r.Ended, r.Status = at.UTC(), "pending"
		}
		return nil
	})
}
func (j *usageJournal) MarkUnknown(id, reason string) error {
	if reason == "" || !validUsageReason(reason) {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		if r.Ended.IsZero() {
			return errUsageJournal
		}
		r.Reason, r.Stable, r.Samples = reason, false, 0
		if r.Usage == nil {
			r.Status = "unknown"
		}
		return nil
	})
}
func (j *usageJournal) SetInternalToolCalls(id string, count int) error {
	return j.setInternal(id, count, false)
}
func (j *usageJournal) SetInternalSteps(id string, count int) error {
	return j.setInternal(id, count, true)
}
func (j *usageJournal) setInternal(id string, count int, steps bool) error {
	if count < 0 || count > 100000 {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		if steps {
			r.InternalSteps = count
		} else {
			r.InternalToolCalls = count
		}
		return nil
	})
}
func (j *usageJournal) Complete(id string, usage map[string]any, source string) error {
	return j.ObserveSnapshot(id, usage, source, false, true)
}
func (j *usageJournal) CompleteDeferred(id string, usage map[string]any, source string) error {
	return j.ObserveSnapshot(id, usage, source, true, true)
}
func (j *usageJournal) Observe(id string, usage map[string]any, source string, deferred bool) error {
	return j.ObserveSnapshot(id, usage, source, deferred, false)
}

// Revisions replace a single event's latest view, never add another charge.
// Different event IDs remain conflict evidence. Stable is resolver sampling
// evidence only: even reviewed records intentionally remain Provisional.
func (j *usageJournal) ObserveSnapshot(id string, usage map[string]any, source string, deferred, stable bool) error {
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
	if source == "turn_ended" {
		event = digest("Cursor TurnEnded\x00" + id)
	}
	if !isUsageHash(event) {
		return errUsageJournal
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.recordLocked(id)
	if !ok || j.closed || r.Ended.IsZero() || r.Reason == "interrupted" {
		return errUsageJournal
	}
	now := j.now().UTC()
	observation := usageObservation{At: now, EventID: event, Source: source, Stable: stable, Deferred: deferred, Usage: measurement}
	if r.EventID != "" && (r.EventID != event || r.Source != source) {
		observation.Kind, observation.Revision = "conflict", r.Revision
		j.addObservation(&r, observation)
		r.Conflicts++
		j.commit(r)
		return errUsageEventConflict
	}
	owner, claimErr := j.claimOwnerLocked(r.Account, event)
	if owner != "" && owner != id {
		observation.Kind, observation.Revision = "conflict", r.Revision
		j.addObservation(&r, observation)
		r.Conflicts++
		j.commit(r)
		return errUsageEventClaimed
	}
	if claimErr != nil && r.EventID != event {
		observation.Kind, observation.Revision = "unverified", r.Revision
		j.addObservation(&r, observation)
		if r.Usage == nil {
			r.Status, r.Reason = "unknown", "storage"
		}
		j.commit(r)
		return errUsageJournal
	}
	unchanged := r.Usage != nil && reflect.DeepEqual(r.Usage, measurement)
	if !unchanged || r.Stable && !stable {
		r.Samples = 1
	} else {
		r.Samples++
	}
	r.ReviewedAt = now
	if unchanged && r.Stable == stable && r.Reason == "" {
		j.retain(r, false)
		return nil
	}
	if !unchanged {
		r.Revision++
		r.ObservedAt = now
		observation.Kind = "observation"
		if r.Usage != nil {
			observation.Kind = "revision"
		}
		observation.Revision = r.Revision
		j.addObservation(&r, observation)
		if r.FirstUsage == nil {
			r.FirstUsage = cloneUsageMeasurement(measurement)
		}
	}
	r.Usage, r.EventID, r.Source, r.Reason = measurement, event, source, ""
	r.Provisional, r.Stable = true, stable
	if r.Status != "measured" && r.Status != "deferred" {
		r.Status = "measured"
		if deferred {
			r.Status = "deferred"
		}
	}
	j.commit(r)
	return nil
}
func (j *usageJournal) addObservation(r *usageRecord, o usageObservation) {
	if len(r.Observations) >= maxUsageHistory {
		r.Observations = append([]usageObservation(nil), r.Observations[1:]...)
		r.HistoryArchived++
	}
	r.Observations = append(r.Observations, o)
}
func (j *usageJournal) ClaimOwner(account, event string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !isUsageHash(event) || j.closed {
		return "", errUsageJournal
	}
	return j.claimOwnerLocked(journalAccountHash(account), event)
}
func (j *usageJournal) claimOwnerLocked(account, event string) (string, error) {
	for _, r := range j.records {
		if r.Account == account && r.EventID == event {
			return r.ID, nil
		}
	}
	for _, entry := range j.spool {
		if entry.Record.Account == account && entry.Record.EventID == event {
			return entry.Record.ID, nil
		}
	}
	if j.path == "" {
		if j.lost != 0 {
			return "", errUsageJournal
		}
		return "", nil
	}
	if j.blocked || j.lock == nil {
		return "", errUsageJournal
	}
	name := j.claimPath(account, event)
	// A missing leaf proves absence only under an available claim store. In
	// particular, Windows can report PATH_NOT_FOUND when a parent is a file;
	// interpreting that as an unclaimed event would invent ownership evidence.
	if !privateUsagePath(j.path+".claims", true) {
		j.storageReason = "claim_unavailable"
		return "", errUsageJournal
	}
	if _, err := os.Lstat(filepath.Dir(name)); err == nil {
		if !privateUsagePath(filepath.Dir(name), true) {
			j.storageReason = "claim_unavailable"
			return "", errUsageJournal
		}
	} else if !os.IsNotExist(err) {
		j.storageReason = "claim_unavailable"
		return "", errUsageJournal
	}
	raw, err := readUsageFile(name)
	if os.IsNotExist(err) {
		if j.attributionIncomplete {
			return "", errUsageJournal
		}
		return "", nil
	}
	if err != nil || !privateUsagePath(name, false) || !isUsageHash(string(raw)) {
		j.storageReason = "claim_unavailable"
		return "", errUsageJournal
	}
	return string(raw), nil
}
func (j *usageJournal) claimPath(account, event string) string {
	key := digest(account + "\x00" + event)
	return filepath.Join(j.path+".claims", key[:2], key+".claim")
}
func (j *usageJournal) persistClaim(r usageRecord) error {
	if r.EventID == "" {
		return nil
	}
	// The root was created when this journal acquired storage ownership. Do
	// not silently recreate a missing root and forget its older claim digests.
	if !privateUsagePath(j.path+".claims", true) {
		return errUsageJournal
	}
	name := j.claimPath(r.Account, r.EventID)
	if raw, err := readUsageFile(name); err == nil {
		if string(raw) != r.ID || !privateUsagePath(name, false) {
			return errUsageEventClaimed
		}
		return nil
	} else if !os.IsNotExist(err) {
		return errUsageJournal
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil || !privateUsagePath(filepath.Dir(name), true) {
		return errUsageJournal
	}
	return writeUsageAtomic(name, []byte(r.ID))
}
func (j *usageJournal) update(id string, change func(*usageRecord) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errUsageJournal
	}
	r, ok := j.recordLocked(id)
	if !ok {
		return errUsageJournal
	}
	before := cloneUsageRecord(r)
	if err := change(&r); err != nil {
		return err
	}
	if !reflect.DeepEqual(before, r) {
		j.commit(r)
	}
	return nil
}
func usageEntryChecksum(seq uint64, r usageRecord, attribution ...bool) string {
	incomplete := len(attribution) != 0 && attribution[0]
	return digest(string(encodeJSON(struct {
		Sequence              uint64      `json:"sequence"`
		Record                usageRecord `json:"record"`
		AttributionIncomplete bool        `json:"attribution_incomplete,omitempty"`
	}{seq, r, incomplete})))
}
func (j *usageJournal) commit(r usageRecord) {
	j.sequence++
	r.Sequence = j.sequence
	j.retain(r, true)
	j.checkpointDirty = true
	if j.path == "" {
		return
	}
	entry := usageLogEntry{Sequence: j.sequence, Record: cloneUsageRecord(r), AttributionIncomplete: j.attributionIncomplete}
	entry.Checksum = usageEntryChecksum(entry.Sequence, entry.Record, entry.AttributionIncomplete)
	size := len(encodeJSON(entry)) + 1
	for len(j.spool) > 0 && (len(j.spool) >= j.spoolCapacity || j.spoolBytes+size > maxUsageSpoolBytes) {
		j.spoolBytes -= len(encodeJSON(j.spool[0])) + 1
		j.spool = j.spool[1:]
		j.dropped++
	}
	if size > maxUsageSpoolBytes || j.spoolCapacity <= 0 {
		j.dropped++
		return
	}
	j.spool = append(j.spool, entry)
	j.spoolBytes += size
	if !j.blocked {
		j.flushSpool()
		j.mutations++
		if j.mutations >= 64 && len(j.spool) == 0 {
			_ = j.checkpoint()
		}
	}
}
func (j *usageJournal) retain(r usageRecord, archive bool) {
	for n := range j.records {
		if j.records[n].ID == r.ID {
			j.records[n] = cloneUsageRecord(r)
			return
		}
	}
	if len(j.records) >= j.capacity {
		index := 0
		for n, record := range j.records {
			if j.evictionPriority(record) < j.evictionPriority(j.records[index]) || j.evictionPriority(record) == j.evictionPriority(j.records[index]) && record.Started.Before(j.records[index].Started) {
				index = n
			}
		}
		prior := j.records[index]
		j.noteEviction(prior, archive)
		j.records = append(j.records[:index], j.records[index+1:]...)
	}
	j.records = append(j.records, cloneUsageRecord(r))
}

func (j *usageJournal) noteEviction(r usageRecord, archive bool) {
	if r.Usage == nil && r.Reason != "expired" && (r.Ended.IsZero() || !r.Ended.Before(j.now().Add(-usagePendingLifetime))) {
		// Removing an unresolved time window loses negative attribution evidence.
		// Old claims can still be looked up, but new ownership stays conservative.
		j.attributionIncomplete = true
	}
	if archive {
		if j.path == "" {
			j.lost++
		} else if !j.blocked && r.EventID != "" && j.persistClaim(r) != nil {
			j.storageReason = "write_failed"
		}
	}
}

func (j *usageJournal) trimActive(target int) {
	type candidate struct {
		index, priority int
		started         time.Time
	}
	ordered := make([]candidate, len(j.records))
	for n, r := range j.records {
		ordered[n] = candidate{n, j.evictionPriority(r), r.Started}
	}
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].priority != ordered[b].priority {
			return ordered[a].priority < ordered[b].priority
		}
		return ordered[a].started.Before(ordered[b].started)
	})
	drop := make(map[int]bool, len(j.records)-target)
	for _, selected := range ordered[:len(j.records)-target] {
		drop[selected.index] = true
		j.noteEviction(j.records[selected.index], true)
	}
	retained := j.records[:0]
	for n, r := range j.records {
		if !drop[n] {
			retained = append(retained, r)
		}
	}
	j.records = retained
}

func (j *usageJournal) evictionPriority(r usageRecord) int {
	if r.Reason == "expired" || !r.Ended.IsZero() && r.Ended.Before(j.now().Add(-usagePendingLifetime)) {
		return 0
	}
	if r.Usage != nil && r.Stable && r.ReviewedAt.Before(j.now().Add(-time.Hour)) {
		return 1
	}
	if r.Usage != nil {
		return 2
	}
	if r.Status != "running" {
		return 3
	}
	return 4
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
func (j *usageJournal) Pending(account, excludeID string) []usageRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []usageRecord
	for _, r := range j.records {
		if !j.closed && r.Account == journalAccountHash(account) && r.ID != excludeID && (r.Status == "pending" || r.Status == "unknown") && j.reviewable(r) {
			out = append(out, cloneUsageRecord(r))
		}
	}
	return out
}
func (j *usageJournal) reviewable(r usageRecord) bool {
	return !r.Ended.IsZero() && !r.Ended.Before(j.now().Add(-usagePendingLifetime)) && r.Reason != "expired" && r.Reason != "interrupted" && (r.Usage == nil || r.Source == "dashboard")
}

// Current and deferred dashboard observations stay reviewable. The active view
// rotates. Archived records remain available for explicit offline inspection;
// model requests never scan the historical archive while holding this mutex.
func (j *usageJournal) Reviewable(account, excludeID string) []usageRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	account = journalAccountHash(account)
	var candidates []usageRecord
	for _, r := range j.records {
		if r.Account == account && r.ID != excludeID && j.reviewable(r) {
			candidates = append(candidates, r)
		}
	}
	if len(j.reviewNext) >= 256 {
		j.reviewNext = map[string]int{}
	}
	var out []usageRecord
	if len(candidates) > 0 {
		start := j.reviewNext[account] % len(candidates)
		for n := 0; n < len(candidates) && n < 4; n++ {
			out = append(out, cloneUsageRecord(candidates[(start+n)%len(candidates)]))
		}
		j.reviewNext[account] = (start + len(out)) % len(candidates)
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
func (j *usageJournal) Record(id string) (usageRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r, ok := j.recordLocked(id); ok {
		return r, true
	}
	var found usageRecord
	if j.path != "" && !j.blocked {
		if j.walkLog(func(entry usageLogEntry) error {
			if entry.Record.ID == id && entry.Sequence > found.Sequence {
				found = entry.Record
			}
			return nil
		}) != nil {
			j.storageReason = "archive_unavailable"
		}
	}
	return cloneUsageRecord(found), found.ID != ""
}
func (j *usageJournal) recordLocked(id string) (usageRecord, bool) {
	for _, r := range j.records {
		if r.ID == id {
			return cloneUsageRecord(r), true
		}
	}
	var found usageRecord
	for _, entry := range j.spool {
		if entry.Record.ID == id {
			found = entry.Record
		}
	}
	return cloneUsageRecord(found), found.ID != ""
}
func (j *usageJournal) segmentPath(n uint64) string {
	return filepath.Join(j.path+".segments", fmt.Sprintf("events-%020d.jsonl", n))
}
func (j *usageJournal) discoverSegments() error {
	dir, err := os.Open(j.path + ".segments")
	if err != nil {
		return errUsageJournal
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(16)
		for _, entry := range entries {
			name := entry.Name()
			if name == "import-v1.json" || strings.HasPrefix(name, ".usage-journal-") {
				continue
			}
			if !strings.HasPrefix(name, "events-") || !strings.HasSuffix(name, ".jsonl") || entry.IsDir() {
				return errUsageJournal
			}
			n, e := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "events-"), ".jsonl"), 10, 64)
			if e != nil || n == 0 {
				return errUsageJournal
			}
			if n > j.segment {
				j.segment = n
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return errUsageJournal
		}
	}
	if j.segment > 0 {
		info, err := os.Stat(j.segmentPath(j.segment))
		if err != nil {
			return errUsageJournal
		}
		j.segmentBytes = info.Size()
	}
	return nil
}
func (j *usageJournal) walkLog(visit func(usageLogEntry) error) error {
	legacy := filepath.Join(j.path+".segments", "import-v1.json")
	if raw, err := readUsageFile(legacy); err == nil {
		var snapshot usageJournalSnapshot
		if !privateUsagePath(legacy, false) || decodeUsageJSON(raw, &snapshot) != nil || snapshot.Version != 1 || len(snapshot.Records) > maxUsageJournalRecords {
			return errUsageJournal
		}
		for n, r := range snapshot.Records {
			if !validUsageRecord(r) {
				return errUsageJournal
			}
			upgradeUsageRecord(&r)
			r.Sequence = uint64(n + 1)
			if err := visit(usageLogEntry{Sequence: r.Sequence, Record: r}); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return errUsageJournal
	}
	var previous uint64
	for segment := uint64(1); segment <= j.segment; segment++ {
		name := j.segmentPath(segment)
		if !privateUsagePath(name, false) {
			return errUsageJournal
		}
		file, err := os.Open(name)
		if err != nil {
			return errUsageJournal
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxUsageSegmentBytes)
		for scanner.Scan() {
			var entry usageLogEntry
			if decodeUsageJSON(scanner.Bytes(), &entry) != nil || entry.Sequence <= previous || entry.Sequence != entry.Record.Sequence || !validUsageRecord(entry.Record) || entry.Checksum != usageEntryChecksum(entry.Sequence, entry.Record, entry.AttributionIncomplete) {
				_ = file.Close()
				return errUsageJournal
			}
			previous = entry.Sequence
			if err := visit(entry); err != nil {
				_ = file.Close()
				return err
			}
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return errUsageJournal
		}
	}
	return nil
}
func (j *usageJournal) flushSpool() {
	if j.blocked || j.lock == nil || len(j.spool) == 0 {
		return
	}
	// Validate the parent before classifying a missing segment leaf. A missing
	// directory or a regular-file obstruction is an availability failure: keep
	// the bounded spool and retry after the owner restores storage. Symlinks or
	// nonprivate directories remain unsafe and must never be opened for append.
	segmentDir := j.path + ".segments"
	if info, err := os.Lstat(segmentDir); err != nil || !info.IsDir() {
		j.storageReason = "write_failed"
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			j.storageReason, j.blocked = "unsafe_path", true
		}
		return
	}
	if !privateUsagePath(segmentDir, true) {
		j.storageReason, j.blocked = "unsafe_path", true
		return
	}
	for len(j.spool) > 0 {
		if j.segment == 0 || j.segmentBytes >= maxUsageSegmentBytes {
			j.segment++
			j.segmentBytes = 0
		}
		var batch bytes.Buffer
		count := 0
		for _, entry := range j.spool {
			raw := append(encodeJSON(entry), '\n')
			if count > 0 && (count >= 64 || j.segmentBytes+int64(batch.Len()+len(raw)) > maxUsageSegmentBytes) {
				break
			}
			if j.persistClaim(entry.Record) != nil {
				j.storageReason = "claim_unavailable"
				return
			}
			batch.Write(raw)
			count++
		}
		name := j.segmentPath(j.segment)
		flags := os.O_WRONLY | os.O_APPEND
		created := false
		if info, statErr := os.Lstat(name); statErr == nil {
			if !info.Mode().IsRegular() || !privateUsagePath(name, false) {
				j.storageReason, j.blocked = "unsafe_path", true
				return
			}
		} else if os.IsNotExist(statErr) {
			flags |= os.O_CREATE | os.O_EXCL
			created = true
		} else {
			j.storageReason = "write_failed"
			return
		}
		file, err := os.OpenFile(name, flags, 0600)
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			j.storageReason = "write_failed"
			return
		}
		n, err := file.Write(batch.Bytes())
		if err == nil && n != batch.Len() {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = file.Sync()
		}
		if err == nil && created {
			err = syncUsageDirectory(filepath.Dir(name))
		}
		if err != nil {
			// Roll back only this process's failed append, never unknown data found
			// damaged at startup. An unsuccessful rollback quarantines this path.
			rollback := file.Truncate(j.segmentBytes)
			_ = file.Close()
			j.storageReason = "write_failed"
			if rollback != nil {
				j.blocked, j.storageReason = true, "corrupt_log"
			}
			return
		}
		_ = file.Close()
		j.segmentBytes += int64(batch.Len())
		j.persisted = j.spool[count-1].Sequence
		j.spool = j.spool[count:]
		j.spoolBytes -= batch.Len()
	}
	j.storageReason = ""
}
func writeUsageAtomic(path string, raw []byte) error {
	if _, err := os.Lstat(path); err == nil {
		if !privateUsagePath(path, false) {
			return errUsageJournal
		}
	} else if !os.IsNotExist(err) {
		return errUsageJournal
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-journal-")
	if err != nil {
		return errUsageJournal
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || os.Rename(name, path) != nil {
		return errUsageJournal
	}
	return syncUsageDirectory(filepath.Dir(path))
}
func syncUsageDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	} // os.File has no Windows directory sync primitive.
	dir, err := os.Open(path)
	if err != nil {
		return errUsageJournal
	}
	defer dir.Close()
	return dir.Sync()
}
func (j *usageJournal) checkpoint() error {
	if j.path == "" || j.blocked || len(j.spool) > 0 || !j.checkpointDirty {
		return nil
	}
	snapshot := func() ([]byte, error) {
		return json.Marshal(usageJournalSnapshot{Version: 2, Sequence: j.sequence, Segment: j.segment, DroppedObservations: j.dropped, LostRecords: j.lost, AttributionIncomplete: j.attributionIncomplete, Records: j.records})
	}
	raw, err := snapshot()
	for err == nil && len(raw) > maxUsageJournalBytes && len(j.records) > 1 {
		// Archive batches rather than repeatedly serializing after each eviction.
		target := max(1, len(j.records)*(maxUsageJournalBytes-4096)/len(raw)*3/4)
		j.trimActive(target)
		raw, err = snapshot()
	}
	if err != nil || len(raw) > maxUsageJournalBytes || writeUsageAtomic(j.path, raw) != nil {
		j.storageReason = "write_failed"
		return errUsageJournal
	}
	j.checkpointDirty, j.mutations = false, 0
	return nil
}

// Flush drains at most the bounded spool (512 entries/4 MiB), without indefinite
// retries. Call off the user path if needed; OS file syscalls have no Go deadline.
func (j *usageJournal) Flush() usageJournalHealth {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.closed && !j.blocked && j.path != "" {
		j.flushSpool()
		_ = j.checkpoint()
	}
	return j.healthLocked()
}
func (j *usageJournal) Health() usageJournalHealth {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.healthLocked()
}
func (j *usageJournal) healthLocked() usageJournalHealth {
	state, reason := "healthy", j.storageReason
	if j.path == "" {
		state = "memory"
	}
	if reason != "" || len(j.spool) > 0 {
		state = "degraded"
		if reason == "" {
			reason = "spooling"
		}
	}
	if j.dropped > 0 || j.lost > 0 {
		state, reason = "degraded", "audit_loss"
	}
	if j.attributionIncomplete && reason == "" {
		state, reason = "degraded", "attribution_incomplete"
	}
	return usageJournalHealth{State: state, Reason: reason, ActiveRecords: len(j.records), SpoolRecords: len(j.spool), SpoolBytes: j.spoolBytes, DroppedObservations: j.dropped, LostRecords: j.lost, Segments: j.segment, Sequence: j.sequence, PersistedSequence: j.persisted, CheckpointDirty: j.checkpointDirty, AttributionIncomplete: j.attributionIncomplete}
}
func (j *usageJournal) Shutdown() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	if !j.blocked && j.path != "" {
		j.flushSpool()
		_ = j.checkpoint()
	}
	j.closed = true
	if j.lock != nil {
		unlockUsageJournal(j.lock)
		_ = j.lock.Close()
		j.lock = nil
	}
	return nil
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

func validUsageMeasurement(u usageMeasurement) bool {
	const max = int64(1<<53 - 1)
	for _, value := range []int64{u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.CachedTokens, u.CacheWriteTokens} {
		if value < 0 || value > max {
			return false
		}
	}
	return u.PromptTokens <= max-u.CompletionTokens && u.TotalTokens == u.PromptTokens+u.CompletionTokens && u.CachedTokens <= u.PromptTokens && u.CacheWriteTokens <= u.PromptTokens-u.CachedTokens && (u.ReasoningTokens == nil || *u.ReasoningTokens >= 0 && *u.ReasoningTokens <= max)
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
