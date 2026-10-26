package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func usageTestWindow() usageWindow {
	since := time.UnixMilli(1_800_000_000_000)
	return usageWindow{"synthetic-conversation", "cursor/grok-4.7@256k", since, since.Add(3 * time.Second)}
}

func usageTestPolicy() stepUsagePolicy {
	return stepUsagePolicy{Wait: 150 * time.Millisecond, First: 0, Interval: time.Millisecond, Ask: 50 * time.Millisecond, Skew: time.Second, PageSize: 50, MaxPages: 3}
}

func usageTestEvent(id string, tokens map[string]any) map[string]any {
	w := usageTestWindow()
	event := map[string]any{"usageEventId": id, "conversationId": w.Conversation, "model": "grok-4.7-xhigh", "timestamp": fmt.Sprint(w.Since.Add(time.Second).UnixMilli())}
	if tokens != nil {
		event["tokenUsage"] = tokens
	}
	return event
}

func usageTestPage(events ...any) map[string]any {
	return map[string]any{"usageEventsDisplay": events, "totalUsageEventsCount": len(events)}
}

func TestDashboardUsageCounterSemantics(t *testing.T) {
	tests := []struct {
		name                       string
		tokens                     map[string]any
		input, output, read, write int64
	}{
		{"high-cache-tool-step", map[string]any{"inputTokens": 100, "outputTokens": 669, "cacheReadTokens": 140160}, 140260, 669, 140160, 0},
		{"write-cache-is-input", map[string]any{"inputTokens": "2", "outputTokens": json.Number("621"), "cacheReadTokens": 63457.0, "cacheWriteTokens": uint64(3081)}, 66540, 621, 63457, 3081},
		{"explicit-zero", map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0}, 0, 0, 0, 0},
		{"proto3-zero-scalars-omitted", map[string]any{}, 0, 0, 0, 0},
		{"uncached-zero-omitted", map[string]any{"cacheReadTokens": 4000, "outputTokens": 12}, 4000, 12, 4000, 0},
		{"price-is-not-a-token-counter", map[string]any{"inputTokens": 893, "outputTokens": 619, "cacheReadTokens": 24832, "totalCents": 1.7916}, 25725, 619, 24832, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := dashboardUsage(test.tokens)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"prompt_tokens": test.input, "completion_tokens": test.output, "total_tokens": test.input + test.output, "prompt_tokens_details": map[string]any{"cached_tokens": test.read, "cache_write_tokens": test.write}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("usage=%v want=%v", got, want)
			}
			if _, exists := got["completion_tokens_details"]; exists {
				t.Fatal("missing reasoning counter was changed into measured zero")
			}
		})
	}
	for _, reasoning := range []int{0, 33} {
		got, err := dashboardUsage(map[string]any{"outputTokens": 99, "reasoningTokens": reasoning})
		if err != nil || object(got["completion_tokens_details"])["reasoning_tokens"] != int64(reasoning) || got["completion_tokens"] != int64(99) || got["total_tokens"] != int64(99) {
			t.Fatalf("reasoning was dropped or added twice: %v %v", got, err)
		}
	}
}

func TestDashboardUsageRejectsAbsentAndInvalidMeasurements(t *testing.T) {
	if _, err := dashboardUsage(nil); err == nil {
		t.Fatal("absent tokenUsage became zero")
	}
	bad := []any{nil, -1, -1.0, 1.5, "NaN", "-2", " 2", "2.0", true, math.NaN(), math.Inf(1), uint64(1 << 53), int64(1 << 53), json.Number("1.5")}
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "reasoningTokens"} {
		for _, value := range bad {
			if _, err := dashboardUsage(map[string]any{key: value}); err == nil {
				t.Errorf("accepted invalid %s=%v (%T)", key, value, value)
			}
		}
	}
	if _, err := dashboardUsage(map[string]any{"outputTokens": 1, "reasoningTokens": 2}); err == nil {
		t.Fatal("reasoning greater than total output accepted")
	}
	if _, err := dashboardUsage(map[string]any{"inputTokens": int64(1<<53 - 1), "cacheReadTokens": 1}); err == nil {
		t.Fatal("inexact summed input accepted")
	}
	if _, err := dashboardUsage(map[string]any{"inputTokens": int64(1<<53 - 1), "outputTokens": 1}); err == nil {
		t.Fatal("inexact summed total accepted")
	}
}

