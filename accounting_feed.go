package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

const maxAccountingPage = 100

// Accounting is a separate, revisioned Run ledger. It never changes a
// request's standard usage or treats the Run's sum as its context occupancy.
func (j *usageJournal) SetRunMetadata(id, requestedModel string) error {
	if requestedModel != "" && !safeUsageID(requestedModel) {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		if r.RequestedModel == "" {
			r.RequestedModel = requestedModel
		}
		return nil
	})
}

func (j *usageJournal) SetNativeTiming(id string, activeMs int64, ttftMs *int64) error {
	if activeMs < 0 || ttftMs != nil && *ttftMs < 0 {
		return errUsageJournal
	}
	return j.update(id, func(r *usageRecord) error {
		r.ActiveInferenceMs = &activeMs
		if ttftMs != nil {
			v := *ttftMs
			r.TTFTMs = &v
		}
		return nil
	})
}

// An active event can keep growing. Keep its provisional snapshot separate
// from terminal reconciliation, which may use the authoritative binary event.
func (j *usageJournal) ObserveLive(id string, usage map[string]any) error {
	u, err := measurementFromUsage(usage)
	if err != nil {
		return err
	}
	event := str(usage["cursor_usage_event_id"])
	if !isUsageHash(event) {
		return errUsageJournal
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.recordLocked(id)
	if !ok || j.closed || !r.Ended.IsZero() {
		return errUsageJournal
	}
	if r.LiveEventID != "" && r.LiveEventID != event {
		return errUsageEventConflict
	}
	owner, err := j.claimOwnerLocked(r.Account, event)
	if err != nil {
		return err
	}
	if owner != "" && owner != id {
		return errUsageEventClaimed
	}
	if reflect.DeepEqual(r.LiveUsage, u) {
		return nil
	}
	claim := r
	claim.EventID = event
	if j.path != "" && j.persistClaim(claim) != nil {
		return errUsageJournal
	}
	now := j.now().UTC()
	r.LiveUsage, r.LiveEventID, r.LiveObservedAt = u, event, &now
	r.Provisional = true
	r.Revision++
	j.commit(r)
	return nil
}

type accountingPage struct {
	SchemaVersion int              `json:"schema_version"`
	InstanceID    string           `json:"instance_id"`
	Sequence      uint64           `json:"sequence"`
	NextSequence  uint64           `json:"next_sequence"`
	HasMore       bool             `json:"has_more"`
	Records       []map[string]any `json:"records"`
}

// A sequence cursor reads the append log, including revisions of records that
// have left the active in-memory view. Limits apply to records and disk reads.
func (j *usageJournal) AccountingChanges(after uint64, limit int) (accountingPage, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	page := accountingPage{SchemaVersion: 1, InstanceID: j.instanceID, Sequence: j.sequence, NextSequence: after, Records: []map[string]any{}}
	if limit < 1 || limit > maxAccountingPage || after > j.sequence || j.closed || j.blocked {
		return page, errUsageJournal
	}
	appendRecord := func(r usageRecord) bool {
		if r.Sequence <= page.NextSequence {
			return true
		}
		if len(page.Records) == limit {
			return false
		}
		page.Records = append(page.Records, publicAccountingRecord(r))
		page.NextSequence = r.Sequence
		return true
	}
	if j.path == "" {
		records := append([]usageRecord(nil), j.records...)
		sort.Slice(records, func(a, b int) bool { return records[a].Sequence < records[b].Sequence })
		for _, r := range records {
			if !appendRecord(r) {
				break
			}
		}
	} else {
		// The one-time v1 import is bounded by the existing checkpoint limit.
		if raw, err := readUsageFile(filepath.Join(j.path+".segments", "import-v1.json")); err == nil {
			var legacy usageJournalSnapshot
			if decodeUsageJSON(raw, &legacy) != nil || len(legacy.Records) > maxUsageJournalRecords {
				return page, errUsageJournal
			}
			for n, r := range legacy.Records {
				upgradeUsageRecord(&r)
				r.Sequence = uint64(n + 1)
				if !validUsageRecord(r) {
					return page, errUsageJournal
				}
				if !appendRecord(r) {
					break
				}
			}
		} else if !os.IsNotExist(err) {
			return page, errUsageJournal
		}
		if len(page.Records) < limit && page.NextSequence < j.persisted && j.segment > 0 {
			segment, err := j.accountingSegment(page.NextSequence)
			if err != nil {
				return page, err
			}
			budget := int64(4 * maxUsageSegmentBytes)
			for ; segment <= j.segment && len(page.Records) < limit && budget > 0; segment++ {
				name := j.segmentPath(segment)
				if !privateUsagePath(name, false) {
					return page, errUsageJournal
				}
				f, err := os.Open(name)
				if err != nil {
					return page, errUsageJournal
				}
				scanner := bufio.NewScanner(io.LimitReader(f, budget+1))
				scanner.Buffer(make([]byte, 4096), maxUsageSegmentBytes)
				for scanner.Scan() {
					line := scanner.Bytes()
					budget -= int64(len(line) + 1)
					if budget < 0 {
						_ = f.Close()
						return page, errUsageJournal
					}
					var entry usageLogEntry
					if decodeUsageJSON(line, &entry) != nil || entry.Sequence != entry.Record.Sequence || !validUsageRecord(entry.Record) || entry.Checksum != usageEntryChecksum(entry.Sequence, entry.Record, entry.AttributionIncomplete) {
						_ = f.Close()
						return page, errUsageJournal
					}
					if entry.Sequence > page.NextSequence && !appendRecord(entry.Record) {
						break
					}
					if len(page.Records) == limit {
						break
					}
				}
				err = scanner.Err()
				_ = f.Close()
				if err != nil {
					return page, errUsageJournal
				}
			}
		}
		if page.NextSequence >= j.persisted {
			for _, entry := range j.spool {
				if !appendRecord(entry.Record) {
					break
				}
			}
		}
	}
	page.HasMore = page.NextSequence < page.Sequence
	if page.HasMore && len(page.Records) == 0 {
		return page, errUsageJournal
	}
	return page, nil
}

// Locate the first relevant segment in O(log segments), not by rereading the
// archive on every collector tick. Only its first bounded JSONL frame is read.
func (j *usageJournal) accountingSegment(after uint64) (uint64, error) {
	lo, hi := uint64(1), j.segment
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		name := j.segmentPath(mid)
		if !privateUsagePath(name, false) {
			return 0, errUsageJournal
		}
		f, err := os.Open(name)
		if err != nil {
			return 0, errUsageJournal
		}
		r := bufio.NewReader(io.LimitReader(f, maxUsageSegmentBytes+1))
		line, err := r.ReadBytes('\n')
		_ = f.Close()
		var entry usageLogEntry
		if err != nil || json.Unmarshal(line, &entry) != nil || entry.Sequence == 0 {
			return 0, errUsageJournal
		}
		if entry.Sequence <= after {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

func publicAccountingRecord(r usageRecord) map[string]any {
	u, source, observed := r.Usage, r.Source, any(r.ObservedAt)
	if u == nil && r.LiveUsage != nil {
		u, source, observed = r.LiveUsage, "dashboard", r.LiveObservedAt
	}
	var counters any
	if u != nil {
		values := map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.TotalTokens, "cached_tokens": u.CachedTokens, "cache_write_tokens": u.CacheWriteTokens, "reasoning_tokens": u.ReasoningTokens}
		if u.Observed != nil {
			if u.Observed["reported_input_tokens"] {
				values["reported_input_tokens"] = u.PromptTokens
			}
			if u.Observed["reported_total_tokens"] {
				values["reported_total_tokens"] = u.TotalTokens
			}
		}
		if u.Observed != nil {
			for name := range values {
				if !u.Observed[name] {
					values[name] = nil
				}
			}
		} else if source == "dashboard" {
			// Old journals discarded JSON field presence. Do not promote their
			// default cache-write zero into a newly claimed measurement.
			values["cache_write_tokens"] = nil
		}
		counters = values
	}
	var ended any
	if !r.Ended.IsZero() {
		ended = r.Ended
	}
	if r.ObservedAt.IsZero() && r.LiveObservedAt == nil {
		observed = nil
	}
	return map[string]any{"id": r.ID, "sequence": r.Sequence, "revision": r.Revision,
		"model": r.Model, "requested_model": r.RequestedModel, "started": r.Started, "ended": ended,
		"status": r.Status, "source": source, "provisional": r.Provisional, "stable": r.Stable,
		"observed_at": observed, "usage": counters, "active_inference_ms": r.ActiveInferenceMs, "ttft_ms": r.TTFTMs}
}

func accountingRequestedModel(req rpcRequest, fallback string) string {
	var original struct {
		Model string `json:"model"`
	}
	if len(req.OriginalRequest) > 0 && json.Unmarshal(req.OriginalRequest, &original) == nil && safeUsageID(original.Model) {
		return original.Model
	}
	if safeUsageID(fallback) {
		return fallback
	}
	return ""
}

func nativeUsageAccounting(accounting map[string]any) map[string]any {
	if str(accounting["continuity"]) != "native" || !isUsageHash(str(accounting["record_id"])) {
		return nil
	}
	state := str(accounting["state"])
	if state == "context_unknown" {
		state = "measured"
	}
	switch state {
	case "pending", "running", "measured", "deferred", "unknown":
	default:
		state = "unknown"
	}
	return map[string]any{"provider": pluginID, "scope": "run", "record_id": accounting["record_id"], "state": state, "replay": accounting["replay"] == true, "resumed": accounting["resumed"] == true}
}
