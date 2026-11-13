package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runViewUsage(input, cached, output int64) *usageMeasurement {
	return &usageMeasurement{PromptTokens: input, CachedTokens: cached, CompletionTokens: output, TotalTokens: input + output}
}

func assertRunViewPrice(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-12 {
		t.Fatalf("price got %v, want %.12f", got, want)
	}
}

func TestAccountingRunEstimateSampleAndContextBounds(t *testing.T) {
	usage := runViewUsage(47386, 35200, 569)
	reasoning := int64(404)
	usage.ReasoningTokens = &reasoning
	got := estimateAccountingRun("grok-4.7-high-fast", usage)
	if got.Status != "estimated" {
		t.Fatal(got)
	}
	assertRunViewPrice(t, got.LowUSD, 0.090772)
	assertRunViewPrice(t, got.HighUSD, 0.090772)
	for _, test := range []struct {
		model     string
		usage     *usageMeasurement
		status    string
		low, high float64
	}{
		{"grok-4.7", runViewUsage(100000, 20000, 5000), "estimated", .2, .2},
		{"grok-4.7-low", runViewUsage(100000, 20000, 5000), "estimated", .2, .2},
		{"grok-4.7-medium-fast", runViewUsage(100000, 20000, 5000), "estimated", .4, .4},
		{"grok-4.7-xhigh", runViewUsage(256000, 0, 1), "estimated", .512006, .512006},
		{"grok-4.7-high", runViewUsage(256001, 0, 1), "range", .512008, 1.024016},
		{"grok-4.7-fast", runViewUsage(256001, 0, 1), "range", 1.024016, 1.536024},
		{"grok-4.7-high-fast", runViewUsage(300000, 300000, 1000), "range", .312, .468},
	} {
		t.Run(fmt.Sprintf("%s/%d", test.model, test.usage.PromptTokens), func(t *testing.T) {
			got := estimateAccountingRun(test.model, test.usage)
			if got.Status != test.status || test.status == "range" && got.Reason != "per_step_context_unknown" {
				t.Fatal("cumulative Run context was treated as a measured step", got)
			}
			assertRunViewPrice(t, got.LowUSD, test.low)
			assertRunViewPrice(t, got.HighUSD, test.high)
		})
	}
}

func TestAccountingRunEstimateUnknownInvalidAndMeasuredZero(t *testing.T) {
	write := runViewUsage(100, 20, 5)
	write.CacheWriteTokens = 10
	badTotal := runViewUsage(100, 20, 5)
	badTotal.TotalTokens++
	badReasoning := runViewUsage(100, 20, 5)
	reasoning := int64(6)
	badReasoning.ReasoningTokens = &reasoning
	for _, test := range []struct {
		usage  *usageMeasurement
		reason string
	}{
		{nil, "usage_unavailable"},
		{write, "cache_write_price_unavailable"},
		{badTotal, "invalid_usage"},
		{badReasoning, "invalid_usage"},
		{runViewUsage(-1, 0, 0), "invalid_usage"},
		{runViewUsage(1, 2, 0), "invalid_usage"},
	} {
		got := estimateAccountingRun("grok-4.7", test.usage)
		if got.Status != "unavailable" || got.LowUSD != nil || got.HighUSD != nil || got.Reason != test.reason {
			t.Fatalf("unknown/invalid amount was priced: %+v", got)
		}
	}
	for _, model := range []string{"grok-4.7-none", "grok-4.7-minimal", "grok-4.7-max", "grok-4.7-high-fast-extra", "grok-4.7-fastest", "grok-4.7@500k", "cursor/grok-4.7", "unknown"} {
		if got := estimateAccountingRun(model, runViewUsage(10, 0, 1)); got.Status != "unavailable" || got.Reason != "model_price_unavailable" {
			t.Fatalf("unverified model alias %q was priced: %+v", model, got)
		}
	}
	zero := estimateAccountingRun("grok-4.7-high-fast", runViewUsage(0, 0, 0))
	if zero.Status != "estimated" {
		t.Fatal("measured zero was made unknown", zero)
	}
	assertRunViewPrice(t, zero.LowUSD, 0)
	assertRunViewPrice(t, zero.HighUSD, 0)
}

