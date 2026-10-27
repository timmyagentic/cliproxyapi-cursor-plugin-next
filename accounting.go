package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// The journal and concurrency boundary use a credential fingerprint, never a
// bearer token or an account label. API-key rotation intentionally starts a new
// boundary; exchanging the same API key for a fresh access token does not.
func usageAccount(req rpcRequest, token string) string {
	auth, _, err := storedCredential(req)
	if err == nil {
		switch {
		case auth.Key != "":
			return digest("cursor-api-account\x00" + auth.Key)
		case auth.AccountID != "":
			return digest("cursor-oauth-account\x00" + auth.AccountID)
		case auth.Refresh != "" && auth.Refresh != "cursor-agent":
			return digest("cursor-oauth-refresh\x00" + auth.Refresh)
		}
	}
	return digest("cursor-access-account\x00" + token)
}

func (p *pluginRuntime) acquireUsageRun(account, cid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return failure(503, "Cursor plugin is shut down")
	}
	if p.journal == nil {
		return failure(503, "Cursor usage journal is unavailable")
	}
	key := account + "\x00" + cid
	if p.usageActive[key] {
		return failure(409, "Another Cursor Run is active for this account and conversation")
	}
	p.usageActive[key] = true
	p.usageRuns.Add(1)
	return nil
}

func (p *pluginRuntime) releaseUsageRun(account, cid string) {
	p.mu.Lock()
	delete(p.usageActive, account+"\x00"+cid)
	p.mu.Unlock()
	p.usageRuns.Done()
}

// Timestamps alone cannot prove ownership. Once a different event is claimed
// for another Run, its overlapping timestamp window no longer makes the current
// event ambiguous. Unresolved overlapping Runs remain a reason to omit usage.
func (p *pluginRuntime) unambiguousUsage(account, cid, id string, usage map[string]any) bool {
	stamp, ok := usageInteger(usage["cursor_usage_timestamp"])
	if !ok {
		return false
	}
	eventID := str(usage["cursor_usage_event_id"])
	if eventID != "" {
		owner, err := p.journal.ClaimOwner(account, eventID)
		if err != nil || owner != "" && owner != id {
			return false
		}
		if owner == id {
			return true
		}
	}
	at := time.UnixMilli(stamp)
	for _, record := range p.journal.Records() {
		if record.ID == id || record.Account != account || record.Conversation != cid {
			continue
		}
		if eventID != "" && record.Source == "dashboard" && record.EventID != "" && record.EventID != eventID {
			continue
		}
		end := record.Ended
		if end.IsZero() {
			end = p.now()
		}
		if !at.Before(record.Started.Add(-p.usagePolicy.Skew)) && !at.After(end.Add(p.usagePolicy.Skew)) {
			return false
		}
	}
	return true
}

func publicUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	result := copyMap(usage)
	delete(result, "cursor_usage_event_id")
	delete(result, "cursor_usage_timestamp")
	delete(result, "cursor_usage_stability_samples")
	return result
}

// resolveRunUsage journals successive observations, including changed counters
// for the same event, before deciding whether a settled snapshot can be returned.
// A storage or attribution failure never turns a successful model reply into 503.
func (p *pluginRuntime) resolveRunUsage(ctx context.Context, s *requestScope, token, account, id string, window usageWindow, policy stepUsagePolicy, deferred bool) (map[string]any, error) {
	return p.resolveRunUsageWithFetcher(ctx, account, id, window, policy, deferred, func(ctx context.Context, body map[string]any) (map[string]any, error) {
		return s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, body)
	})
}

func (p *pluginRuntime) resolveRunUsageWithFetcher(ctx context.Context, account, id string, window usageWindow, policy stepUsagePolicy, deferred bool, fetch usageFetcher) (map[string]any, error) {
	var observationErr error
	options := UsageResolveOptions{
		ExcludeEvent: func(eventID string) bool {
			owner, err := p.journal.ClaimOwner(account, eventID)
			if err != nil {
				observationErr = err
				return true
			}
			return owner != "" && owner != id
		},
		Observe: func(observation UsageObservation) {
			if observation.Usage == nil {
				return
			}
			if !p.unambiguousUsage(account, window.Conversation, id, observation.Usage) {
				observationErr = usageUnavailable(UsageAmbiguous, "overlapping unresolved Run")
				return
			}
			if err := p.journal.ObserveSnapshot(id, observation.Usage, "dashboard", deferred, observation.Stable); err != nil {
				observationErr = err
			} else {
				observationErr = nil
			}
		},
	}
	usage, err := resolveDashboardUsageWithOptions(ctx, window, fetch, policy, options)
	if observationErr != nil {
		return nil, observationErr
	}
	if err != nil || usage == nil {
		return nil, err
	}
	if !p.unambiguousUsage(account, window.Conversation, id, usage) {
		return nil, usageUnavailable(UsageAmbiguous, "overlapping unresolved Run")
	}
	return usage, nil
}

