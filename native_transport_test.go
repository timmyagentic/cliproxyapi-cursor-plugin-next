package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func nativeTestServer(t *testing.T, h2 bool, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = h2
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func nativeTransportTestPool(t *testing.T, server *httptest.Server, proxy string) *nativeTransportPool {
	t.Helper()
	pool, err := newNativeTransportPool(proxy)
	if err != nil {
		t.Fatal(err)
	}
	trust := x509.NewCertPool()
	trust.AddCert(server.Certificate())
	pool.endpoint = server.URL
	pool.tlsConfig = &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}
	pool.heartbeatInterval = time.Hour
	t.Cleanup(pool.Close)
	return pool
}

func nativeTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func nativeTestFrame(w http.ResponseWriter, data []byte, end bool) {
	frame := connectFrame(data)
	if end {
		frame[0] = 2
	}
	_, _ = w.Write(frame)
	w.(http.Flusher).Flush()
}

func nativeTestEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/connect+proto")
	for {
		data, _, err := readConnectFrame(r.Body)
		if err != nil {
			return
		}
		if string(data) == "finish" {
			nativeTestFrame(w, []byte(`{}`), true)
			return
		}
		nativeTestFrame(w, data, false)
	}
}

func nativeTestNext(t *testing.T, ctx context.Context, run cursorRunTransport, want []byte) {
	t.Helper()
	data, end, err := run.Next(ctx)
	if err != nil || end || !bytes.Equal(data, want) {
		t.Fatalf("Next: size=%d end=%v err=%v, want size=%d", len(data), end, err, len(want))
	}
}

func TestNativeTransportH2DuplexReuseAndAccountIsolation(t *testing.T) {
	var mu sync.Mutex
	connections := map[string]string{}
	server := nativeTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent.v1.AgentService/Run" || r.ProtoMajor != 2 {
			t.Errorf("wrong native route/protocol: %s %s", r.URL.Path, r.Proto)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		connections[r.Header.Get("X-Request-Id")] = r.RemoteAddr
		mu.Unlock()
		nativeTestEcho(w, r)
	})
	pool := nativeTransportTestPool(t, server, "direct")
	ctx := nativeTestContext(t)
	for _, test := range []struct{ account, id string }{{"a", "one"}, {"a", "two"}, {"b", "three"}} {
		run, err := pool.Open(ctx, test.account, "synthetic-token", test.id, []byte("first"))
		if err != nil {
			t.Fatal(err)
		}
		if run.Protocol() != "h2" {
			t.Fatal(run.Protocol())
		}
		// A response arrives before request EOF; the same stream accepts tools.
		nativeTestNext(t, ctx, run, []byte("first"))
		large := bytes.Repeat([]byte{0, 1, 0xff, 0x80}, 2<<20)
		if err = run.Send(ctx, large); err != nil {
			t.Fatal(err)
		}
		nativeTestNext(t, ctx, run, large)
		if err = run.Send(ctx, []byte("finish")); err != nil {
			t.Fatal(err)
		}
		_, end, err := run.Next(ctx)
		if err != nil || !end {
			t.Fatalf("terminal frame: %v %v", end, err)
		}
		run.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if connections["one"] != connections["two"] || connections["one"] == connections["three"] {
		t.Fatalf("connection pool did not respect account affinity: %v", connections)
	}
}

func TestNativeTransportALPNFallbackIsSequentialAndDoesNotReplay(t *testing.T) {
	var mu sync.Mutex
	var runRequests atomic.Int32
	var messages [][]byte
	var sequence []uint64
	appends := make(chan []byte, 32)
	server := nativeTestServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent.v1.AgentService/Run":
			runRequests.Add(1)
			w.WriteHeader(500)
		case "/agent.v1.AgentService/RunSSE":
			if _, _, err := readConnectFrame(r.Body); err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case data := <-appends:
					nativeTestFrame(w, data, false)
				}
			}
		case "/aiserver.v1.BidiService/BidiAppend":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			fields, err := pbFields(raw)
			if err != nil {
				t.Error(err)
				return
			}
			data, err := hex.DecodeString(pbText(fields, 1))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			messages = append(messages, data)
			sequence = append(sequence, pbNum(fields, 3))
			mu.Unlock()
			appends <- data
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
	pool := nativeTransportTestPool(t, server, "direct")
	pool.heartbeatInterval = 10 * time.Millisecond
	ctx := nativeTestContext(t)
	run, err := pool.Open(ctx, "a", "synthetic", "fallback", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Protocol() != "h1-bidi" {
		t.Fatal(run.Protocol())
	}
	nativeTestNext(t, ctx, run, []byte("first"))
	nativeTestNext(t, ctx, run, protobuf(nil).bytes(7, nil))
	if err = run.Send(ctx, []byte("tool-result")); err != nil {
		t.Fatal(err)
	}
	for {
		data, _, err := run.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == "tool-result" {
			break
		}
	}
	large := bytes.Repeat([]byte("blob"), 2<<20)
	if err = run.Send(ctx, large); err != nil {
		t.Fatal(err)
	}
	for {
		data, _, err := run.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == len(large) {
			if !bytes.Equal(data, large) {
				t.Fatal("large append bytes changed")
			}
			break
		}
	}
	run.Close()
	if runRequests.Load() != 0 {
		t.Fatal("semantic Run was submitted before ALPN fallback")
	}
	mu.Lock()
	defer mu.Unlock()
	firstCount := 0
	for i, data := range messages {
		if string(data) == "first" {
			firstCount++
		}
		if sequence[i] != uint64(i) {
			t.Fatalf("append sequence %v", sequence)
		}
	}
	if firstCount != 1 {
		t.Fatalf("initial payload submitted %d times", firstCount)
	}
}

