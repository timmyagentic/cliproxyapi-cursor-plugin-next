package main

import (
	"regexp"
	"sort"
	"time"
)

const accountingRunViewLimit = 200
const grokRunPricingSource = "https://cursor.com/docs/models/grok-4-7"
const grokShortContextInputLimit = 256000

var pricedGrokRunModel = regexp.MustCompile(`^grok-4\.7(?:-(?:low|medium|high|xhigh))?(-fast)?$`)

type accountingRunsView struct {
	Scope              string             `json:"scope"`
	ContextUsage       string             `json:"context_usage"`
	TotalActiveRecords int                `json:"total_active_records"`
	Limit              int                `json:"limit"`
	Rows               []accountingRunRow `json:"rows"`
	Journal            usageJournalHealth `json:"journal"`
	PricingSource      string             `json:"pricing_source"`
}

type accountingRunRow struct {
	ID          string            `json:"id"`
	Model       string            `json:"model"`
	Started     time.Time         `json:"started"`
	Ended       *time.Time        `json:"ended,omitempty"`
	Status      string            `json:"status"`
	Source      string            `json:"source"`
	Provisional bool              `json:"provisional"`
	Usage       *usageMeasurement `json:"usage"`
	DurationMS  *float64          `json:"duration_ms"`
	// Whole-Run output divided by whole-Run wall time, including reasoning,
	// transport and caller-tool waits. This is not decoder or per-step TPS.
	ThroughputTokensPerSecond *float64              `json:"throughput_tokens_per_second"`
	Estimate                  accountingRunEstimate `json:"estimate"`
}

type accountingRunEstimate struct {
	Status  string   `json:"status"`
	LowUSD  *float64 `json:"low_usd"`
	HighUSD *float64 `json:"high_usd"`
	Reason  string   `json:"reason"`
}

// Only the existing bounded active view is read. Revisions of one Run already
// replace its record in Records(); observations are never extra rows or charges.
func (p *pluginRuntime) accountingRuns() accountingRunsView {
	view := accountingRunsView{
		Scope: "run", ContextUsage: "unavailable", Limit: accountingRunViewLimit,
		Rows: []accountingRunRow{}, PricingSource: grokRunPricingSource,
	}
	p.mu.Lock()
	journal := p.journal
	p.mu.Unlock()
	if journal == nil {
		view.Journal = usageJournalHealth{State: "unavailable", Reason: "uninitialized"}
		return view
	}
	records := journal.Records()
	view.TotalActiveRecords = len(records)
	view.Journal = journal.Health()
	sort.Slice(records, func(i, j int) bool {
		if records[i].Started.Equal(records[j].Started) {
			return records[i].ID < records[j].ID
		}
		return records[i].Started.After(records[j].Started)
	})
	if len(records) > accountingRunViewLimit {
		records = records[:accountingRunViewLimit]
	}
	for _, record := range records {
		view.Rows = append(view.Rows, accountingRunRowFor(record))
	}
	return view
}

func accountingRunRowFor(record usageRecord) accountingRunRow {
	row := accountingRunRow{
		ID: record.ID, Model: record.Model, Started: record.Started,
		Status: record.Status, Source: record.Source, Provisional: record.Provisional,
		Estimate: estimateAccountingRun(record.Model, record.Usage),
	}
	if record.Usage != nil && validAccountingRunUsage(*record.Usage) {
		row.Usage = cloneUsageMeasurement(record.Usage)
	}
	if !record.Ended.IsZero() {
		ended := record.Ended
		row.Ended = &ended
		if !record.Started.IsZero() && ended.After(record.Started) {
			seconds := ended.Sub(record.Started).Seconds()
			milliseconds := seconds * 1000
			row.DurationMS = &milliseconds
			if row.Usage != nil {
				throughput := float64(row.Usage.CompletionTokens) / seconds
				row.ThroughputTokensPerSecond = &throughput
			}
		}
	}
	return row
}

func validAccountingRunUsage(usage usageMeasurement) bool {
	return validUsageMeasurement(usage) && (usage.ReasoningTokens == nil || *usage.ReasoningTokens <= usage.CompletionTokens)
}

func estimateAccountingRun(model string, usage *usageMeasurement) accountingRunEstimate {
	unavailable := func(reason string) accountingRunEstimate {
		return accountingRunEstimate{Status: "unavailable", Reason: reason}
	}
	if usage == nil {
		return unavailable("usage_unavailable")
	}
	if !validAccountingRunUsage(*usage) {
		return unavailable("invalid_usage")
	}
	variant := pricedGrokRunModel.FindStringSubmatch(model)
	if variant == nil {
		return unavailable("model_price_unavailable")
	}
	if usage.CacheWriteTokens > 0 {
		return unavailable("cache_write_price_unavailable")
	}
	// Rates are half-micro-USD per token: $2/$0.5/$6 per million for
	// standard short input/cache/output, doubled for Fast or standard long.
	short, long := [3]int64{4, 1, 12}, [3]int64{8, 2, 24}
	if variant[1] != "" {
		short, long = [3]int64{8, 2, 24}, [3]int64{12, 3, 36}
	}
	price := func(rates [3]int64) float64 {
		// Cached input is already part of prompt, and reasoning is already
		// part of completion. Never bill either twice. Valid JSON-safe totals
		// keep this integer arithmetic below int64's maximum.
		halfMicroUSD := (usage.PromptTokens-usage.CachedTokens)*rates[0] + usage.CachedTokens*rates[1] + usage.CompletionTokens*rates[2]
		return float64(halfMicroUSD) / 2_000_000
	}
	low := price(short)
	estimate := accountingRunEstimate{Status: "estimated", LowUSD: &low, HighUSD: &low, Reason: "run_input_within_short_context_limit"}
	if usage.PromptTokens > grokShortContextInputLimit {
		// A cumulative Run above 256K does not prove any one inference crossed
		// the threshold. Retain both possible rate bounds rather than guessing.
		high := price(long)
		estimate.Status, estimate.HighUSD, estimate.Reason = "range", &high, "per_step_context_unknown"
	}
	return estimate
}
