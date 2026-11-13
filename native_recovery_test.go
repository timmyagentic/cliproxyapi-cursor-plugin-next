package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNativeFirstFailureABIIsSanitizedAndClassified(t *testing.T) {
	secret := "synthetic-private-response-token"
	for _, tc := range []struct {
		phase, reason string
		cause         error
		retry         int
	}{
		{"stream read", "stream_read/unexpected_eof", &url.Error{Op: "Post", URL: "https://synthetic.invalid/?token=" + secret, Err: io.ErrUnexpectedEOF}, 2},
		{"stream read", "stream_read/eof", io.EOF, 2},
		{"open", "open/tls_verification", &url.Error{Op: "Post", URL: "https://synthetic.invalid/?token=" + secret, Err: x509.UnknownAuthorityError{}}, 0},
		{"stream decode", "stream_decode/protocol_decode", errors.New(secret), 0},
		{"open", "open/transport_error", errors.New(secret), 0},
	} {
		err := nativeRecoveryRequired(nativeSafeError(context.Background(), tc.phase, tc.cause))
		detail := pluginErrorDetail(err)
		if detail.Code != "cursor_native_transport_error" || detail.HTTPStatus != 502 || detail.RetryAfterSeconds != tc.retry || !strings.Contains(detail.Message, tc.reason) {
			t.Fatalf("lost native first-failure classification: %+v", detail)
		}
		if strings.Contains(string(errorEnvelope(err)), secret) || strings.Contains(detail.Message, "synthetic.invalid") {
			t.Fatal("ABI error leaked a URL query or upstream response body")
		}
	}
	unknown := nativeRecoveryRequired(errors.New(secret))
	if strings.Contains(unknown.Error(), secret) || nativeErrorCode(unknown) != "cursor_native_run_failed" {
		t.Fatal("unclassified native errors were reflected without sanitization")
	}
}

// Negative recovery PoC: Send records the bytes before reporting an error. The
// caller has the complete history and completed tool results, but cannot know
// whether Cursor accepted that write. Never reconstruct/replay it automatically.
func TestNativeCompletedResultsDoNotAuthorizeRebuildingAnUncertainRun(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	in := nativeTestInput()
	in.OwnerScope = "private-caller-scope"
	next, _, _ := nativePark(t, ns, tr, in, 1)
	secret := "synthetic-private-bearer"
	tr.mu.Lock()
	tr.failSend = true
	tr.failAt = 1
	tr.sendError = nativeSafeError(context.Background(), "write", &url.Error{
		Op: "Post", URL: "https://synthetic.invalid/run?token=" + secret, Err: io.ErrUnexpectedEOF,
	})
	tr.mu.Unlock()
	events, _, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	result := nativeReadEvents(t, events)
	first := result[len(result)-1].err
	if nativeErrorCode(first) != "cursor_native_submission_uncertain" || errorStatus(first) != 502 || retryAfterSeconds(first) != 2 {
		t.Fatalf("lost original status, cause or cooldown hint: %v", first)
	}
	if len(tr.snapshot()) != 1 {
		t.Fatal("PoC must represent one potentially accepted write")
	}
	for range 3 {
		_, _, err = ns.Start(context.Background(), next)
		if nativeErrorCode(err) != "cursor_native_owner_lost" || errorStatus(err) != 410 || retryAfterSeconds(err) != 0 {
			t.Fatalf("lost-owner result is not distinguishable/non-retryable: %v", err)
		}
		for _, want := range []string{"original_failure=cursor_native_submission_uncertain", "write/unexpected_eof", "retry_same_request=false", "new user turn"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("missing recovery fact %q: %v", want, err)
			}
		}
		for _, private := range []string{secret, "synthetic.invalid", "private-caller-scope", "synthetic result"} {
			if strings.Contains(err.Error(), private) || strings.Contains(first.Error(), private) {
				t.Fatal("recovery diagnostics exposed sensitive input")
			}
		}
	}
	if opens.Load() != 1 || len(tr.snapshot()) != 1 {
		t.Fatal("complete caller history was mistaken for authority to replay a write")
	}
	for _, other := range []struct{ account, scope string }{{"other-account", in.OwnerScope}, {in.Account, "other-scope"}} {
		foreign := next
		foreign.Account, foreign.OwnerScope = other.account, other.scope
		_, _, err = ns.Start(context.Background(), foreign)
		if nativeErrorCode(err) != "cursor_native_owner_unavailable" || errorStatus(err) != 410 || strings.Contains(err.Error(), "original_failure=") {
			t.Fatal("retired-owner diagnostics crossed an account/caller boundary")
		}
	}
}

func TestNativeRestartRequiresANewUserTurnEvenWithCompleteResults(t *testing.T) {
	ns, tr, _ := nativeTestPool(t)
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	if err := ns.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, replacement, opens := nativeTestPool(t)
	_, _, err := restarted.Start(context.Background(), next)
	if nativeErrorCode(err) != "cursor_native_owner_unavailable" || errorStatus(err) != 410 || !strings.Contains(err.Error(), "new user turn") || opens.Load() != 0 {
		t.Fatalf("restart silently imported stale tool results: %v", err)
	}
	// An explicit new user turn can import the known history. It is a fresh Run,
	// never a claim that the lost execution/its tool results were resumed.
	next.Chat.Messages = append(next.Chat.Messages, map[string]any{"role": "user", "content": "continue from the verified completed work"})
	next.Conversation, _ = conversation(next.Chat, next.Tools)
	events, receipt, err := restarted.Start(context.Background(), next)
	if err != nil || receipt.Resumed || opens.Load() != 1 {
		t.Fatalf("explicit recovery turn failed: %v", err)
	}
	replacement.push(nativeTestFinal("fresh turn"))
	out := nativeReadEvents(t, events)
	if out[len(out)-1].err != nil || len(replacement.snapshot()) != 0 {
		t.Fatal("new user turn resubmitted an old native tool result")
	}
}

