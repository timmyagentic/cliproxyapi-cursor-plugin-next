package main

import (
	"context"
	"net/http"
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
	if p.journalErr != nil || p.journal == nil {
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

// A dashboard timestamp is not a request ID. Even a single returned candidate
// must be rejected when it could belong to another Run, including a cancelled
// or restarted Run. A stable conversation ID is retained for upstream caching.
func (p *pluginRuntime) unambiguousUsage(account, cid, id string, usage map[string]any) bool {
	stamp, ok := usageInteger(usage["cursor_usage_timestamp"])
	if !ok {
		return false
	}
	at := time.UnixMilli(stamp)
	for _, record := range p.journal.Records() {
		if record.ID == id || record.Account != account || record.Conversation != cid {
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
	return result
}

func (p *pluginRuntime) accountUsage(s *requestScope, token, account, id, cid, model string, started, ended time.Time, terminal runEvent) runEvent {
	_ = p.journal.SetInternalToolCalls(id, terminal.internalCalls)
	source := "turn_ended"
	usage := terminal.usage
	if usage == nil && s.ctx.Err() == nil {
		source = "dashboard"
		usage, _ = resolveDashboardUsage(s.ctx, usageWindow{cid, model, started, ended}, func(ctx context.Context, body map[string]any) (map[string]any, error) {
			return s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, body)
		}, p.usagePolicy)
		if usage != nil && !p.unambiguousUsage(account, cid, id, usage) {
			usage = nil
		}
	}
	if usage != nil {
		if err := p.journal.Complete(id, usage, source); err != nil {
			// A failed durable claim must not publish a potentially duplicate count.
			usage = nil
		}
	}
	terminal.usage = publicUsage(usage)
	if source == "dashboard" && terminal.internalCalls > 0 {
		// A Run that handled native discovery/rejected tools can contain several
		// inference steps. Its dashboard counter is a Run total, not the current
		// prompt size. Keep that billing observation only in the journal.
		terminal.usage = nil
	}
	if usage == nil {
		_ = p.journal.MarkUnknown(id, "unavailable")
		terminal.accounting = map[string]any{"state": "unknown", "record_id": id, "reason": "authoritative_usage_unavailable"}
	} else {
		terminal.accounting = map[string]any{"state": "measured", "source": source, "record_id": id}
	}
	terminal.accounting["internal_tool_calls"] = terminal.internalCalls
	if usage != nil && terminal.usage == nil {
		terminal.accounting["state"] = "context_unknown"
		terminal.accounting["reason"] = "dashboard_counts_multiple_inference_steps"
	}
	// Historical measurements remain in their own journal records. The standard
	// usage object above always describes this request, never a sum of requests.
	p.reconcileUsage(s, token, account, id)
	return terminal
}

func (p *pluginRuntime) reconcileUsage(s *requestScope, token, account, current string) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	pending := p.journal.Pending(account, current)
	if len(pending) == 0 {
		return
	}
	p.mu.Lock()
	if p.reconcileNext == nil {
		p.reconcileNext = make(map[string]int)
	}
	start := p.reconcileNext[account] % len(pending)
	p.mu.Unlock()
	for i := 0; i < len(pending) && i < 4; i++ {
		if ctx.Err() != nil {
			return
		}
		index := (start + i) % len(pending)
		record := pending[index]
		p.mu.Lock()
		p.reconcileNext[account] = (index + 1) % len(pending)
		p.mu.Unlock()
		usage, err := fetchUsagePages(ctx, usageWindow{record.Conversation, record.Model, record.Started, record.Ended}, func(ctx context.Context, body map[string]any) (map[string]any, error) {
			return s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, body)
		}, p.usagePolicy)
		if err != nil {
			if status := errorStatus(err); status == 401 || status == 403 || status == 429 {
				return
			}
			continue
		}
		if usage != nil && p.unambiguousUsage(account, record.Conversation, record.ID, usage) {
			_ = p.journal.CompleteDeferred(record.ID, usage, "dashboard")
		}
	}
}
