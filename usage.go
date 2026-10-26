package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A dashboard event is a measurement, not a token estimate. In particular,
// an absent tokenUsage message never means measured zero. Within an existing
// message, Cursor's proto3 JSON omits zero-valued scalar counters.
var errUsageUnavailable = errors.New("authoritative Cursor usage unavailable")

type stepUsagePolicy struct {
	Wait, First, Interval, Ask  time.Duration
	Skew, Stable                time.Duration
	PageSize, MaxPages, Samples int
}

func defaultStepUsagePolicy() stepUsagePolicy {
	return stepUsagePolicy{Wait: 8 * time.Second, First: time.Second, Interval: 500 * time.Millisecond, Ask: 2 * time.Second, Skew: time.Second, Stable: time.Second, Samples: 3, PageSize: 50, MaxPages: 20}
}

type usageWindow struct {
	Conversation, Model string
	Since, Until        time.Time
}

type usageFetcher func(context.Context, map[string]any) (map[string]any, error)

func (p *pluginRuntime) resolveStepUsage(s *requestScope, token, cid, model string, since, until time.Time) (map[string]any, error) {
	return resolveDashboardUsage(s.ctx, usageWindow{cid, model, since, until}, func(ctx context.Context, body map[string]any) (map[string]any, error) {
		return s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetFilteredUsageEvents", token, body)
	}, p.usagePolicy)
}

// until is the Run's close time, not the polling deadline. Looking up only a
// finite Run window prevents an eventual event from a later Run being adopted.
// The caller must serialize account/conversation Runs and reject already used
// event IDs: a dashboard timestamp alone is not a globally unique request ID.
func resolveDashboardUsage(parent context.Context, window usageWindow, fetch usageFetcher, policy stepUsagePolicy) (map[string]any, error) {
	if window.Conversation == "" || window.Model == "" || window.Since.IsZero() || window.Until.Before(window.Since) {
		return nil, fmt.Errorf("%w: invalid Run identity", errUsageUnavailable)
	}
	if policy.Wait <= 0 || policy.First < 0 || policy.Ask <= 0 || policy.Interval <= 0 || policy.PageSize < 1 || policy.MaxPages < 1 || policy.Skew < 0 || policy.Stable < 0 || policy.Samples < 0 || policy.Stable > 0 && policy.Samples < 1 {
		return nil, fmt.Errorf("%w: invalid polling limits", errUsageUnavailable)
	}
	ctx, cancel := context.WithTimeout(parent, policy.Wait)
	defer cancel()
	if err := waitUsage(ctx, policy.First); err != nil {
		return nil, usageContextError(parent, err)
	}
	backoff := policy.Interval
	var stability usageStability
	for {
		usage, err := fetchUsagePages(ctx, window, fetch, policy)
		if ctx.Err() != nil {
			return nil, usageContextError(parent, ctx.Err())
		}
		if err == nil && usage != nil {
			// The same dashboard event may still be accumulating internal model
			// steps after the Run closes. Stable sampling reduces that race; the
			// API has no final marker, so it cannot prove immutable finality.
			if policy.Stable == 0 {
				return usage, nil
			}
			if stability.observe(usage, time.Now(), policy) {
				usage["cursor_usage_stability_samples"] = stability.samples
				return usage, nil
			}
		} else {
			// Only consecutive complete matching snapshots establish stability.
			// A missing event or failed query breaks that evidence window.
			stability = usageStability{}
		}
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if err != nil {
			// Account permissions cannot be fixed by polling. Never leak the
			// upstream error body, which can contain account information.
			status := errorStatus(err)
			if status >= 400 && status < 500 && status != 408 && status != 429 {
				return nil, fmt.Errorf("%w: dashboard HTTP %d", errUsageUnavailable, status)
			}
			if errors.Is(err, errUsageUnavailable) {
				return nil, err
			}
			if status == 429 || status >= 500 {
				backoff *= 2
				if backoff > 2*time.Second {
					backoff = 2 * time.Second
				}
			}
		} else {
			backoff = policy.Interval
		}
		if err := waitUsage(ctx, backoff); err != nil {
			return nil, usageContextError(parent, err)
		}
	}
}