func TestDashboardUsageIdentityIsSpecificAndStable(t *testing.T) {
	w := usageTestWindow()
	event := usageTestEvent("synthetic-event", map[string]any{"inputTokens": 1})
	if !usageEventMatches(event, w, time.Second) {
		t.Fatal("own event did not match")
	}
	for key, value := range map[string]any{"conversationId": "another-session", "model": "grok-4.7-unrelated", "timestamp": w.Since.Add(-time.Minute).UnixMilli()} {
		other := copyMap(event)
		other[key] = value
		if usageEventMatches(other, w, time.Second) {
			t.Errorf("matched foreign %s", key)
		}
	}
	for _, actual := range []string{"grok-4.7", "grok-4.7-high", "grok-4.7-xhigh-fast", "cursor/grok-4.7@256k"} {
		if !usageModelMatches(actual, w.Model) {
			t.Errorf("rejected known model resolution %q", actual)
		}
	}
	for _, actual := range []string{"", "grok-4.70", "grok-4.7-unrelated", "grok-4.7-high-foreign", "grok-4.7-xhigh-fastest"} {
		if usageModelMatches(actual, w.Model) {
			t.Errorf("accepted arbitrary model prefix %q", actual)
		}
	}
	if usageModelMatches("grok-4.7-high", "grok-4.7-xhigh") {
		t.Fatal("a requested exact variant matched a different effort")
	}
	id := usageEventID(event)
	if len(id) != 64 || strings.Contains(id, "synthetic-event") {
		t.Fatal("event ID was not an opaque fingerprint")
	}
	changed := copyMap(event)
	changed["tokenUsage"] = map[string]any{"inputTokens": 100}
	if usageEventID(changed) != id {
		t.Fatal("usageEventId identity changed with counter population")
	}
	changed["usageEventId"] = "another-event-at-the-same-millisecond"
	if usageEventID(changed) == id {
		t.Fatal("distinct same-millisecond events were collapsed")
	}
	delete(event, "usageEventId")
	if usageEventID(event) != usageEventID(copyMap(event)) {
		t.Fatal("fallback fingerprint is nondeterministic")
	}
}

func TestDashboardUsageWaitsForLateEventAndPreservesInternalIdentity(t *testing.T) {
	var calls int
	event := usageTestEvent("synthetic-late-event", map[string]any{"inputTokens": 100, "outputTokens": 669, "cacheReadTokens": 140160})
	w := usageTestWindow()
	got, err := resolveDashboardUsage(context.Background(), w, func(ctx context.Context, body map[string]any) (map[string]any, error) {
		calls++
		if body["teamId"] != 0 || body["page"] != 1 || body["pageSize"] != 50 || body["startDate"] != fmt.Sprint(w.Since.Add(-time.Second).UnixMilli()) || body["endDate"] != fmt.Sprint(w.Until.Add(time.Second).UnixMilli()) {
			t.Fatalf("incorrect query identity/window: %v", body)
		}
		if _, exists := ctx.Deadline(); !exists {
			t.Fatal("dashboard HTTP request has no deadline")
		}
		switch calls {
		case 1:
			return map[string]any{}, nil // empty proto3 query result
		case 2:
			return usageTestPage(usageTestEvent("synthetic-late-event", nil)), nil // not populated yet
		default:
			return usageTestPage(event), nil
		}
	}, usageTestPolicy())
	if err != nil || calls != 3 {
		t.Fatalf("late result failed: calls=%d usage=%v err=%v", calls, got, err)
	}
	if got["prompt_tokens"] != int64(140260) || got["completion_tokens"] != int64(669) || object(got["prompt_tokens_details"])["cached_tokens"] != int64(140160) || got["cursor_usage_timestamp"] != w.Since.Add(time.Second).UnixMilli() || got["cursor_usage_event_id"] != usageEventID(event) || got["cursor_usage_source"] != "dashboard" {
		t.Fatalf("lost exact usage or internal identity: %v", got)
	}
}