func TestNativeTransportNoFallbackAfterSubmittedErrorOrHeadTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(strconv.FormatBool(timeout), func(t *testing.T) {
			var submitted, fallback atomic.Int32
			server := nativeTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/agent.v1.AgentService/Run" {
					fallback.Add(1)
					w.WriteHeader(500)
					return
				}
				if _, _, err := readConnectFrame(r.Body); err != nil {
					return
				}
				submitted.Add(1)
				if timeout {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(503)
			})
			pool := nativeTransportTestPool(t, server, "direct")
			pool.responseHeadWait = 500 * time.Millisecond
			_, err := pool.Open(nativeTestContext(t), "a", "synthetic-secret", "uncertain", []byte("first"))
			if err == nil || submitted.Load() != 1 || fallback.Load() != 0 {
				t.Fatalf("unexpected retry: err=%v submitted=%d fallback=%d", err, submitted.Load(), fallback.Load())
			}
			if timeout && errorStatus(err) != 504 {
				t.Fatalf("head timeout was lost: %v", err)
			}
		})
	}
}

func TestNativeTransportCancellationUnblocksReadAndBlockedWrite(t *testing.T) {
	blocked := make(chan struct{})
	server := nativeTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := readConnectFrame(r.Body); err != nil {
			return
		}
		nativeTestFrame(w, []byte("ready"), false)
		var head [5]byte
		if _, err := io.ReadFull(r.Body, head[:]); err != nil {
			return
		}
		close(blocked)
		<-r.Context().Done()
	})
	pool := nativeTransportTestPool(t, server, "direct")
	ctx, cancel := context.WithCancel(nativeTestContext(t))
	defer cancel()
	run, err := pool.Open(ctx, "a", "synthetic", "cancel", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	nativeTestNext(t, ctx, run, []byte("ready"))
	readDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { _, _, e := run.Next(context.Background()); readDone <- e }()
	go func() { writeDone <- run.Send(ctx, bytes.Repeat([]byte("x"), 16<<20)) }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("write never reached server")
	}
	cancel()
	for _, done := range []chan error{readDone, writeDone} {
		select {
		case e := <-done:
			if !errors.Is(e, context.Canceled) {
				t.Fatalf("cancel lost: %v", e)
			}
		case <-time.After(time.Second):
			t.Fatal("pending operation survived cancellation")
		}
	}
	run.Close()
}