func (p *pluginRuntime) accountUsage(s *requestScope, token, account, id, cid, model string, started, ended time.Time, terminal runEvent) runEvent {
	_ = p.journal.SetInternalToolCalls(id, terminal.internalCalls)
	_ = p.journal.SetInternalSteps(id, terminal.internalSteps)
	source := "turn_ended"
	usage := terminal.usage
	var err error
	if usage == nil && s.ctx.Err() == nil {
		source = "dashboard"
		usage, err = p.resolveRunUsage(s.ctx, s, token, account, id, usageWindow{cid, model, started, ended}, p.usagePolicy, false)
	} else if usage != nil {
		// Native counters arrive directly on this Run and need no timestamp
		// attribution. Keep that valid measurement even if the independent
		// audit journal is unavailable; its health remains visible below.
		err = p.journal.Complete(id, usage, source)
	}
	terminal.usage = publicUsage(usage)
	if terminal.internalCalls > 0 || terminal.internalSteps > 0 {
		// Both TurnEnded and Dashboard usage can total several model calls.
		// Keep the Run observation, but never report that sum as current context.
		terminal.usage = nil
	}
	if usage == nil {
		reason := UsageReasonOf(err)
		if s.ctx.Err() != nil {
			reason = UsageCancelled
		}
		if reason == "" {
			reason = UsageMissing
		}
		_ = p.journal.MarkUnknown(id, string(reason))
		terminal.accounting = map[string]any{"state": "unknown", "record_id": id, "reason": string(reason)}
	} else {
		terminal.accounting = map[string]any{"state": "measured", "source": source, "record_id": id, "provisional": true, "usage_scope": "run"}
		if samples, ok := usage["cursor_usage_stability_samples"]; ok {
			terminal.accounting["stability_samples"] = samples
		}
	}
	terminal.accounting["internal_tool_calls"] = terminal.internalCalls
	terminal.accounting["observed_internal_steps"] = terminal.internalSteps
	terminal.accounting["journal"] = p.journal.Health()
	if source == "turn_ended" && usage != nil {
		terminal.accounting["journal_recorded"] = err == nil
	}
	if usage != nil && terminal.usage == nil {
		terminal.accounting["state"] = "context_unknown"
		terminal.accounting["reason"] = "run_counts_multiple_inference_steps"
	} else if usage != nil {
		// No visible internal tool batch is a useful guard, not evidence that
		// the upstream made exactly one model call. Do not claim finality.
		terminal.accounting["context_basis"] = "no_observed_internal_steps"
	}
	return terminal
}

func (p *pluginRuntime) reconcileUsage(s *requestScope, token, account, current string) {
	p.reconcileUsageContext(s.ctx, s, token, account, current)
}

func (p *pluginRuntime) reconcileUsageContext(parent context.Context, s *requestScope, token, account, current string) {
	p.reconcileUsageWithFetcher(parent, account, current, func(ctx context.Context, body map[string]any) (map[string]any, error) {
		return s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, body)
	})
}

func (p *pluginRuntime) reconcileUsageWithFetcher(parent context.Context, account, current string, fetch usageFetcher) {
	p.mu.Lock()
	if p.reconciling == nil {
		p.reconciling = make(map[string]bool)
	}
	if p.reconciling[account] {
		p.mu.Unlock()
		return
	}
	p.reconciling[account] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.reconciling, account)
		p.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	pending := p.journal.Reviewable(account, current)
	if len(pending) == 0 {
		return
	}
	// The journal supplies a bounded fair batch including previously measured
	// Dashboard events, whose counters may still change. All candidates receive
	// the same settling checks as the current Run, never a one-snapshot backfill.
	policy := p.usagePolicy
	policy.Wait, policy.First = 2*time.Second, 0
	var workers sync.WaitGroup
	for i, record := range pending {
		if i == 4 || ctx.Err() != nil {
			break
		}
		workers.Add(1)
		go func(record usageRecord) {
			defer workers.Done()
			_, err := p.resolveRunUsageWithFetcher(ctx, account, record.ID, usageWindow{record.Conversation, record.Model, record.Started, record.Ended}, policy, true, fetch)
			reason := UsageReasonOf(err)
			if err != nil {
				_ = p.journal.MarkUnknown(record.ID, string(reason))
			}
			if reason == UsagePermission || reason == UsageRateLimit {
				cancel()
			}
		}(record)
	}
	workers.Wait()
}