func TestDashboardUsagePaginationDeduplicatesWithoutSumming(t *testing.T) {
	policy := usageTestPolicy()
	policy.PageSize = 2
	event := usageTestEvent("synthetic-page-event", map[string]any{"cacheReadTokens": 4000, "outputTokens": 3})
	foreign := usageTestEvent("synthetic-foreign-event", map[string]any{"inputTokens": 99999})
	foreign["conversationId"] = "foreign-conversation"
	var pages []int
	got, err := fetchUsagePages(context.Background(), usageTestWindow(), func(_ context.Context, body map[string]any) (map[string]any, error) {
		page := body["page"].(int)
		pages = append(pages, page)
		if page == 1 {
			return map[string]any{"totalUsageEventsCount": 4, "usageEventsDisplay": []any{foreign, event}}, nil
		}
		return map[string]any{"totalUsageEventsCount": 4, "usageEventsDisplay": []any{event, foreign}}, nil
	}, policy)
	if err != nil || !reflect.DeepEqual(pages, []int{1, 2}) || got["prompt_tokens"] != int64(4000) || got["completion_tokens"] != int64(3) {
		t.Fatalf("pagination repeated or summed measurements: %v pages=%v err=%v", got, pages, err)
	}
}

func TestDashboardUsageRejectsAmbiguityAndPartialPagination(t *testing.T) {
	a := usageTestEvent("synthetic-a", map[string]any{"inputTokens": 5})
	b := usageTestEvent("synthetic-b", map[string]any{"inputTokens": 7})
	conflicting := copyMap(a)
	conflicting["tokenUsage"] = map[string]any{"inputTokens": 8}
	for name, data := range map[string]map[string]any{
		"two-distinct-same-millisecond-events": usageTestPage(a, b),
		"same-id-conflicting-measurements":     usageTestPage(a, conflicting),
		"invalid-count":                        {"usageEventsDisplay": []any{a}, "totalUsageEventsCount": -1},
		"invalid-list":                         {"usageEventsDisplay": "not-a-list"},
		"null-list":                            {"usageEventsDisplay": nil},
		"missing-page":                         {"totalUsageEventsCount": 200, "usageEventsDisplay": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := fetchUsagePages(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) { return data, nil }, usageTestPolicy())
			if !errors.Is(err, errUsageUnavailable) || got != nil {
				t.Fatalf("ambiguous/incomplete data was accepted: %v %v", got, err)
			}
		})
	}
	policy := usageTestPolicy()
	policy.MaxPages, policy.PageSize = 1, 1
	got, err := fetchUsagePages(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"totalUsageEventsCount": 2, "usageEventsDisplay": []any{a}}, nil
	}, policy)
	if got != nil || !errors.Is(err, errUsageUnavailable) || !strings.Contains(err.Error(), "pagination limit") {
		t.Fatalf("partial first-page measurement was accepted: %v %v", got, err)
	}
}

func TestDashboardUsagePermissionErrorsStopAndRedact(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				return nil, failure(status, "synthetic-sensitive-upstream-body")
			}, usageTestPolicy())
			if calls != 1 || got != nil || !errors.Is(err, errUsageUnavailable) || strings.Contains(err.Error(), "synthetic-sensitive") || !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Fatalf("permission retry/redaction failed: calls=%d got=%v err=%v", calls, got, err)
			}
		})
	}
}

func TestDashboardUsageRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				if calls < 3 {
					return nil, failure(status, "synthetic-transient")
				}
				return usageTestPage(usageTestEvent("synthetic-retry", map[string]any{"inputTokens": 22})), nil
			}, usageTestPolicy())
			if err != nil || calls != 3 || got["prompt_tokens"] != int64(22) {
				t.Fatalf("transient recovery failed: calls=%d got=%v err=%v", calls, got, err)
			}
		})
	}
}

func TestDashboardUsageCancellationAndDeadline(t *testing.T) {
	t.Run("cancel-before-request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		got, err := resolveDashboardUsage(ctx, usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) { calls++; return nil, nil }, usageTestPolicy())
		if got != nil || !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("canceled request did work: %v %v calls=%d", got, err, calls)
		}
	})
	t.Run("cancel-during-HTTP", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := resolveDashboardUsage(ctx, usageTestWindow(), func(ask context.Context, _ map[string]any) (map[string]any, error) {
				close(started)
				<-ask.Done()
				return nil, ask.Err()
			}, usageTestPolicy())
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation stalled")
		}
	})
	for _, mode := range []string{"missing-event", "HTTP-timeout", "repeated-limit"} {
		t.Run(mode, func(t *testing.T) {
			policy := usageTestPolicy()
			policy.Wait, policy.Ask = 15*time.Millisecond, 3*time.Millisecond
			got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(ask context.Context, _ map[string]any) (map[string]any, error) {
				switch mode {
				case "HTTP-timeout":
					<-ask.Done()
					return nil, ask.Err()
				case "repeated-limit":
					return nil, failure(429, "limited")
				default:
					return map[string]any{}, nil
				}
			}, policy)
			if got != nil || !errors.Is(err, errUsageUnavailable) {
				t.Fatalf("unknown usage became an estimate/zero: %v %v", got, err)
			}
		})
	}
}

