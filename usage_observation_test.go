package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUsageOwnershipExcludesProvenForeignClaimBeforeAmbiguity(t *testing.T) {
	previous := usageTestEvent("synthetic-owned-by-previous-run", map[string]any{"inputTokens": 123})
	current := usageTestEvent("synthetic-current-run", map[string]any{"inputTokens": 7, "cacheReadTokens": 3000, "outputTokens": 19})
	page := usageTestPage(previous, current)
	fetch := func(context.Context, map[string]any) (map[string]any, error) { return page, nil }
	w := usageTestWindow()
	if got, err := fetchUsagePages(context.Background(), w, fetch, usageTestPolicy()); got != nil || UsageReasonOf(err) != UsageAmbiguous {
		t.Fatalf("unproven overlap became a precise claim: %v %v", got, err)
	}
	opts := UsageResolveOptions{ExcludedEventIDs: map[string]bool{usageEventID(previous): true}}
	got, err := fetchUsagePagesWithOptions(context.Background(), w, fetch, usageTestPolicy(), opts)
	if err != nil || got["cursor_usage_event_id"] != usageEventID(current) || got["prompt_tokens"] != int64(3007) {
		t.Fatalf("known earlier claim blocked the current event: %v %v", got, err)
	}
	third := usageTestEvent("synthetic-another-unowned-event", map[string]any{"inputTokens": 99})
	page = usageTestPage(previous, current, third)
	if got, err = fetchUsagePagesWithOptions(context.Background(), w, fetch, usageTestPolicy(), opts); got != nil || UsageReasonOf(err) != UsageAmbiguous {
		t.Fatalf("excluding an earlier claim hid genuine ambiguity: %v %v", got, err)
	}
}

func TestUsageOwnershipCannotAuthorizeOutsideIdentityOrWindow(t *testing.T) {
	foreign := usageTestEvent("synthetic-foreign", map[string]any{"inputTokens": 100})
	foreign["conversationId"] = "synthetic-other-conversation"
	w := usageTestWindow()
	previous := usageTestEvent("synthetic-previous", map[string]any{"inputTokens": 20})
	future := usageTestEvent("synthetic-future", map[string]any{"inputTokens": 500})
	future["timestamp"] = w.Until.Add(time.Minute).UnixMilli()
	got, err := fetchUsagePagesWithOptions(context.Background(), w, func(context.Context, map[string]any) (map[string]any, error) {
		return usageTestPage(previous, foreign, future), nil
	}, usageTestPolicy(), UsageResolveOptions{ExcludedEventIDs: map[string]bool{usageEventID(previous): true}})
	if err != nil || got != nil {
		t.Fatalf("excluded ownership caused foreign data to match: %v %v", got, err)
	}
}

func TestUsageOwnershipSupportsArchivedClaimLookup(t *testing.T) {
	previous := usageTestEvent("synthetic-archived-claim", map[string]any{"inputTokens": 123})
	current := usageTestEvent("synthetic-current-claim", map[string]any{"inputTokens": 9})
	var inspected []string
	got, err := fetchUsagePagesWithOptions(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		return usageTestPage(previous, current), nil
	}, usageTestPolicy(), UsageResolveOptions{ExcludeEvent: func(eventID string) bool {
		inspected = append(inspected, eventID)
		return eventID == usageEventID(previous)
	}})
	if err != nil || got["prompt_tokens"] != int64(9) || !reflect.DeepEqual(inspected, []string{usageEventID(previous), usageEventID(current)}) {
		t.Fatalf("archived claim lookup failed: got=%v inspected=%v err=%v", got, inspected, err)
	}
}