func TestNativeTransportOwnWriteDeadlineStopsStalledRun(t *testing.T) {
	for _, h2 := range []bool{true, false} {
		t.Run(strconv.FormatBool(h2), func(t *testing.T) {
			blocked, releaseHandler := make(chan struct{}), make(chan struct{})
			var appends, runs atomic.Int32
			server := nativeTestServer(t, h2, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "BidiAppend") {
					n := appends.Add(1)
					if n == 1 {
						_, _ = io.Copy(io.Discard, r.Body)
						w.WriteHeader(200)
						return
					}
					var one [1]byte
					if _, err := io.ReadFull(r.Body, one[:]); err != nil {
						return
					}
					if n == 2 {
						close(blocked)
					}
					select {
					case <-r.Context().Done():
					case <-releaseHandler:
					}
					return
				}
				runs.Add(1)
				if _, _, err := readConnectFrame(r.Body); err != nil {
					return
				}
				nativeTestFrame(w, []byte("ready"), false)
				if h2 {
					var header [5]byte
					if _, err := io.ReadFull(r.Body, header[:]); err != nil {
						return
					}
					close(blocked)
				}
				select {
				case <-r.Context().Done():
				case <-releaseHandler:
				}
			})
			t.Cleanup(func() { close(releaseHandler) })
			pool := nativeTransportTestPool(t, server, "direct")
			// Neither Open nor Send has a caller deadline. Only the transport's
			// shortened write-wait seam can release this post-open flow-control stall.
			ctx := context.Background()
			run, err := pool.Open(ctx, "a", "synthetic", "write-bound", []byte("first"))
			if err != nil {
				t.Fatal(err)
			}
			nativeTestNext(t, nativeTestContext(t), run, []byte("ready"))
			wire := run.(*nativeWireRun)
			wire.mu.Lock()
			wire.writeWait = 250 * time.Millisecond
			wire.mu.Unlock()
			finished := make(chan error, 1)
			go func() { finished <- run.Send(ctx, bytes.Repeat([]byte("x"), 16<<20)) }()
			select {
			case <-blocked:
			case <-time.After(2 * time.Second):
				t.Fatal("write never reached the stalled upstream")
			}
			select {
			case err = <-finished:
				if errorStatus(err) != 504 || !strings.Contains(err.Error(), "outcome uncertain") {
					t.Fatalf("transport write bound was lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Send depended on caller/lifetime cancellation")
			}
			if err = run.Send(ctx, []byte("must-not-retry")); errorStatus(err) != 504 {
				t.Fatalf("timed-out Run accepted another send: %v", err)
			}
			closed := make(chan struct{})
			go func() { run.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close did not join the timed-out writer")
			}
			if runs.Load() != 1 || !h2 && appends.Load() != 2 {
				t.Fatalf("uncertain write was replayed: runs=%d appends=%d", runs.Load(), appends.Load())
			}
		})
	}
}

func TestNativeTransportPreservesSanitizedUpstreamStatus(t *testing.T) {
	for _, mode := range []string{"head", "append", "trailer"} {
		for _, status := range []int{401, 429} {
			t.Run(mode+strconv.Itoa(status), func(t *testing.T) {
				server := nativeTestServer(t, mode != "append", func(w http.ResponseWriter, r *http.Request) {
					if mode == "append" && strings.HasSuffix(r.URL.Path, "RunSSE") {
						_, _ = io.Copy(io.Discard, r.Body)
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						return
					}
					if mode == "trailer" {
						if _, _, err := readConnectFrame(r.Body); err != nil {
							return
						}
						code := "unauthenticated"
						if status == 429 {
							code = "resource_exhausted"
						}
						nativeTestFrame(w, []byte(`{"error":{"code":"`+code+`","message":"synthetic-secret-token"}}`), true)
						return
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte("synthetic-secret-token"))
				})
				pool := nativeTransportTestPool(t, server, "direct")
				ctx := nativeTestContext(t)
				run, err := pool.Open(ctx, "a", "synthetic-secret-token", "status", []byte("first"))
				if mode == "trailer" && err == nil {
					_, _, err = run.Next(ctx)
					run.Close()
				}
				if err == nil || errorStatus(err) != status || strings.Contains(err.Error(), "synthetic-secret-token") {
					t.Fatalf("status lost or secret exposed: %v", err)
				}
			})
		}
	}
}

func nativeTestConnectProxy(t *testing.T, tlsProxy bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("proxy did not receive CONNECT")
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("proxy credentials missing")
			w.WriteHeader(407)
			return
		}
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			return
		}
		count.Add(1)
		defer client.Close()
		defer target.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(target, buffer); target.Close(); close(done) }()
		_, _ = io.Copy(client, target)
		client.Close()
		<-done
	})
	var server *httptest.Server
	if tlsProxy {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server, count
}

func TestNativeTransportExplicitHTTPAndHTTPSProxy(t *testing.T) {
	for _, secured := range []bool{false, true} {
		t.Run(strconv.FormatBool(secured), func(t *testing.T) {
			server := nativeTestServer(t, true, nativeTestEcho)
			proxy, count := nativeTestConnectProxy(t, secured)
			u, _ := url.Parse(proxy.URL)
			u.User = url.UserPassword("user", "pass")
			pool := nativeTransportTestPool(t, server, u.String())
			if secured {
				pool.tlsConfig.RootCAs.AddCert(proxy.Certificate())
			}
			run, err := pool.Open(nativeTestContext(t), "a", "synthetic-token", "proxied", []byte("first"))
			if err != nil {
				t.Fatal(err)
			}
			nativeTestNext(t, nativeTestContext(t), run, []byte("first"))
			run.Close()
			if count.Load() != 1 {
				t.Fatalf("proxy tunnel count %d", count.Load())
			}
		})
	}
}