func TestDashboardUsageAccountsRemainIsolatedThroughHostCallbacks(t *testing.T) {
	p := newTestRuntime(t)
	p.usagePolicy = usageTestPolicy()
	h := newMockHost(t)
	h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
		if !strings.HasSuffix(req.URL, "/aiserver.v1.DashboardService/GetFilteredUsageEvents") || req.Method != "POST" {
			return mockResponse{}, errors.New("unexpected dashboard request")
		}
		var input int
		switch req.Headers.Get("Authorization") {
		case "Bearer synthetic-account-a":
			input = 11
		case "Bearer synthetic-account-b":
			input = 22
		default:
			return mockResponse{}, errors.New("lost account identity")
		}
		return mockResponse{Status: 200, Body: encodeJSON(usageTestPage(usageTestEvent("synthetic-same-event-id", map[string]any{"inputTokens": input})))}, nil
	}
	var wait sync.WaitGroup
	for _, account := range []struct {
		token string
		input int64
	}{{"synthetic-account-a", 11}, {"synthetic-account-b", 22}} {
		account := account
		wait.Add(1)
		go func() {
			defer wait.Done()
			s := p.newScope(h.call, "synthetic-account-context")
			defer s.close()
			w := usageTestWindow()
			got, err := p.resolveStepUsage(s, account.token, w.Conversation, w.Model, w.Since, w.Until)
			if err != nil || got["prompt_tokens"] != account.input {
				t.Errorf("account crossed: got=%v err=%v", got, err)
			}
		}()
	}
	wait.Wait()
	h.assertClosed(t)
}

func TestDashboardUsageDefaultRequiresPostCloseStableSampling(t *testing.T) {
	policy := defaultStepUsagePolicy()
	if policy.Wait != 8*time.Second || policy.First < time.Second || policy.Stable != time.Second || policy.Samples != 3 {
		t.Fatalf("default lost bounded post-close stable sampling: %+v", policy)
	}
}

func TestDashboardUsageStabilityRequiresTimeAndSamples(t *testing.T) {
	policy := defaultStepUsagePolicy()
	base := time.Unix(1_800_000_000, 0)
	usage := map[string]any{"prompt_tokens": int64(3000), "completion_tokens": int64(300), "cursor_usage_event_id": "synthetic-same-event"}
	t.Run("three-samples-over-one-second", func(t *testing.T) {
		var stable usageStability
		if stable.observe(usage, base, policy) || stable.observe(usage, base.Add(500*time.Millisecond), policy) || !stable.observe(usage, base.Add(time.Second), policy) {
			t.Fatal("stable counter observations did not require three samples across one second")
		}
	})
	t.Run("three-fast-samples-are-insufficient", func(t *testing.T) {
		var stable usageStability
		for _, at := range []time.Time{base, base.Add(time.Millisecond), base.Add(2 * time.Millisecond)} {
			if stable.observe(usage, at, policy) {
				t.Fatal("sample count alone bypassed the stability interval")
			}
		}
		if !stable.observe(usage, base.Add(time.Second), policy) {
			t.Fatal("stable interval never completed")
		}
	})
	t.Run("two-slow-samples-are-insufficient", func(t *testing.T) {
		var stable usageStability
		if stable.observe(usage, base, policy) || stable.observe(usage, base.Add(2*time.Second), policy) {
			t.Fatal("elapsed time alone bypassed the minimum sample count")
		}
		if !stable.observe(usage, base.Add(3*time.Second), policy) {
			t.Fatal("third consistent sample was not accepted")
		}
	})
}