func TestUsageObservationReportsRevisionAndSettlingWithoutFinality(t *testing.T) {
	policy := usageTestPolicy()
	policy.Stable, policy.Samples = 6*time.Millisecond, 3
	var seen []UsageObservation
	calls := 0
	got, err := resolveDashboardUsageWithOptions(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		tokens := map[string]any{"inputTokens": 2000, "cacheReadTokens": 1000, "outputTokens": 300}
		if calls >= 3 {
			tokens = map[string]any{"inputTokens": 5000, "cacheReadTokens": 3000, "outputTokens": 500}
		}
		return usageTestPage(usageTestEvent("synthetic-revision-observation", tokens)), nil
	}, policy, UsageResolveOptions{Observe: func(observation UsageObservation) { seen = append(seen, observation) }})
	if err != nil || got["prompt_tokens"] != int64(8000) || len(seen) < 6 || seen[0].Reason != UsageInitialWait {
		t.Fatalf("observation lifecycle lost: usage=%v observations=%v err=%v", got, seen, err)
	}
	var revisions int
	for _, observation := range seen {
		if observation.Revised {
			revisions++
			if observation.Reason != UsageRevised || observation.Samples != 1 || observation.Stable || observation.Usage["prompt_tokens"] != int64(8000) {
				t.Fatalf("revision retained earlier settling evidence: %+v", observation)
			}
		}
	}
	last := seen[len(seen)-1]
	if revisions != 1 || last.Reason != UsageSettled || !last.Stable || last.Samples < 3 || last.StableFor < policy.Stable || last.EventID == "" || last.Stamp == 0 {
		t.Fatalf("settling evidence incomplete: %+v revisions=%d", last, revisions)
	}
	// The stable observation intentionally has no final/context-exact assertion.
	if _, exists := last.Usage["final"]; exists {
		t.Fatal("settling was promoted to an upstream finality assertion")
	}
}

func TestUsageObservationCannotMutateResolverCounters(t *testing.T) {
	policy := usageTestPolicy()
	policy.Stable, policy.Samples = 3*time.Millisecond, 3
	got, err := resolveDashboardUsageWithOptions(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		return usageTestPage(usageTestEvent("synthetic-mutation-isolation", map[string]any{"inputTokens": 10, "cacheReadTokens": 90})), nil
	}, policy, UsageResolveOptions{Observe: func(observation UsageObservation) {
		if observation.Usage != nil {
			observation.Usage["prompt_tokens"] = int64(999)
			object(observation.Usage["prompt_tokens_details"])["cached_tokens"] = int64(999)
		}
	}})
	if err != nil || got["prompt_tokens"] != int64(100) || object(got["prompt_tokens_details"])["cached_tokens"] != int64(90) {
		t.Fatalf("observer changed the published measurement: %v %v", got, err)
	}
}

func TestUsageObservationRejectsEventIdentityReplacementAcrossPolls(t *testing.T) {
	policy := usageTestPolicy()
	policy.Stable, policy.Samples = 20*time.Millisecond, 3
	calls := 0
	var last UsageObservation
	got, err := resolveDashboardUsageWithOptions(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		id := "synthetic-first-event"
		if calls > 1 {
			id = "synthetic-second-event"
		}
		return usageTestPage(usageTestEvent(id, map[string]any{"inputTokens": 1})), nil
	}, policy, UsageResolveOptions{Observe: func(observation UsageObservation) { last = observation }})
	if got != nil || calls != 2 || UsageReasonOf(err) != UsageAmbiguous || last.Reason != UsageAmbiguous || last.Stable {
		t.Fatalf("event replacement was mistaken for count revision: %v %v %+v", got, err, last)
	}
}

func TestUsageObservationReasonsAreTypedAndSanitized(t *testing.T) {
	for _, test := range []struct {
		err  error
		want UsageReason
	}{
		{nil, ""}, {context.Canceled, UsageCancelled}, {context.DeadlineExceeded, UsageTimeout},
		{failure(401, "synthetic-private-body"), UsagePermission}, {failure(403, "synthetic-private-body"), UsagePermission},
		{failure(429, "synthetic-private-body"), UsageRateLimit}, {failure(408, "synthetic-private-body"), UsageTimeout},
		{failure(400, "synthetic-private-body"), UsageInvalid}, {errors.New("synthetic-private-body"), UsageUpstreamError},
		{usageUnavailable(UsageAmbiguous, "safe mismatch"), UsageAmbiguous},
	} {
		if got := UsageReasonOf(test.err); got != test.want {
			t.Errorf("reason=%s want=%s", got, test.want)
		}
	}
	for _, status := range []int{401, 403} {
		var seen []UsageReason
		_, err := resolveDashboardUsageWithOptions(context.Background(), usageTestWindow(), func(context.Context, map[string]any) (map[string]any, error) {
			return nil, failure(status, "synthetic-private-body")
		}, usageTestPolicy(), UsageResolveOptions{Observe: func(observation UsageObservation) { seen = append(seen, observation.Reason) }})
		if UsageReasonOf(err) != UsagePermission || strings.Contains(err.Error(), "synthetic-private") || !reflect.DeepEqual(seen, []UsageReason{UsageInitialWait, UsagePermission}) {
			t.Fatalf("permission reason was lost or leaked: %v %v", seen, err)
		}
	}
}
