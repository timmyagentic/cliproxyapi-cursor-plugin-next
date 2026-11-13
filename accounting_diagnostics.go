package main

import "time"

// The management view is deliberately bounded by the journal's active records.
// It never walks archived segments or returns account, conversation or event IDs.
type accountingSummary struct {
	Scope              string                       `json:"scope"`
	Records            int                          `json:"records"`
	WithMeasurement    int                          `json:"with_measurement"`
	WithoutMeasurement int                          `json:"without_measurement"`
	StatusCounts       map[string]int               `json:"status_counts"`
	SourceCounts       map[string]int               `json:"source_counts"`
	ReasonCounts       map[string]int               `json:"reason_counts"`
	RevisedRecords     int                          `json:"revised_records"`
	ConflictedRecords  int                          `json:"conflicted_records"`
	OldestStarted      *time.Time                   `json:"oldest_started,omitempty"`
	LatestMeasurement  *accountingLatestMeasurement `json:"latest_measurement,omitempty"`
}

type accountingLatestMeasurement struct {
	ObservedAt   time.Time         `json:"observed_at"`
	Status       string            `json:"status"`
	Source       string            `json:"source"`
	Scope        string            `json:"usage_scope"`
	Provisional  bool              `json:"provisional"`
	Stable       bool              `json:"stable"`
	Revision     uint64            `json:"revision"`
	ReviewReason string            `json:"review_reason,omitempty"`
	Usage        *usageMeasurement `json:"usage"`
}

type accountingContract struct {
	MeasurementScope              string                  `json:"measurement_scope"`
	PerInferenceContext           string                  `json:"per_inference_context"`
	MissingMeansZero              bool                    `json:"missing_means_zero"`
	StableMeansFinal              bool                    `json:"stable_means_final"`
	LateUpdates                   string                  `json:"late_updates"`
	EffectiveTransport            string                  `json:"effective_transport"`
	NativeLimits                  *accountingNativeLimits `json:"native_limits,omitempty"`
	PermanentNativeHandleStatus   int                     `json:"permanent_native_handle_status"`
	PermanentNativeHandleRecovery string                  `json:"permanent_native_handle_recovery"`
}

type accountingNativeLimits struct {
	ToolResultTimeoutSeconds int `json:"tool_result_timeout_seconds"`
	RunLifetimeSeconds       int `json:"run_lifetime_seconds"`
}

type accountingManagementResponse struct {
	Version    string             `json:"version"`
	Build      pluginBuildInfo    `json:"build"`
	Journal    usageJournalHealth `json:"journal"`
	ActiveRuns int                `json:"active_runs"`
	Contract   accountingContract `json:"contract"`
	Summary    accountingSummary  `json:"summary"`
}

func (p *pluginRuntime) accountingDiagnostics() accountingManagementResponse {
	p.mu.Lock()
	journal, active := p.journal, len(p.usageActive)
	cfg, native := p.nativeConfig, p.nativeSessions != nil
	p.mu.Unlock()
	health, summary := journal.diagnosticSnapshot()
	transport := "host"
	var limits *accountingNativeLimits
	if native {
		transport = "native"
		limits = &accountingNativeLimits{ToolResultTimeoutSeconds: cfg.ToolResultTimeoutSeconds, RunLifetimeSeconds: cfg.RunLifetimeSeconds}
	}
	return accountingManagementResponse{
		Version: version, Build: currentBuildInfo(), Journal: health, ActiveRuns: active,
		Contract: accountingContract{
			MeasurementScope: "run", PerInferenceContext: "unavailable",
			MissingMeansZero: false, StableMeansFinal: false, LateUpdates: "journal_only",
			EffectiveTransport: transport, NativeLimits: limits,
			PermanentNativeHandleStatus: 400, PermanentNativeHandleRecovery: "new_user_turn_with_full_history_verify_tool_side_effects",
		},
		Summary: summary,
	}
}

func (j *usageJournal) diagnosticSnapshot() (usageJournalHealth, accountingSummary) {
	summary := accountingSummary{
		Scope: "active_journal_only", StatusCounts: map[string]int{},
		SourceCounts: map[string]int{}, ReasonCounts: map[string]int{},
	}
	if j == nil {
		return usageJournalHealth{State: "unavailable", Reason: "uninitialized"}, summary
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	summary.Records = len(j.records)
	for _, record := range j.records {
		summary.StatusCounts[record.Status]++
		if record.Reason != "" {
			summary.ReasonCounts[record.Reason]++
		}
		if summary.OldestStarted == nil || record.Started.Before(*summary.OldestStarted) {
			started := record.Started
			summary.OldestStarted = &started
		}
		if record.Revision > 1 {
			summary.RevisedRecords++
		}
		if record.Conflicts > 0 {
			summary.ConflictedRecords++
		}
		if record.Usage == nil {
			summary.WithoutMeasurement++
			continue
		}
		summary.WithMeasurement++
		summary.SourceCounts[record.Source]++
		observed := record.ObservedAt
		if observed.IsZero() {
			observed = record.Ended
		}
		if summary.LatestMeasurement == nil || !observed.Before(summary.LatestMeasurement.ObservedAt) {
			summary.LatestMeasurement = &accountingLatestMeasurement{
				ObservedAt: observed, Status: record.Status, Source: record.Source, Scope: "run",
				Provisional: record.Provisional, Stable: record.Stable, Revision: record.Revision,
				ReviewReason: record.Reason,
				Usage:        cloneUsageMeasurement(record.Usage),
			}
		}
	}
	return j.healthLocked(), summary
}
