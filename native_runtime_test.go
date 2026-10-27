package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeRuntimeKeepsOneRunJournalAcrossToolRequests(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	ns, tr, opens := nativeTestPool(t)
	p.nativeSessions = ns
	ns.OnStart = p.beginNativeAccounting
	ns.OnEnd = func(r nativeReceipt, ev runEvent, err error) { p.endNativeAccounting(nil, r, ev, err) }
	next, receipt, pending := nativePark(t, ns, tr, nativeTestInput(), 1)
	if err := p.configureAccounting([]byte("transport: host\n")); errorStatus(err) != 409 {
		t.Fatal("reconfiguration must not abandon an active native tool turn")
	}
	if got := p.nativeTerminal(receipt, pending[len(pending)-1]); got.usage != nil || got.accounting["state"] != "pending" {
		t.Fatal("tool handoff must omit unknown counts without waiting for the Dashboard")
	}
	continued, resumed, err := ns.Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	tr.push(nativeTestFinal("synthetic finished"))
	result := nativeReadEvents(t, continued)
	resumed.Delivered()
	got := p.nativeTerminal(resumed, result[len(result)-1])
	if got.usage != nil || got.accounting["state"] != "context_unknown" || got.accounting["run_usage"] == nil {
		t.Fatal("cumulative Run counters must stay outside current-context usage")
	}
	if opens.Load() != 1 || len(p.journal.Records()) != 1 {
		t.Fatal("continuation created a second Run or journal entry")
	}
	r, ok := p.journal.Record(nativeRecordID(receipt))
	if !ok || r.Usage == nil || r.Ended.IsZero() {
		t.Fatal("final Run measurement missing from its original record")
	}
}

func TestNativeFinalAccountingDoesNotBlockReplyAndShutdownCancelsIt(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	entered := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer func() { close(releaseHandler); server.Close() }()
	pool, err := newNativeTransportPool("direct")
	if err != nil {
		t.Fatal(err)
	}
	certs := x509.NewCertPool()
	certs.AddCert(server.Certificate())
	pool.endpoint, pool.tlsConfig = server.URL, &tls.Config{RootCAs: certs, MinVersion: tls.VersionTLS12}
	p := newTestRuntime(t)
	p.nativePool = pool
	p.usagePolicy = defaultStepUsagePolicy()
	p.usagePolicy.First = 0
	p.usagePolicy.Wait, p.usagePolicy.Ask = 30*time.Second, 30*time.Second
	r := nativeReceipt{RunID: "synthetic-run", CID: "synthetic-conversation", RequestID: "synthetic-request", Account: "synthetic-account", ModelID: "fixture-model", Started: time.Now(), token: "synthetic-bearer"}
	if err := p.beginNativeAccounting(r); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { p.endNativeAccounting(pool, r, runEvent{stop: "stop"}, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("final reply waited for billing")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("asynchronous reconciliation did not start")
	}
	if err := p.configureAccounting([]byte("transport: host\n")); errorStatus(err) != 409 {
		t.Fatal("reconfiguration closed a transport still reconciling")
	}
	stopped := make(chan struct{})
	go func() { p.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel pending accounting")
	}
}

func TestNativeRequiresExplicitRouteAndCanReturnToHost(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	if err := p.configureAccounting(nil); err != nil || p.nativeSessions != nil {
		t.Fatal("empty config must retain CPA transport")
	}
	if err := p.configureAccounting([]byte("transport: native\n")); errorStatus(err) != 400 {
		t.Fatal("native mode inferred direct access")
	}
	if err := p.configureAccounting([]byte("transport: native\nnative-proxy-url: direct\n")); err != nil || p.nativeSessions == nil {
		t.Fatal("explicit direct native configuration failed")
	}
	if err := p.configureAccounting([]byte("transport: host\n")); err != nil || p.nativeSessions != nil {
		t.Fatal("idle native mode could not return to host transport")
	}
}

func TestNativeStreamCancellationDuringOpenDoesNotWaitForResponseHead(t *testing.T) {
	p := newTestRuntime(t)
	var calls atomic.Int32
	s := p.newScope(func(method string, raw []byte) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("synthetic host stream already cancelled")
	}, "synthetic-callback")
	defer s.close()
	ns := newNativeSessions(p.ctx, func(ctx context.Context, account, token, requestID string, first []byte) (cursorRunTransport, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer ns.Close()
	stop := s.watchNativeOpening("synthetic-stream", "synthetic-id", "fixture-model", 1, time.Millisecond)
	defer stop()
	done := make(chan error, 1)
	go func() { _, _, err := ns.Start(s.ctx, nativeTestInput()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled open succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not reach pending Open")
	}
	stop()
	if calls.Load() != 1 {
		t.Fatal("opening watcher made callbacks after cancellation")
	}
}

func TestNativeInvalidProxyDoesNotPartiallyReconfigureAccounting(t *testing.T) {
	t.Setenv("CURSOR_USAGE_JOURNAL", "")
	p := newTestRuntime(t)
	journal, policy := p.journal, p.usagePolicy
	path := filepath.Join(t.TempDir(), "must-not-create", "usage.json")
	raw := []byte("transport: native\nnative-proxy-url: unsupported://proxy\nusage-wait-ms: 30000\nusage-journal: " + path + "\n")
	if err := p.configureAccounting(raw); errorStatus(err) != 400 {
		t.Fatal("invalid transport accepted")
	}
	if p.journal != journal || p.usagePolicy != policy || p.journalPath != "" {
		t.Fatal("failed configuration changed accounting state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed configuration created a journal")
	}
}

func TestNativeDetectsOriginalResponsesEnvelopeAfterHostNormalization(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"input":[{"role":"user","content":"fixture"}]}`, "openai-response"},
		{`{"input":"fixture"}`, "openai-response"},
		{`{"messages":[],"input":"ignored"}`, "openai"},
		{`{"messages":[]}`, "openai"},
		{`{"input":null}`, "openai"},
	} {
		if got := nativeSourceFormat(rpcRequest{SourceFormat: "openai", OriginalRequest: []byte(tc.body)}); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}
