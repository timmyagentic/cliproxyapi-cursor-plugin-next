package main

import "time"

const (
	nativeDefaultToolResultSeconds = 120
	nativeMinToolResultSeconds     = 15
	nativeMaxToolResultSeconds     = 900
	nativeDefaultRunSeconds        = 900
	nativeMinRunSeconds            = 60
	nativeMaxRunSeconds            = 3600
)

func nativeResumeBuffer(lifetime time.Duration) time.Duration {
	return min(time.Minute, lifetime/10)
}

func (s *nativeSession) toolResultDeadline(now time.Time) time.Time {
	return minTime(now.Add(s.limits.PendingTTL), s.expires.Add(-nativeResumeBuffer(s.limits.Lifetime)))
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Called before any caller tools are published, while s.mu is held. The
// advertised deadline is also the actual admission deadline: the Run cannot
// promise a full tool wait after it enters its final processing reserve.
func (s *nativeSession) armPendingDeadlineLocked(seg *nativeSegment) error {
	now := time.Now()
	deadline := s.toolResultDeadline(now)
	if !deadline.After(now) {
		return nativePermanentFailure("cursor_native_deadline", "run_lifetime", "native Run is near its lifetime deadline; no new tool batch was delivered")
	}
	seg.deadline = deadline
	s.pendingTimer = time.AfterFunc(time.Until(deadline), func() {
		s.finishIf(nativePermanentFailure("cursor_native_deadline", "tool_result_timeout", "native tool-result deadline expired"), runEvent{}, func() bool {
			return s.state == "parked" && s.segment == seg && seg.resultDigest == ""
		})
	})
	return nil
}

func (s *nativeSession) receiptFor(seg *nativeSegment) nativeReceipt {
	receipt := s.receipt
	receipt.RunDeadline = s.expires
	receipt.ToolResultTimeoutSeconds = int(s.limits.PendingTTL / time.Second)
	receipt.PendingDeadline = func() time.Time {
		s.mu.Lock()
		defer s.mu.Unlock()
		return seg.deadline
	}
	return receipt
}

func nativeDeadlineMetadata(receipt nativeReceipt) map[string]any {
	info := map[string]any{}
	if !receipt.RunDeadline.IsZero() {
		info["run_expires_at"] = receipt.RunDeadline.UTC().Format(time.RFC3339Nano)
	}
	if receipt.ToolResultTimeoutSeconds > 0 {
		info["tool_result_timeout_seconds"] = receipt.ToolResultTimeoutSeconds
	}
	if receipt.PendingDeadline != nil {
		if deadline := receipt.PendingDeadline(); !deadline.IsZero() {
			info["tool_result_deadline"] = deadline.UTC().Format(time.RFC3339Nano)
		}
	}
	return info
}