func TestNativeTransportJSONRedirectTrustAndShutdown(t *testing.T) {
	var leaked atomic.Int32
	other := nativeTestServer(t, false, func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) })
	started := make(chan struct{})
	server := nativeTestServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["mode"] {
		case "redirect":
			http.Redirect(w, r, other.URL, 302)
		case "wait":
			close(started)
			<-r.Context().Done()
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"usageEventsDisplay": []any{}})
		}
	})
	pool := nativeTransportTestPool(t, server, "direct")
	ctx := nativeTestContext(t)
	path := "/aiserver.v1.DashboardService/GetFilteredUsageEvents"
	if _, err := pool.JSON(ctx, "a", "secret-token", path, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.JSON(ctx, "a", "secret-token", path, map[string]any{"mode": "redirect"}); err == nil {
		t.Fatal("redirect followed")
	}
	if leaked.Load() != 0 {
		t.Fatal("authorization sent to redirect target")
	}
	if _, err := pool.JSON(ctx, "a", "secret-token", "https://elsewhere.invalid", nil); err == nil {
		t.Fatal("unapproved JSON target accepted")
	}
	done := make(chan error, 1)
	go func() { _, e := pool.JSON(ctx, "a", "secret-token", path, map[string]any{"mode": "wait"}); done <- e }()
	<-started
	pool.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	untrusted, _ := newNativeTransportPool("direct")
	untrusted.endpoint = server.URL
	t.Cleanup(untrusted.Close)
	_, err := untrusted.Open(ctx, "a", "secret-token", "trust", []byte("first"))
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsanitized trust failure: %v", err)
	}
}

func TestNativeTransportLimitsAndProxyValidation(t *testing.T) {
	for _, value := range []string{"ftp://user:secret@localhost", "http://user:secret@localhost:99999", "http://localhost/path", "http://localhost?key=secret"} {
		if _, err := newNativeTransportPool(value); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe proxy validation: %v", err)
		}
	}
	pool, err := newNativeTransportPool("")
	if err != nil || pool != nil {
		t.Fatal("empty proxy enabled native transport")
	}
	server := nativeTestServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := readConnectFrame(r.Body); err != nil {
			return
		}
		var frame [5]byte
		binary.BigEndian.PutUint32(frame[1:], maxWireBytes+1)
		_, _ = w.Write(frame[:])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	pool = nativeTransportTestPool(t, server, "direct")
	ctx := nativeTestContext(t)
	run, err := pool.Open(ctx, "a", "synthetic", "oversize", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = run.Next(ctx); err == nil {
		t.Fatal("oversize frame accepted")
	}
	run.Close()
	var releases []func()
	for i := 0; i < nativePoolEntries; i++ {
		_, release, err := pool.acquire(strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err = pool.acquire("overflow"); err == nil {
		t.Fatal("unbounded connection pool")
	}
	for _, release := range releases {
		release()
	}
	if _, release, err := pool.acquire("after-eviction"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

// A real SOCKS listener checks remote DNS framing without contacting public DNS.
func TestNativeTransportSOCKS5AndSOCKS5H(t *testing.T) {
	for _, remoteDNS := range []bool{false, true} {
		t.Run(strconv.FormatBool(remoteDNS), func(t *testing.T) {
			server := nativeTestServer(t, true, nativeTestEcho)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			served := make(chan byte, 1)
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				reader := bufio.NewReader(conn)
				var greet [3]byte
				if _, e = io.ReadFull(reader, greet[:]); e != nil {
					return
				}
				_, _ = conn.Write([]byte{5, 0})
				var head [4]byte
				if _, e = io.ReadFull(reader, head[:]); e != nil {
					return
				}
				size := 4
				if head[3] == 3 {
					n, _ := reader.ReadByte()
					size = int(n)
				} else if head[3] == 4 {
					size = 16
				}
				_, _ = io.CopyN(io.Discard, reader, int64(size+2))
				target, e := net.Dial("tcp", server.Listener.Addr().String())
				if e != nil {
					return
				}
				defer target.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				served <- head[3]
				done := make(chan struct{})
				go func() { _, _ = io.Copy(target, reader); target.Close(); close(done) }()
				_, _ = io.Copy(conn, target)
				conn.Close()
				<-done
			}()
			scheme := "socks5"
			if remoteDNS {
				scheme = "socks5h"
			}
			pool := nativeTransportTestPool(t, server, scheme+"://"+listener.Addr().String())
			if remoteDNS {
				u, _ := url.Parse(server.URL)
				_, port, _ := net.SplitHostPort(u.Host)
				u.Host = net.JoinHostPort(server.Certificate().DNSNames[0], port)
				pool.endpoint = u.String()
			}
			ctx := nativeTestContext(t)
			run, err := pool.Open(ctx, "a", "synthetic", "socks", []byte("first"))
			if err != nil {
				t.Fatal(err)
			}
			nativeTestNext(t, ctx, run, []byte("first"))
			run.Close()
			kind := <-served
			if remoteDNS && kind != 3 || !remoteDNS && kind != 1 {
				t.Fatalf("SOCKS address type %d", kind)
			}
		})
	}
}
