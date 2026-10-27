package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// An executor callback does not include the host's effective proxy/TLS policy.
// Empty proxy therefore means host-owned HTTP, never implicit direct access.
type nativeConfig struct {
	Mode                  string `yaml:"transport"`
	Proxy                 string `yaml:"native-proxy-url"`
	ClientContextEstimate bool   `yaml:"client-context-estimate"`
}

func parseNativeConfig(raw []byte) (nativeConfig, error) {
	var cfg nativeConfig
	if len(raw) > 64<<10 {
		return cfg, failure(400, "Invalid Cursor transport configuration")
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, failure(400, "Invalid Cursor transport configuration")
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	cfg.Proxy = strings.TrimSpace(cfg.Proxy)
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	if cfg.Mode != "auto" && cfg.Mode != "host" && cfg.Mode != "native" {
		return cfg, failure(400, "Cursor transport must be auto, native, or host")
	}
	if cfg.Mode == "native" && cfg.Proxy == "" {
		return cfg, failure(400, "Native Cursor transport requires native-proxy-url (direct or an explicit proxy URL)")
	}
	// Validate before accounting configuration can replace a journal or policy.
	// Construction only parses the route; it does not open a network connection.
	if cfg.Proxy != "" && cfg.Mode != "host" {
		pool, err := newNativeTransportPool(cfg.Proxy)
		if err != nil {
			return cfg, err
		}
		pool.Close()
	}
	return cfg, nil
}

// Called with p.mu held, while no requests or physical Runs are active.
func (p *pluginRuntime) configureNativeLocked(cfg nativeConfig) error {
	if cfg == p.nativeConfig {
		return nil
	}
	var pool *nativeTransportPool
	var sessions *nativeSessions
	if cfg.Proxy != "" && cfg.Mode != "host" {
		var err error
		pool, err = newNativeTransportPool(cfg.Proxy)
		if err != nil {
			return err
		}
		sessions = newNativeSessions(p.ctx, pool.Open)
		sessions.OnStart = func(receipt nativeReceipt) error {
			if err := p.beginNativeAccounting(receipt); err != nil {
				return err
			}
			p.startNativeLiveAccounting(receipt, func(ctx context.Context, body map[string]any) (map[string]any, error) {
				return pool.JSON(ctx, receipt.Account, receipt.token, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", body)
			}, nativeLiveUsageInterval)
			// Revisit mutable or missing older events during this Run, rather
			// than freezing the first observed Dashboard snapshot forever.
			_ = p.startNativeBackground(2*time.Second, func(ctx context.Context) {
				p.reconcileUsageWithFetcher(ctx, receipt.Account, nativeRecordID(receipt), func(ctx context.Context, body map[string]any) (map[string]any, error) {
					return pool.JSON(ctx, receipt.Account, receipt.token, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", body)
				})
			})
			return nil
		}
		sessions.OnEnd = func(receipt nativeReceipt, terminal runEvent, err error) {
			p.endNativeAccounting(pool, receipt, terminal, err)
		}
	}
	// Admission above excludes live Runs. Closed replay entries may be evicted.
	if p.nativeSessions != nil {
		_ = p.nativeSessions.Close()
	}
	if p.nativePool != nil {
		p.nativePool.Close()
	}
	p.nativeConfig, p.nativePool, p.nativeSessions = cfg, pool, sessions
	return nil
}

func nativeRecordID(receipt nativeReceipt) string {
	return digest(journalAccountHash(receipt.Account) + "\x00" + receipt.RequestID)
}

func (p *pluginRuntime) beginNativeAccounting(receipt nativeReceipt) error {
	if err := p.acquireUsageRun(receipt.Account, receipt.CID); err != nil {
		return err
	}
	// Disk failures do not discard a valid inference. The fallback ID is the
	// same deterministic record ID and journal health exposes the failure.
	_, _ = p.journal.Begin(receipt.Account, receipt.CID, receipt.RequestID, receipt.ModelID, receipt.Started)
	if receipt.RequestedModel != "" {
		_ = p.journal.SetRunMetadata(nativeRecordID(receipt), receipt.RequestedModel)
	}
	return nil
}

func (p *pluginRuntime) endNativeAccounting(pool *nativeTransportPool, receipt nativeReceipt, terminal runEvent, runErr error) {
	// Session cancellation occurs before OnEnd. Wait for the current bounded
	// observation before closing the record so late live snapshots cannot
	// overwrite terminal accounting or revive a closed Run.
	if receipt.accounting != nil && receipt.accounting.done != nil {
		<-receipt.accounting.done
	}
	id, ended := nativeRecordID(receipt), p.now()
	_ = p.journal.SetNativeTiming(id, receipt.ActiveMilliseconds, receipt.FirstOutputMilliseconds)
	_ = p.journal.SetInternalToolCalls(id, terminal.internalCalls)
	_ = p.journal.SetInternalSteps(id, terminal.internalSteps)
	_ = p.journal.Close(id, ended)
	if terminal.usage != nil {
		_ = p.journal.Complete(id, terminal.usage, "turn_ended")
	} else {
		reason := "pending_dashboard"
		if runErr != nil {
			reason = "run_interrupted"
		}
		_ = p.journal.MarkUnknown(id, reason)
	}
	// The final Run may be the last request this caller makes. Reconcile on a
	// bounded runtime-owned transport, off the reply path, without callbacks
	// from an already returned HTTP request or storing a bearer credential.
	if pool != nil && terminal.usage == nil && p.ctx.Err() == nil {
		p.mu.Lock()
		policy := p.usagePolicy
		p.mu.Unlock()
		_ = p.startNativeBackground(policy.Wait, func(ctx context.Context) {
			_, _ = p.resolveRunUsageWithFetcher(ctx, receipt.Account, id,
				usageWindow{receipt.CID, receipt.ModelID, receipt.Started, ended}, policy, true,
				func(ctx context.Context, body map[string]any) (map[string]any, error) {
					return pool.JSON(ctx, receipt.Account, receipt.token, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", body)
				})
		})
	}
	p.releaseUsageRun(receipt.Account, receipt.CID)
}

const nativeLiveUsageInterval = 10 * time.Second
const nativeLiveUsageDeadline = 2 * time.Second

// Live observations carry the same event identity and revision semantics as
// final reconciliation. They are provisional Run counters, never per-request
// current-context usage, and are collected on runtime-owned transport only.
func (p *pluginRuntime) startNativeLiveAccounting(receipt nativeReceipt, fetch usageFetcher, interval time.Duration) {
	lifecycle := receipt.accounting
	if lifecycle == nil || lifecycle.ctx == nil || lifecycle.done != nil || interval <= 0 {
		return
	}
	lifecycle.done = make(chan struct{})
	if err := p.startNativeBackground(defaultNativeLimits().Lifetime, func(ctx context.Context) {
		defer close(lifecycle.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-lifecycle.ctx.Done():
				return
			case <-ticker.C:
				ask, cancel := context.WithTimeout(lifecycle.ctx, nativeLiveUsageDeadline)
				_ = p.observeNativeLiveUsage(ask, receipt, fetch)
				cancel()
			}
		}
	}); err != nil {
		close(lifecycle.done)
	}
}

func (p *pluginRuntime) observeNativeLiveUsage(ctx context.Context, receipt nativeReceipt, fetch usageFetcher) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	policy := p.usagePolicy
	p.mu.Unlock()
	// The whole scan is limited by the caller's two-second context; any
	// incomplete pagination or ambiguous matching omits this observation.
	policy.Ask = nativeLiveUsageDeadline
	policy.MaxPages = min(policy.MaxPages, 4)
	id := nativeRecordID(receipt)
	var claimErr error
	usage, err := fetchUsagePagesWithOptions(ctx, usageWindow{receipt.CID, receipt.ModelID, receipt.Started, p.now()}, fetch, policy, UsageResolveOptions{
		ExcludeEvent: func(eventID string) bool {
			owner, err := p.journal.ClaimOwner(receipt.Account, eventID)
			if err != nil {
				claimErr = err
				return true
			}
			return owner != "" && owner != id
		},
	})
	if claimErr != nil {
		return claimErr
	}
	if err != nil || usage == nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.unambiguousUsage(receipt.Account, receipt.CID, id, usage) {
		return usageUnavailable(UsageAmbiguous, "overlapping unresolved Run")
	}
	return p.journal.ObserveLive(id, usage)
}

func (p *pluginRuntime) nativeTerminal(receipt nativeReceipt, terminal runEvent) runEvent {
	// A retained Run spans several inference steps; its totals cannot measure
	// the current context. Never proportionally split those totals across tools.
	runUsage := publicUsage(terminal.usage)
	if !receipt.Replay && terminal.stop != "tool_calls" && runUsage == nil {
		// Only the bounded active/spool state belongs on the reply path. A
		// missing record must not trigger a scan of historical journal segments.
		p.journal.mu.Lock()
		record, ok := p.journal.recordLocked(nativeRecordID(receipt))
		p.journal.mu.Unlock()
		if ok && record.Usage != nil {
			runUsage, _ = jsonObject(encodeJSON(record.Usage))
		}
	}
	terminal.usage = nil
	state := "pending"
	if terminal.stop != "tool_calls" {
		state = "unknown"
	}
	if runUsage != nil {
		state = "context_unknown"
	}
	terminal.accounting = map[string]any{
		"state": state, "usage_scope": "run", "record_id": nativeRecordID(receipt),
		"continuity": "native", "run_id": receipt.RunID, "resumed": receipt.Resumed,
		"replay": receipt.Replay, "transport": receipt.Protocol,
		"internal_tool_calls": terminal.internalCalls, "observed_internal_steps": terminal.internalSteps,
		"journal": p.journal.Health(),
	}
	p.mu.Lock()
	terminal.accounting["client_context_estimate"] = p.nativeConfig.ClientContextEstimate
	p.mu.Unlock()
	if runUsage != nil {
		terminal.accounting["run_usage"] = runUsage
	}
	return terminal
}

// Admission protects pool lifetime from concurrent reconfiguration. The scope
// retains only this request's lease; the store holds no CPA callback pointer.
func (p *pluginRuntime) nativeForRequest() (*nativeSessions, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, failure(503, "Cursor plugin is shut down")
	}
	if p.nativeSessions == nil {
		return nil, func() {}, nil
	}
	p.nativeRequests++
	return p.nativeSessions, func() {
		p.mu.Lock()
		p.nativeRequests--
		p.mu.Unlock()
	}, nil
}

const nativeHeartbeat = 5 * time.Second

func (p *pluginRuntime) startNativeBackground(wait time.Duration, work func(context.Context)) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return context.Canceled
	}
	p.nativeRequests++
	p.mu.Unlock()
	release := func() { p.mu.Lock(); p.nativeRequests--; p.mu.Unlock() }
	err := p.startStreamWorker(func() {
		defer release()
		ctx, cancel := context.WithTimeout(p.ctx, wait)
		defer cancel()
		work(ctx)
	})
	if err != nil {
		release()
	}
	return err
}

func nativeOwnerScope(chat chatRequest, req rpcRequest, cid string) string {
	if chat.PromptCacheKey != "" {
		return cid
	}
	for _, key := range []string{"canonical_session_id", "execution_session_id", "derived_session_id"} {
		if str(req.Metadata[key]) != "" {
			return cid
		}
	}
	return ""
}

func nativeSourceFormat(req rpcRequest) string {
	if req.SourceFormat == "openai-response" {
		return req.SourceFormat
	}
	// CPA 8.0.43 overwrites SourceFormat with the plugin's chosen input format
	// during prepareExecutorCall. OriginalRequest still carries the original
	// downstream envelope, so distinguish Responses without inspecting content.
	if len(req.OriginalRequest) > 0 && len(req.OriginalRequest) <= maxWireBytes {
		var original struct {
			Input    json.RawMessage `json:"input"`
			Messages json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(req.OriginalRequest, &original) == nil && len(original.Input) > 0 && string(original.Input) != "null" && len(original.Messages) == 0 {
			return "openai-response"
		}
	}
	return "openai"
}