func TestDashboardUsageMutableEventResetsStability(t *testing.T) {
	policy := defaultStepUsagePolicy()
	base := time.Unix(1_800_000_000, 0)
	first := usageTestEvent("synthetic-mutable-event", map[string]any{"inputTokens": 2000, "cacheReadTokens": 1000, "outputTokens": 300})
	updated := usageTestEvent("synthetic-mutable-event", map[string]any{"inputTokens": 5000, "cacheReadTokens": 3000, "outputTokens": 500})
	firstUsage, _ := dashboardUsage(object(first["tokenUsage"]))
	updatedUsage, _ := dashboardUsage(object(updated["tokenUsage"]))
	firstUsage["cursor_usage_event_id"] = usageEventID(first)
	updatedUsage["cursor_usage_event_id"] = usageEventID(updated)
	if firstUsage["cursor_usage_event_id"] != updatedUsage["cursor_usage_event_id"] {
		t.Fatal("fixture did not reproduce one upstream event changing its counters")
	}
	var stable usageStability
	if stable.observe(firstUsage, base, policy) || stable.observe(firstUsage, base.Add(500*time.Millisecond), policy) {
		t.Fatal("first partial snapshot stabilized too early")
	}
	if stable.observe(updatedUsage, base.Add(time.Second), policy) || stable.samples != 1 || !stable.since.Equal(base.Add(time.Second)) {
		t.Fatal("changed counters retained the earlier stability window")
	}
	if stable.observe(updatedUsage, base.Add(1500*time.Millisecond), policy) || !stable.observe(updatedUsage, base.Add(2*time.Second), policy) || stable.samples != 3 {
		t.Fatal("updated counters did not establish an independent stability window")
	}
	// Even a completed stable window cannot promise immutability: a later
	// observation can still change, and must reset rather than retain "final".
	later := copyMap(updatedUsage)
	later["completion_tokens"] = int64(501)
	if stable.observe(later, base.Add(10*time.Second), policy) || stable.samples != 1 {
		t.Fatal("a late mutation was treated as an immutable final count")
	}
}

func TestDashboardUsageResolverWaitsForStableMutableEvent(t *testing.T) {
	policy := usageTestPolicy()
	policy.Stable, policy.Samples = 6*time.Millisecond, 3
	calls := 0
	got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		tokens := map[string]any{"inputTokens": 2000, "cacheReadTokens": 1000, "outputTokens": 300}
		if calls >= 3 {
			tokens = map[string]any{"inputTokens": 5000, "cacheReadTokens": 3000, "outputTokens": 500}
		}
		return usageTestPage(usageTestEvent("synthetic-mutable-event", tokens)), nil
	}, policy)
	if err != nil || calls < 5 || got["prompt_tokens"] != int64(8000) || got["completion_tokens"] != int64(500) || object(got["prompt_tokens_details"])["cached_tokens"] != int64(3000) {
		t.Fatalf("partial snapshot escaped: calls=%d got=%v err=%v", calls, got, err)
	}
	if samples, ok := got["cursor_usage_stability_samples"].(int); !ok || samples < 3 || samples != calls-2 {
		t.Fatalf("stable sample evidence lost/reset incorrectly: calls=%d got=%v", calls, got)
	}
}

func TestDashboardUsageStabilityBreaksOnMissingOrFailedSnapshot(t *testing.T) {
	for _, interrupted := range []string{"missing", "transient-error"} {
		t.Run(interrupted, func(t *testing.T) {
			policy := usageTestPolicy()
			policy.Stable, policy.Samples = 6*time.Millisecond, 3
			calls := 0
			got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				if calls == 3 {
					if interrupted == "transient-error" {
						return nil, failure(503, "synthetic-transient")
					}
					return map[string]any{}, nil
				}
				return usageTestPage(usageTestEvent("synthetic-stable", map[string]any{"inputTokens": 1})), nil
			}, policy)
			if err != nil || calls < 6 || got["prompt_tokens"] != int64(1) || got["cursor_usage_stability_samples"] != calls-3 {
				t.Fatalf("interrupted stability evidence carried through: calls=%d got=%v err=%v", calls, got, err)
			}
		})
	}
}

func TestDashboardUsageNeverPublishesUnstableSnapshotAtDeadline(t *testing.T) {
	policy := usageTestPolicy()
	policy.Wait, policy.Stable, policy.Samples = 15*time.Millisecond, 10*time.Millisecond, 3
	calls := 0
	got, err := resolveDashboardUsage(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		return usageTestPage(usageTestEvent("synthetic-never-stable", map[string]any{"inputTokens": calls})), nil
	}, policy)
	if got != nil || !errors.Is(err, errUsageUnavailable) || calls < 2 {
		t.Fatalf("unstable value became a final measurement: calls=%d got=%v err=%v", calls, got, err)
	}
}