func TestNativeFailureSummaryRetentionIsBounded(t *testing.T) {
	ns, _, _ := nativeTestPool(t)
	now := time.Now()
	summary := nativeFailureSummary{"cursor_native_transport_error", "stream_read/eof", "Native Cursor stream read failed", 502}
	ns.mu.Lock()
	for i := 0; i < nativeFailureCapacity+20; i++ {
		ns.rememberFailureLocked(fmt.Sprintf("%032x", i), "scope-hash", summary, now.Add(time.Duration(i)*time.Millisecond))
	}
	retained := len(ns.failures)
	_, oldestExists := ns.failures[fmt.Sprintf("%032x", 0)]
	ns.rememberFailureLocked("after-expiry", "scope-hash", summary, now.Add(nativeFailureTTL+time.Second))
	afterExpiry := len(ns.failures)
	ns.mu.Unlock()
	if retained != nativeFailureCapacity {
		t.Fatal("retired diagnostic capacity is not bounded")
	}
	if oldestExists {
		t.Fatal("capacity did not evict the oldest summary")
	}
	if afterExpiry != 1 {
		t.Fatal("expired summaries remained eligible for retention")
	}
	for _, content := range []string{"synthetic tool name ", "测试工具"} {
		long := nativeFailure(502, "cursor_native_protocol_error", "invalid_frame", strings.Repeat(content, 1000))
		message := nativeFailureDetails(long).Message
		if len(message) > nativeDiagnosticBytes+3 || !utf8.ValidString(message) {
			t.Fatal("one diagnostic could defeat the bounded owner-summary capacity or break UTF-8")
		}
	}
}

func TestNativeCancelledAndExpiredOwnersRetainTheirCause(t *testing.T) {
	for _, mode := range []string{"cancel", "tool_timeout"} {
		t.Run(mode, func(t *testing.T) {
			ns, tr, opens := nativeTestPool(t)
			if mode == "tool_timeout" {
				ns.limits.PendingTTL = 25 * time.Millisecond
			}
			next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
			want := "tool_result_timeout"
			if mode == "cancel" {
				ctx, cancel := context.WithCancel(context.Background())
				events, _, err := ns.Start(ctx, next)
				if err != nil {
					t.Fatal(err)
				}
				nativeWait(t, func() bool { return len(tr.snapshot()) == 2 })
				cancel()
				_ = nativeReadEvents(t, events)
				want = "caller_cancelled"
			}
			nativeWait(t, func() bool { return tr.closed.Load() == 1 && !ns.Busy() })
			_, _, err := ns.Start(context.Background(), next)
			if nativeErrorCode(err) != "cursor_native_owner_lost" || errorStatus(err) != 410 || !strings.Contains(err.Error(), want) || opens.Load() != 1 {
				t.Fatalf("lost cancellation/expiry cause: %v", err)
			}
		})
	}
}

func TestNativeCompactionErrorIsActionableWithoutChangingOwnership(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	next.Chat.Messages[0]["content"] = "private compacted history"
	_, _, err := ns.Start(context.Background(), next)
	if nativeErrorCode(err) != "cursor_native_context_changed" || errorStatus(err) != 409 || !strings.Contains(err.Error(), "new user turn") || strings.Contains(err.Error(), "private compacted history") {
		t.Fatalf("compaction mismatch lost its safe recovery contract: %v", err)
	}
	if opens.Load() != 1 || len(tr.snapshot()) != 0 || !ns.Busy() {
		t.Fatal("history validation rewrote, replaced or cancelled the original owner")
	}
}

func TestNativeParallelPolicyIsPerRequestAndReplaySensitive(t *testing.T) {
	in := nativeTestInput()
	parallel := true
	same := in
	same.Chat.ParallelToolCalls = &parallel
	if nativeContract(in) != nativeContract(same) {
		t.Fatal("omitted and explicit default parallel policy diverged")
	}
	if nativePolicyDigest(in.Chat) != nativePolicyDigest(same.Chat) {
		t.Fatal("omitted and explicit default parallel policy cannot replay")
	}
	parallel = false
	if nativeContract(in) != nativeContract(same) || nativePolicyDigest(in.Chat) == nativePolicyDigest(same.Chat) {
		t.Fatal("parallel policy must change the response policy without changing Run schemas")
	}
}

func TestNativeFinishingResponseRemains409AndCanReplayAfterDelivery(t *testing.T) {
	ns, tr, opens := nativeTestPool(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ns.OnEnd = func(nativeReceipt, runEvent, error) {
		close(entered)
		<-release
	}
	next, _, _ := nativePark(t, ns, tr, nativeTestInput(), 1)
	events, receipt, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestFinal("done once"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Run did not enter its completion hook")
	}
	if _, _, err = ns.Start(context.Background(), next); errorStatus(err) != 409 || nativeErrorCode(err) == "cursor_native_owner_lost" {
		t.Fatalf("an in-flight completion was reported permanently gone: %v", err)
	}
	once.Do(func() { close(release) })
	_ = nativeReadEvents(t, events)
	receipt.Delivered()
	replay, replayReceipt, err := ns.Start(context.Background(), next)
	if err != nil || !replayReceipt.Replay || opens.Load() != 1 {
		t.Fatalf("transient completion conflict lost its exact replay: %v", err)
	}
	_ = nativeReadEvents(t, replay)
	replayReceipt.Delivered()
}
