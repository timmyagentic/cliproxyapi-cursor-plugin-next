package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const nativeFailureTTL = 10 * time.Minute
const nativeFailureCapacity = 128
const nativeDiagnosticBytes = 512

// A failed owner cannot be resumed. Retain only a bounded, sanitized explanation,
// not its history, tools, bearer, request URLs, or an upstream response body.
type nativeFailureSummary struct {
	Code, Reason, Message string
	Status                int
}

type nativeFailureRecord struct {
	Summary nativeFailureSummary
	Scope   string
	Expires time.Time
}

type nativeRecoveryError struct {
	err      error
	summary  nativeFailureSummary
	recovery bool
}

func (e *nativeRecoveryError) Error() string {
	text := fmt.Sprintf("%s [code=%s; reason=%s]", e.summary.Message, e.summary.Code, e.summary.Reason)
	if e.recovery {
		text += "; retry_same_request=false; start a new user turn with the full history; verify completed tool side effects before continuing"
	}
	return text
}

func (e *nativeRecoveryError) Unwrap() error { return e.err }

// The stable subtype is present in both Error() and the C ABI code field,
// because a host may discard structured extension fields.
func nativeErrorCode(err error) string {
	var detail *nativeRecoveryError
	if errors.As(err, &detail) {
		return detail.summary.Code
	}
	return ""
}

func nativeFailure(status int, code, reason, message string) error {
	message = boundedNativeDiagnostic(message)
	return &nativeRecoveryError{
		err:     failure(status, "%s", message),
		summary: nativeFailureSummary{Code: code, Reason: reason, Message: message, Status: status},
	}
}

func boundedNativeDiagnostic(message string) string {
	if len(message) <= nativeDiagnosticBytes {
		return message
	}
	message = message[:nativeDiagnosticBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message + "..."
}

func nativeFailureDetails(err error) nativeFailureSummary {
	var detail *nativeRecoveryError
	if errors.As(err, &detail) {
		return detail.summary
	}
	if errors.Is(err, context.Canceled) {
		return nativeFailureSummary{"cursor_native_cancelled", "caller_cancelled", "native Run was cancelled", 499}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nativeFailureSummary{"cursor_native_deadline", "caller_deadline", "native caller deadline expired", errorStatus(err)}
	}
	// Unknown error strings may include credentials, query parameters or server
	// content. Never retain or reflect them as a continuation failure summary.
	return nativeFailureSummary{"cursor_native_run_failed", "unclassified", "native Run failed; the upstream outcome may be uncertain", errorStatus(err)}
}

func nativeRecoveryRequired(err error) error {
	if err == nil {
		return nil
	}
	var detail *nativeRecoveryError
	if errors.As(err, &detail) && detail.recovery {
		return err
	}
	return &nativeRecoveryError{err: err, summary: nativeFailureDetails(err), recovery: true}
}

func nativeContinuationFailure(code, reason, message string) error {
	return nativeRecoveryRequired(nativeFailure(409, code, reason, message))
}

// An irrecoverable continuation is invalid request input, not an account fault.
// Codex 0.167 retries 410 but stops on 400; retrying cannot recreate this owner.
func nativePermanentFailure(code, reason, message string) error {
	return nativeRecoveryRequired(nativeFailure(400, code, reason, message))
}

func nativeUncertainSubmission(err error, message string) error {
	original := nativeFailureDetails(err)
	message += fmt.Sprintf("; original_failure=%s; original_reason=%s", original.Code, original.Reason)
	return preserveRetryHint(err, nativeFailure(errorStatus(err), "cursor_native_submission_uncertain", original.Reason, message))
}

// Only the local native-frame validator calls this. Its diagnostics contain
// fixed descriptions and structural counts, not upstream messages or payloads;
// transport errors are returned separately before this boundary.
func nativeProtocolValidationError(err error) error {
	if err == nil || nativeErrorCode(err) != "" {
		return err
	}
	return nativeFailure(errorStatus(err), "cursor_native_protocol_error", "invalid_frame", err.Error())
}

func nativeTransportFailure(status int, phase, reason, message string, retry bool) error {
	err := nativeFailure(status, "cursor_native_transport_error", phase+"/"+reason, message)
	if retry {
		return &transportRetryError{err: err}
	}
	return err
}

// Classify a cause without formatting it. In particular, url.Error and TLS
// errors can carry secret URLs, certificate subjects or upstream body text.
func nativeTransportReason(phase string, err error) string {
	if phase == "stream decode" {
		return "protocol_decode"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return "connection_closed"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timeout"
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) || errors.As(err, &hostname) {
		return "tls_verification"
	}
	if nativeNetworkFailure(err) {
		return "network_error"
	}
	return "transport_error"
}

func nativeOwnerKey(account, scope string) string { return digest(account + "\x00" + scope) }

func nativeHandleOwner(id string) string {
	if !nativeHandlePattern.MatchString(id) {
		return ""
	}
	return strings.Split(id, "_")[2]
}

// Called under ns.mu. A fixed cap and expiry bound both memory and retention;
// this is diagnostic state only, never a replacement execution owner.
func (ns *nativeSessions) rememberFailureLocked(identity, scope string, summary nativeFailureSummary, now time.Time) {
	if ns.failures == nil {
		ns.failures = make(map[string]nativeFailureRecord)
	}
	for id, record := range ns.failures {
		if !record.Expires.After(now) {
			delete(ns.failures, id)
		}
	}
	if _, exists := ns.failures[identity]; !exists && len(ns.failures) >= nativeFailureCapacity {
		oldest, at := "", time.Time{}
		for id, record := range ns.failures {
			if oldest == "" || record.Expires.Before(at) {
				oldest, at = id, record.Expires
			}
		}
		delete(ns.failures, oldest)
	}
	summary.Message = boundedNativeDiagnostic(summary.Message)
	ns.failures[identity] = nativeFailureRecord{Summary: summary, Scope: scope, Expires: now.Add(nativeFailureTTL)}
}

func (ns *nativeSessions) unavailableOwner(in nativeInput, ids []string) error {
	identity := nativeHandleOwner(ids[0])
	for _, id := range ids[1:] {
		if nativeHandleOwner(id) != identity {
			return nativeContinuationFailure("cursor_native_owner_unavailable", "mixed_owners", "native continuation contains handles from different owners")
		}
	}
	ns.mu.Lock()
	record, found := ns.failures[identity]
	if found && !record.Expires.After(time.Now()) {
		delete(ns.failures, identity)
		found = false
	}
	ns.mu.Unlock()
	if found && record.Scope == nativeOwnerKey(in.Account, in.OwnerScope) {
		return nativeLostOwner(record.Summary)
	}
	return nativePermanentFailure("cursor_native_owner_unavailable", "owner_not_retained", "native continuation owner is unavailable or expired (restart, expiry, or unknown owner); the original failure is not retained")
}

func nativeLostOwner(summary nativeFailureSummary) error {
	message := fmt.Sprintf("native continuation owner is unavailable or expired; original_failure=%s; original_reason=%s; original_status=%d; %s",
		summary.Code, summary.Reason, summary.Status, summary.Message)
	return nativePermanentFailure("cursor_native_owner_lost", summary.Reason, message)
}