type usageStability struct {
	fingerprint string
	since       time.Time
	samples     int
}

func (stable *usageStability) observe(usage map[string]any, at time.Time, policy stepUsagePolicy) bool {
	fingerprint := string(encodeJSON(usage))
	if stable.samples == 0 || stable.fingerprint != fingerprint {
		stable.fingerprint, stable.since, stable.samples = fingerprint, at, 1
	} else {
		stable.samples++
	}
	return stable.samples >= policy.Samples && at.Sub(stable.since) >= policy.Stable
}

func usageContextError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("%w: dashboard polling deadline (%v)", errUsageUnavailable, err)
}

func waitUsage(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func fetchUsagePages(ctx context.Context, window usageWindow, fetch usageFetcher, policy stepUsagePolicy) (map[string]any, error) {
	// Do not return a first-page candidate until all pages were inspected:
	// another page may contain a second, ambiguous event for this Run.
	found := make(map[string]map[string]any)
	candidates := make(map[string]struct{})
	for page := 1; page <= policy.MaxPages; page++ {
		ask, cancel := context.WithTimeout(ctx, policy.Ask)
		data, err := fetch(ask, map[string]any{"teamId": 0, "startDate": strconv.FormatInt(window.Since.Add(-policy.Skew).UnixMilli(), 10), "endDate": strconv.FormatInt(window.Until.Add(policy.Skew).UnixMilli(), 10), "page": page, "pageSize": policy.PageSize})
		cancel()
		if err != nil {
			return nil, err
		}
		raw, exists := data["usageEventsDisplay"]
		// Cursor's proto3 JSON uses {} for an empty query result. A null
		// list is malformed; an absent list is the default empty array.
		events, ok := raw.([]any)
		if !exists {
			events, ok = nil, true
		}
		if !ok {
			return nil, fmt.Errorf("%w: invalid dashboard event list", errUsageUnavailable)
		}
		if len(events) > policy.PageSize {
			return nil, fmt.Errorf("%w: dashboard page exceeds requested size", errUsageUnavailable)
		}
		for _, rawEvent := range events {
			event := object(rawEvent)
			if !usageEventMatches(event, window, policy.Skew) {
				continue
			}
			id := usageEventID(event)
			candidates[id] = struct{}{}
			if len(candidates) > 1 {
				return nil, fmt.Errorf("%w: multiple dashboard events match the Run", errUsageUnavailable)
			}
			tokenMessage := object(event["tokenUsage"])
			// An event may be published before its measurement is populated.
			// Poll again; do not convert an absent message into a zero count.
			if tokenMessage == nil {
				continue
			}
			usage, err := dashboardUsage(tokenMessage)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errUsageUnavailable, err)
			}
			usage["cursor_usage_source"] = "dashboard"
			usage["cursor_usage_event_id"] = id
			usage["cursor_usage_timestamp"], _ = usageInteger(event["timestamp"])
			if previous, exists := found[id]; exists && string(encodeJSON(previous)) != string(encodeJSON(usage)) {
				return nil, fmt.Errorf("%w: conflicting copies of a dashboard event", errUsageUnavailable)
			}
			found[id] = usage
		}
		if len(found) > 1 {
			return nil, fmt.Errorf("%w: multiple dashboard events match the Run", errUsageUnavailable)
		}
		done := len(events) < policy.PageSize
		if rawCount, exists := data["totalUsageEventsCount"]; exists {
			count, valid := usageInteger(rawCount)
			if !valid || count < int64((page-1)*policy.PageSize+len(events)) {
				return nil, fmt.Errorf("%w: invalid dashboard event count", errUsageUnavailable)
			}
			done = int64((page-1)*policy.PageSize+len(events)) == count
			if len(events) < policy.PageSize && !done {
				return nil, fmt.Errorf("%w: incomplete dashboard pagination", errUsageUnavailable)
			}
		}
		if done {
			for _, usage := range found {
				return usage, nil
			}
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%w: dashboard pagination limit reached", errUsageUnavailable)
}

func usageEventMatches(event map[string]any, window usageWindow, skew time.Duration) bool {
	if event == nil || str(event["conversationId"]) != window.Conversation || !usageModelMatches(str(event["model"]), window.Model) {
		return false
	}
	stamp, ok := usageInteger(event["timestamp"])
	return ok && stamp >= window.Since.Add(-skew).UnixMilli() && stamp <= window.Until.Add(skew).UnixMilli()
}

func usageModelMatches(actual, requested string) bool {
	normalize := func(model string) string {
		model = strings.TrimPrefix(model, "cursor/")
		model, _, _ = strings.Cut(model, "@")
		return model
	}
	actual, requested = normalize(actual), normalize(requested)
	if actual == "" || requested == "" {
		return false
	}
	if actual == requested {
		return true
	}
	// A base model may resolve to an effort/Fast variant in the dashboard.
	// Accept only known suffixes, never an arbitrary model-name prefix.
	for _, suffix := range []string{"-none", "-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-fast", "-none-fast", "-minimal-fast", "-low-fast", "-medium-fast", "-high-fast", "-xhigh-fast", "-max-fast"} {
		if actual == requested+suffix {
			return true
		}
	}
	return false
}

func usageEventID(event map[string]any) string {
	if id, ok := event["usageEventId"].(string); ok && id != "" {
		return digest("cursor dashboard event\x00" + id)
	}
	// Include measurement identity even when the API's optional event ID is
	// absent. Canonical map encoding is deterministic in encoding/json.
	return digest(string(encodeJSON(map[string]any{"id": event["id"], "requestId": event["requestId"], "conversationId": event["conversationId"], "timestamp": event["timestamp"], "model": event["model"], "tokenUsage": event["tokenUsage"]})))
}

// JSON counters must be nonnegative integral values representable exactly by
// every downstream JSON client. Coercing an invalid/missing value to zero would
// turn "unknown" into a false measurement.
func usageInteger(value any) (int64, bool) {
	const maxExact = int64(1<<53 - 1)
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case uint64:
		if v > uint64(maxExact) {
			return 0, false
		}
		n = int64(v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 0 || v > float64(maxExact) {
			return 0, false
		}
		n = int64(v)
	case json.Number:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	case string:
		if v == "" || strings.Trim(v, "0123456789") != "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	return n, n >= 0 && n <= maxExact
}

func dashboardUsage(tokens map[string]any) (map[string]any, error) {
	if tokens == nil {
		return nil, errors.New("dashboard tokenUsage message missing")
	}
	counts := make(map[string]int64, 5)
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"} {
		raw, exists := tokens[key]
		if !exists {
			// Verified wire behavior: zero proto3 scalar counters are omitted
			// even when their enclosing tokenUsage message is present.
			counts[key] = 0
			continue
		}
		value, ok := usageInteger(raw)
		if !ok {
			return nil, fmt.Errorf("dashboard %s counter missing or invalid", key)
		}
		counts[key] = value
	}
	input := counts["inputTokens"] + counts["cacheReadTokens"] + counts["cacheWriteTokens"]
	total := input + counts["outputTokens"]
	if input > 1<<53-1 || total > 1<<53-1 {
		return nil, errors.New("dashboard token total exceeds exact JSON range")
	}
	usage := map[string]any{"prompt_tokens": input, "completion_tokens": counts["outputTokens"], "total_tokens": total, "prompt_tokens_details": map[string]any{"cached_tokens": counts["cacheReadTokens"], "cache_write_tokens": counts["cacheWriteTokens"]}}
	if raw, exists := tokens["reasoningTokens"]; exists {
		reasoning, ok := usageInteger(raw)
		if !ok || reasoning > counts["outputTokens"] {
			return nil, errors.New("dashboard reasoningTokens counter invalid")
		}
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return usage, nil
}
