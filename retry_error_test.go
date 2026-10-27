package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTransportRetryHintDoesNotShortenAuthQuotaOrTrustFailures(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 429, 500, 503} {
		if retryAfterSeconds(transportFailure(status, "synthetic classified error")) != 0 {
			t.Fatal("unexpected status gained a short hint")
		}
	}
	trust := &url.Error{Op: "Post", URL: "https://synthetic.invalid", Err: x509.UnknownAuthorityError{}}
	if nativeNetworkFailure(trust) || retryAfterSeconds(nativeSafeError(context.Background(), "TLS", trust)) != 0 {
		t.Fatal("certificate verification gained a transient network hint")
	}
}

func TestTransportRetryHintSurvivesExecutorEnvelopeAndStreamClose(t *testing.T) {
	err := transportFailure(502, "synthetic disconnect")
	var result struct {
		Error rpcError `json:"error"`
	}
	if json.Unmarshal(errorEnvelope(err), &result) != nil || result.Error.RetryAfterSeconds != 2 || result.Error.HTTPStatus != 502 {
		t.Fatal("RPC envelope lost transport retry hint")
	}
	p := newTestRuntime(t)
	var closed rpcError
	s := p.newScope(func(method string, raw []byte) ([]byte, error) {
		if method == "host.stream.close" {
			var fields struct {
				Detail rpcError `json:"error_detail"`
			}
			if e := json.Unmarshal(raw, &fields); e != nil {
				return nil, e
			}
			closed = fields.Detail
		}
		return hostResult(map[string]any{}), nil
	}, "synthetic-callback")
	events := make(chan runEvent)
	close(events)
	streamReply(s, "synthetic-stream", events, runEvent{err: err}, "synthetic-id", "fixture-model", 1, nil)
	if closed.RetryAfterSeconds != 2 || closed.HTTPStatus != 502 {
		t.Fatal("stream close lost transport retry hint")
	}
}

func TestTransportRetryHintPreservesStatusAndReplacement(t *testing.T) {
	for _, status := range []int{502, 504} {
		original := transportFailure(status, "synthetic outcome uncertain, cannot resubmit")
		wrapped := fmt.Errorf("outer: %w", original)
		if errorStatus(wrapped) != status || retryAfterSeconds(wrapped) != 2 {
			t.Fatalf("status/hint lost: %v", wrapped)
		}
		replacement := failure(status, "sanitized replacement")
		forwarded := preserveRetryHint(wrapped, replacement)
		if forwarded.Error() != replacement.Error() || errorStatus(forwarded) != status || retryAfterSeconds(forwarded) != 2 {
			t.Fatalf("replacement lost status/message/hint: %v", forwarded)
		}
		if preserveRetryHint(original, nil) != nil {
			t.Fatal("nil replacement became an error")
		}
		for _, cancelled := range []error{context.Canceled, context.DeadlineExceeded} {
			forwarded = preserveRetryHint(original, cancelled)
			if !errors.Is(forwarded, cancelled) || retryAfterSeconds(forwarded) != 0 {
				t.Fatalf("caller cancellation gained hint: %v", forwarded)
			}
		}
	}
	for _, status := range []int{401, 403, 429, 500, 502, 503, 504} {
		err := nativeHTTPError(status)
		if retryAfterSeconds(err) != 0 || retryAfterSeconds(preserveRetryHint(err, failure(status, "replacement"))) != 0 {
			t.Fatalf("upstream HTTP %d gained a transport hint", status)
		}
	}
}

func TestNativeTransportRetryHintLocalTLSFailures(t *testing.T) {
	for _, mode := range []string{"eof", "head-timeout", "malformed", "compressed-malformed", "401", "403", "429", "500", "502", "503", "504"} {
		t.Run(mode, func(t *testing.T) {
			server := nativeTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
				if _, _, err := readConnectFrame(r.Body); err != nil {
					return
				}
				switch mode {
				case "eof":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				case "head-timeout":
					<-r.Context().Done()
				case "malformed", "compressed-malformed":
					frame := connectFrame([]byte("bad compressed frame"))
					frame[0] = 4
					if mode == "compressed-malformed" {
						frame[0] = 1
					}
					_, _ = w.Write(frame)
					w.(http.Flusher).Flush()
				default:
					status, _ := strconv.Atoi(mode)
					w.WriteHeader(status)
				}
			})
			pool := nativeTransportTestPool(t, server, "direct")
			pool.responseHeadWait = 100 * time.Millisecond
			ctx := nativeTestContext(t)
			run, err := pool.Open(ctx, "synthetic", "synthetic", mode, []byte("first"))
			if err == nil {
				_, _, err = run.Next(ctx)
				run.Close()
			}
			wantStatus, wantHint := 502, 0
			switch mode {
			case "eof":
				wantHint = 2
			case "head-timeout":
				wantStatus, wantHint = 504, 2
			case "malformed", "compressed-malformed":
			default:
				wantStatus, _ = strconv.Atoi(mode)
			}
			if err == nil || errorStatus(err) != wantStatus || retryAfterSeconds(err) != wantHint {
				t.Fatalf("got err=%v status=%d hint=%d, want %d/%d", err, errorStatus(err), retryAfterSeconds(err), wantStatus, wantHint)
			}
		})
	}
}

func TestNativeTransportRetryHintDialAndCallerCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	pool, err := newNativeTransportPool("direct")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.endpoint = "https://" + address
	_, err = pool.Open(nativeTestContext(t), "synthetic", "synthetic", "dial", []byte("first"))
	if errorStatus(err) != 502 || retryAfterSeconds(err) != 2 {
		t.Fatalf("dial failure lost hint: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pool.Open(ctx, "synthetic", "synthetic", "cancel", []byte("first"))
	if !errors.Is(err, context.Canceled) || retryAfterSeconds(err) != 0 {
		t.Fatalf("caller cancellation gained hint: %v", err)
	}
	if retryAfterSeconds(nativeSafeError(ctx, "read", io.EOF)) != 0 {
		t.Fatal("cancelled network read gained hint")
	}
}

func TestNativeTransportRetryHintWriteFailureAndOwnDeadline(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(strconv.FormatBool(stalled), func(t *testing.T) {
			pool, err := newNativeTransportPool("direct")
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run := &nativeWireRun{pool: pool, ctx: ctx, cancel: cancel, queue: make(chan nativeWrite, 1), writeWait: 20 * time.Millisecond}
			run.write = func([]byte) error {
				if stalled {
					<-ctx.Done()
				}
				return io.ErrUnexpectedEOF
			}
			run.wg.Add(1)
			go run.writer()
			defer run.Close()
			err = run.Send(context.Background(), []byte("synthetic"))
			wantStatus := 502
			if stalled {
				wantStatus = 504
				if !strings.Contains(err.Error(), "outcome uncertain, cannot resubmit") {
					t.Fatalf("uncertain-write message lost: %v", err)
				}
			}
			if errorStatus(err) != wantStatus || retryAfterSeconds(err) != 2 {
				t.Fatalf("write failure lost status/hint: %v", err)
			}
			if next := run.Send(context.Background(), []byte("must-not-resubmit")); next != err {
				t.Fatalf("failed write accepted a new send: %v", next)
			}
		})
	}
}