func TestAccountingRunViewDurationAndUnknownPresence(t *testing.T) {
	start := time.Unix(1700000000, 0).UTC()
	record := usageRecord{ID: digest("record"), Model: "grok-4.7-high-fast", Started: start, Ended: start.Add(20 * time.Second), Usage: runViewUsage(47386, 35200, 569)}
	row := accountingRunRowFor(record)
	if row.DurationMS == nil || *row.DurationMS != 20000 || row.ThroughputTokensPerSecond == nil || *row.ThroughputTokensPerSecond != 28.45 {
		t.Fatal("whole-Run wall-time throughput is incorrect", row)
	}
	var value map[string]any
	if err := json.Unmarshal(encodeJSON(row), &value); err != nil {
		t.Fatal(err)
	}
	if _, exists := object(value["usage"])["reasoning_tokens"]; exists {
		t.Fatal("missing reasoning was replaced with zero")
	}
	record.Usage = nil
	record.Ended = time.Time{}
	row = accountingRunRowFor(record)
	if row.DurationMS != nil || row.ThroughputTokensPerSecond != nil || row.Usage != nil || row.Ended != nil {
		t.Fatal("unfinished/unknown metrics were replaced with zero", row)
	}
	raw := string(encodeJSON(row))
	for _, want := range []string{`"usage":null`, `"duration_ms":null`, `"throughput_tokens_per_second":null`, `"low_usd":null`, `"high_usd":null`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing explicit unknown %s", want)
		}
	}
	for _, end := range []time.Time{start, start.Add(-time.Second)} {
		record.Ended = end
		if got := accountingRunRowFor(record); got.DurationMS != nil || got.ThroughputTokensPerSecond != nil {
			t.Fatal("nonpositive elapsed time became a valid throughput")
		}
	}
	record.Ended, record.Usage = start.Add(time.Second), runViewUsage(0, 0, 0)
	if got := accountingRunRowFor(record); got.ThroughputTokensPerSecond == nil || *got.ThroughputTokensPerSecond != 0 {
		t.Fatal("measured zero output over a known duration was lost")
	}
}

func TestAccountingRunsBoundedSortedRevisionAwareAndPrivate(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	start := time.Now().UTC().Add(-time.Hour)
	ids := make([]string, 201)
	for i := range ids {
		r, err := p.journal.Begin("private-account", "private-conversation", fmt.Sprintf("private-upstream-%d", i), "grok-4.7-high-fast", start.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
		if err := p.journal.Close(r.ID, r.Started.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []int64{100, 150} {
		if err := p.journal.CompleteDeferred(ids[200], journalUsage(input, 20, 0, 10, "private-event"), "dashboard"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.journal.Begin("private-account", "private-conversation", "private-upstream-200", "grok-4.7-high-fast", start); err != nil {
		t.Fatal(err)
	}
	before := p.journal.Health()
	view := p.accountingRuns()
	if view.Scope != "run" || view.ContextUsage != "unavailable" || view.Limit != 200 || view.TotalActiveRecords != 201 || len(view.Rows) != 200 || view.PricingSource != grokRunPricingSource {
		t.Fatalf("unbounded or mislabeled active view: %+v", view)
	}
	if view.Rows[0].ID != ids[200] || view.Rows[199].ID != ids[1] || view.Rows[0].Usage.PromptTokens != 150 || view.Rows[0].Status != "deferred" {
		t.Fatal("sorting/revisions created extra rows or summed an old observation")
	}
	seen := map[string]bool{}
	for _, row := range view.Rows {
		if !isUsageHash(row.ID) || seen[row.ID] {
			t.Fatal("view returned duplicate or nonopaque record identities")
		}
		seen[row.ID] = true
	}
	raw := string(encodeJSON(view))
	for _, forbidden := range []string{"private-account", "private-conversation", "private-upstream", "private-event", "account_hash", "conversation_id", "run_id", "event_id", "authorization", "synthetic-never-write-token", "synthetic-never-write-conversation"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("view exposed sensitive material %q", forbidden)
		}
	}
	view.Rows[0].Usage.PromptTokens = 999
	if p.accountingRuns().Rows[0].Usage.PromptTokens != 150 || !reflect.DeepEqual(before, p.journal.Health()) {
		t.Fatal("reading or mutating the view changed the journal")
	}
}
