package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cursorRunTransport interface {
	Send(context.Context, []byte) error
	Next(context.Context) ([]byte, bool, error)
	Close() error
	Protocol() string
}

const nativePoolEntries = 16
const nativeOutboundMessages = 32
const nativeProxyHeaderBytes = 64 << 10
const nativeHandshakeWait = 15 * time.Second
const nativeWriteWait = 30 * time.Second

var errNativeNoH2ALPN = errors.New("Cursor native HTTP/2 ALPN unavailable before request submission")
var errNativeHeadTimeout = transportFailure(504, "Native Cursor response header deadline exceeded")
var errNativeWriteTimeout = transportFailure(504, "Native Cursor write deadline exceeded; outcome uncertain, cannot resubmit")

// The pool contains clients, never request headers or host callbacks. Endpoint
// and trust injection are package-local test seams, not operator TLS bypasses.
type nativeTransportPool struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	closed            bool
	closeDone         chan struct{}
	proxy             *url.URL
	endpoint          string
	tlsConfig         *tls.Config
	heartbeatInterval time.Duration
	responseHeadWait  time.Duration
	entries           map[[32]byte]*nativeTransportEntry
	runs              map[*nativeWireRun]struct{}
	clock             uint64
	ops               sync.WaitGroup
}

type nativeTransportEntry struct {
	h2, h1  *http.Client
	refs    int
	used    uint64
	h1Until atomic.Int64
}

func newNativeTransportPool(proxyURL string) (*nativeTransportPool, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil // The operator has not opted into runtime-owned transport.
	}
	var proxy *url.URL
	if proxyURL != "direct" {
		var err error
		proxy, err = url.Parse(proxyURL)
		if err != nil || proxy.Hostname() == "" || proxy.Opaque != "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
			return nil, failure(400, "Invalid native proxy URL")
		}
		switch proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, failure(400, "Unsupported native proxy scheme")
		}
		if proxy.Port() != "" {
			port, e := strconv.Atoi(proxy.Port())
			if e != nil || port < 1 || port > 65535 {
				return nil, failure(400, "Invalid native proxy port")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &nativeTransportPool{ctx: ctx, cancel: cancel, closeDone: make(chan struct{}), proxy: proxy, endpoint: cursorAPI,
		heartbeatInterval: 5 * time.Second, responseHeadWait: 15 * time.Second, entries: make(map[[32]byte]*nativeTransportEntry), runs: make(map[*nativeWireRun]struct{})}, nil
}

func (p *nativeTransportPool) acquire(account string) (*nativeTransportEntry, func(), error) {
	if p == nil {
		return nil, nil, failure(503, "Native Cursor transport is not enabled")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, context.Canceled
	}
	endpoint, err := url.Parse(p.endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, nil, failure(400, "Invalid native Cursor endpoint")
	}
	proxyKey := "direct"
	if p.proxy != nil {
		proxyKey = p.proxy.String()
	}
	key := sha256.Sum256([]byte(account + "\x00" + proxyKey + "\x00" + p.endpoint))
	entry := p.entries[key]
	if entry == nil {
		if len(p.entries) >= nativePoolEntries {
			var oldest [32]byte
			var selected *nativeTransportEntry
			for k, e := range p.entries {
				if e.refs == 0 && (selected == nil || e.used < selected.used) {
					oldest, selected = k, e
				}
			}
			if selected == nil {
				return nil, nil, failure(503, "Native Cursor connection pool is full")
			}
			selected.closeIdle()
			delete(p.entries, oldest)
		}
		entry = &nativeTransportEntry{h2: p.client(true), h1: p.client(false)}
		p.entries[key] = entry
	}
	p.clock++
	entry.used, entry.refs = p.clock, entry.refs+1
	p.ops.Add(1)
	var once sync.Once
	return entry, func() { once.Do(func() { p.mu.Lock(); entry.refs--; p.mu.Unlock(); p.ops.Done() }) }, nil
}

func (e *nativeTransportEntry) closeIdle() {
	e.h1.CloseIdleConnections()
	e.h2.CloseIdleConnections()
}

func (p *nativeTransportPool) client(h2 bool) *http.Client {
	trust := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.tlsConfig != nil {
		trust = p.tlsConfig.Clone()
	}
	transport := &http.Transport{ForceAttemptHTTP2: h2, DisableCompression: true, MaxConnsPerHost: 16,
		MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
		MaxResponseHeaderBytes: nativeProxyHeaderBytes, ResponseHeaderTimeout: 15 * time.Second}
	if !h2 {
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, nativeHandshakeWait)
		defer cancel()
		conn, err := p.dial(ctx, address, trust)
		if err != nil {
			return nil, err
		}
		cfg := trust.Clone()
		cfg.ServerName, _, err = net.SplitHostPort(address)
		if err != nil {
			conn.Close()
			return nil, errors.New("invalid native target")
		}
		cfg.NextProtos = []string{"http/1.1"}
		if h2 {
			cfg.NextProtos = []string{"h2", "http/1.1"}
		}
		secured := tls.Client(conn, cfg)
		if err = secured.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		if h2 && secured.ConnectionState().NegotiatedProtocol != "h2" {
			secured.Close()
			// No HTTP headers or semantic body can have been sent on this socket.
			return nil, errNativeNoH2ALPN
		}
		return secured, nil
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (p *nativeTransportPool) Open(ctx context.Context, account, token, requestID string, first []byte) (cursorRunTransport, error) {
	if len(first) > maxWireBytes {
		return nil, failure(413, "Native Cursor initial message exceeds limit")
	}
	entry, release, err := p.acquire(account)
	if err != nil {
		return nil, err
	}
	if entry.h1Until.Load() <= time.Now().UnixNano() {
		run, fallback, errOpen := p.open(ctx, entry, token, requestID, first, release, true)
		if !fallback {
			if errOpen != nil {
				return nil, errOpen
			}
			return run, errOpen
		}
		// Only the dedicated ALPN-before-submission sentinel admits fallback.
		entry.h1Until.Store(time.Now().Add(10 * time.Minute).UnixNano())
		entry, release, err = p.acquire(account)
		if err != nil {
			return nil, err
		}
	}
	run, _, err := p.open(ctx, entry, token, requestID, first, release, false)
	if err != nil {
		return nil, err
	}
	return run, err
}

type nativeWrite struct {
	data []byte
	done chan error
}

type nativeWireRun struct {
	pool      *nativeTransportPool
	ctx       context.Context
	cancel    context.CancelFunc
	stopPool  func() bool
	mu        sync.Mutex
	closed    bool
	err       error
	body      io.ReadCloser
	pipe      *io.PipeWriter
	write     func([]byte) error
	queue     chan nativeWrite
	queued    int
	readGate  chan struct{}
	wg        sync.WaitGroup
	release   func()
	protocol  string
	writeWait time.Duration
}

func nativeSafeError(ctx context.Context, phase string, cause ...error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cause) > 0 && nativeNetworkFailure(cause[0]) {
		return transportFailure(502, "Native Cursor "+phase+" failed")
	}
	return failure(502, "Native Cursor %s failed", phase)
}

func (p *nativeTransportPool) open(ctx context.Context, entry *nativeTransportEntry, token, requestID string, first []byte, release func(), h2 bool) (*nativeWireRun, bool, error) {
	if ctx.Err() != nil {
		release()
		return nil, false, ctx.Err()
	}
	runCtx, cancel := context.WithCancel(ctx)
	t := &nativeWireRun{pool: p, ctx: runCtx, cancel: cancel, release: release, queue: make(chan nativeWrite, nativeOutboundMessages), readGate: make(chan struct{}, 1), protocol: "h1-bidi", writeWait: nativeWriteWait}
	t.stopPool = context.AfterFunc(p.ctx, cancel)
	client := entry.h1
	path := "/agent.v1.AgentService/RunSSE"
	var body io.Reader = bytes.NewReader(connectFrame(protobuf(nil).text(1, requestID)))
	h := headers(token)
	h.Set("Content-Type", "application/connect+proto")
	h.Set("Accept-Encoding", "identity")
	h.Set("X-Cursor-Streaming", "true")
	h.Set("X-Request-Id", requestID)
	h.Set("X-Cursor-Agent-Allowed-Tools", "mcp_tool_call,get_mcp_tools_tool_call")
	if h2 {
		t.protocol = "h2"
		client, path = entry.h2, "/agent.v1.AgentService/Run"
		reader, writer := io.Pipe()
		body, t.pipe = reader, writer
		t.write = func(data []byte) error {
			var frame [5]byte
			binary.BigEndian.PutUint32(frame[1:], uint32(len(data)))
			if err := nativeWriteAll(writer, frame[:]); err != nil {
				return err
			}
			return nativeWriteAll(writer, data)
		}
	} else {
		var sequence uint64
		t.write = func(data []byte) error {
			prefix := binary.AppendUvarint([]byte{10}, uint64(len(data))*2)
			suffix := protobuf(nil).bytes(2, protobuf(nil).text(1, requestID)).uint(3, sequence)
			sequence++
			appendBody := io.MultiReader(bytes.NewReader(prefix), &nativeHexReader{data: data}, bytes.NewReader(suffix))
			req, err := http.NewRequestWithContext(runCtx, http.MethodPost, p.endpoint+"/aiserver.v1.BidiService/BidiAppend", appendBody)
			if err != nil {
				return nativeSafeError(ctx, "append request")
			}
			req.GetBody = nil
			req.ContentLength = int64(len(prefix) + len(data)*2 + len(suffix))
			req.Header = h.Clone()
			req.Header.Set("Content-Type", "application/proto")
			resp, err := client.Do(req)
			if err != nil {
				return nativeSafeError(runCtx, "append", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nativeHTTPError(resp.StatusCode)
			}
			n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxWireBytes+1))
			if err != nil || n > maxWireBytes {
				return nativeSafeError(runCtx, "append response", err)
			}
			return nil
		}
	}
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, p.endpoint+path, body)
	if err != nil {
		t.Close()
		return nil, false, nativeSafeError(ctx, "request")
	}
	req.Header, req.GetBody = h, nil
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		t.Close()
		return nil, false, context.Canceled
	}
	t.wg.Add(3)
	p.runs[t] = struct{}{}
	p.mu.Unlock()
	go func() { defer t.wg.Done(); <-runCtx.Done(); t.shutdown(runCtx.Err()) }()
	go t.writer()
	go t.heartbeat(p.heartbeatInterval)
	type opened struct {
		response *http.Response
		err      error
	}
	ready := make(chan opened, 1)
	headTimer := time.AfterFunc(p.responseHeadWait, func() { t.shutdown(errNativeHeadTimeout) })
	go func() {
		defer headTimer.Stop()
		resp, e := client.Do(req)
		if e != nil {
			t.shutdown(nativeSafeError(ctx, "open", e))
		} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.shutdown(nativeHTTPError(resp.StatusCode))
			resp.Body.Close()
		} else {
			t.mu.Lock()
			if t.closed {
				resp.Body.Close()
			} else {
				t.body = resp.Body
			}
			t.mu.Unlock()
		}
		ready <- opened{resp, e}
	}()
	firstErr := t.Send(ctx, first)
	result := <-ready
	if result.err != nil {
		t.Close()
		fallback := h2 && errors.Is(result.err, errNativeNoH2ALPN) && !errors.Is(t.failure(), errNativeHeadTimeout) && ctx.Err() == nil && p.ctx.Err() == nil
		return nil, fallback, t.failure()
	}
	if result.response.StatusCode < 200 || result.response.StatusCode >= 300 {
		t.Close()
		return nil, false, nativeHTTPError(result.response.StatusCode)
	}
	if firstErr != nil {
		t.Close()
		return nil, false, firstErr
	}
	return t, false, nil
}

func nativeHTTPError(status int) error {
	if status < 400 || status > 599 {
		status = 502
	}
	return failure(status, "Native Cursor upstream returned HTTP %d", status)
}

func (t *nativeWireRun) Protocol() string { return t.protocol }

func (t *nativeWireRun) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return t.err
	}
	if t.ctx.Err() != nil {
		return t.ctx.Err()
	}
	return context.Canceled
}

func (t *nativeWireRun) Send(ctx context.Context, data []byte) error {
	if ctx.Err() != nil {
		t.shutdown(ctx.Err())
		return ctx.Err()
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return t.failure()
	}
	if len(data) > maxWireBytes || t.queued+len(data) > maxWireBytes || len(t.queue) >= nativeOutboundMessages-1 {
		t.mu.Unlock()
		return failure(503, "Native Cursor outbound queue exceeds limit")
	}
	message := nativeWrite{data: bytes.Clone(data), done: make(chan error, 1)}
	wait := t.writeWait
	if wait <= 0 {
		wait = nativeWriteWait
	}
	t.queued += len(data)
	t.queue <- message
	t.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-message.done:
		return err
	case <-ctx.Done():
		t.shutdown(ctx.Err())
		return ctx.Err()
	case <-timer.C:
		// The message may already be partly submitted. Cancel the whole Run,
		// including its request body/append, instead of permitting another send.
		t.shutdown(errNativeWriteTimeout)
		return t.failure()
	case <-t.ctx.Done():
		return t.failure()
	}
}

func (t *nativeWireRun) writer() {
	defer t.wg.Done()
	defer func() {
		for {
			select {
			case message := <-t.queue:
				t.finishWrite(message, t.failure())
			default:
				t.mu.Lock()
				t.write = nil
				t.mu.Unlock()
				return
			}
		}
	}()
	for {
		select {
		case <-t.ctx.Done():
			return
		case message := <-t.queue:
			err := t.write(message.data)
			if err != nil {
				// Append status errors are already sanitized; retain 401/429 so
				// account selection does not mistake them for transport failures.
				var status *statusError
				if !errors.As(err, &status) {
					err = nativeSafeError(t.ctx, "write", err)
				}
				t.shutdown(err)
			}
			t.finishWrite(message, err)
			if err != nil {
				return
			}
		}
	}
}

func (t *nativeWireRun) finishWrite(message nativeWrite, err error) {
	t.mu.Lock()
	t.queued -= len(message.data)
	t.mu.Unlock()
	message.done <- err
}

func (t *nativeWireRun) heartbeat(interval time.Duration) {
	defer t.wg.Done()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			if t.Send(t.ctx, protobuf(nil).bytes(7, nil)) != nil && t.ctx.Err() != nil {
				return
			}
		}
	}
}

func (t *nativeWireRun) Next(ctx context.Context) ([]byte, bool, error) {
	select {
	case t.readGate <- struct{}{}:
	case <-ctx.Done():
		t.shutdown(ctx.Err())
		return nil, false, ctx.Err()
	case <-t.ctx.Done():
		return nil, false, t.failure()
	}
	defer func() { <-t.readGate }()
	stop := context.AfterFunc(ctx, func() { t.shutdown(ctx.Err()) })
	defer stop()
	t.mu.Lock()
	body := t.body
	closed := t.closed
	t.mu.Unlock()
	if closed || body == nil {
		return nil, false, t.failure()
	}
	data, end, err := readConnectFrame(nativeNetworkReader{body})
	if ctx.Err() != nil {
		t.shutdown(ctx.Err())
		return nil, false, ctx.Err()
	}
	if t.ctx.Err() != nil {
		return nil, false, t.failure()
	}
	if err != nil {
		// Only the underlying response-body read is a network failure;
		// Connect/gzip decoding failures retain the normal host policy.
		var read nativeReadError
		if errors.As(err, &read) {
			err = nativeSafeError(ctx, "stream read", err)
		} else {
			err = nativeSafeError(ctx, "stream read")
		}
		t.shutdown(err)
		return nil, false, err
	}
	if end {
		var trailer map[string]json.RawMessage
		if json.Unmarshal(data, &trailer) != nil || trailer == nil {
			err = failure(502, "Native Cursor Connect trailer is malformed")
		} else if raw := trailer["error"]; len(raw) > 0 && string(raw) != "null" {
			var detail struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &detail)
			status := map[string]int{"invalid_argument": 400, "unauthenticated": 401, "permission_denied": 403, "resource_exhausted": 429, "deadline_exceeded": 504}[detail.Code]
			if status == 0 {
				status = 502
			}
			err = failure(status, "Native Cursor Connect stream returned an error")
		}
		if err != nil {
			t.shutdown(err)
			return nil, true, err
		}
		t.shutdown(nil)
	}
	return data, end, nil
}

func (t *nativeWireRun) shutdown(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed, t.err = true, err
	body, pipe, release := t.body, t.pipe, t.release
	t.body = nil
	t.mu.Unlock()
	t.cancel()
	if pipe != nil {
		pipe.CloseWithError(context.Canceled)
	}
	if body != nil {
		body.Close()
	}
	if t.stopPool != nil {
		t.stopPool()
	}
	if release != nil {
		release()
	}
	t.pool.mu.Lock()
	delete(t.pool.runs, t)
	t.pool.mu.Unlock()
}

func (t *nativeWireRun) Close() error { t.shutdown(nil); t.wg.Wait(); return nil }

func (p *nativeTransportPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.closeDone
		return
	}
	p.closed = true
	defer close(p.closeDone)
	runs := make([]*nativeWireRun, 0, len(p.runs))
	for run := range p.runs {
		runs = append(runs, run)
	}
	entries := make([]*nativeTransportEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	p.cancel()
	for _, run := range runs {
		run.Close()
	}
	for _, entry := range entries {
		entry.closeIdle()
	}
	p.ops.Wait()
}

func (p *nativeTransportPool) JSON(ctx context.Context, account, token, path string, body map[string]any) (map[string]any, error) {
	if path != "/aiserver.v1.DashboardService/GetFilteredUsageEvents" {
		return nil, failure(400, "Native Cursor JSON path is not permitted")
	}
	entry, release, err := p.acquire(account)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > maxWireBytes {
		return nil, failure(400, "Invalid native Cursor JSON body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return nil, nativeSafeError(ctx, "JSON request")
	}
	req.Header = headers(token)
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = nil
	resp, err := entry.h1.Do(req)
	if err != nil {
		return nil, nativeSafeError(ctx, "JSON query", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nativeHTTPError(resp.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
	if err != nil || len(raw) > maxWireBytes {
		return nil, nativeSafeError(ctx, "JSON response", err)
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, failure(502, "Invalid native Cursor JSON response")
	}
	return result, nil
}

func nativeWriteAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// The hex string is streamed instead of duplicating a large KV blob twice.
type nativeHexReader struct {
	data     []byte
	position int
}

func (r *nativeHexReader) Read(out []byte) (int, error) {
	const digits = "0123456789abcdef"
	n := 0
	for n < len(out) && r.position < len(r.data)*2 {
		value := r.data[r.position/2]
		if r.position%2 == 0 {
			out[n] = digits[value>>4]
		} else {
			out[n] = digits[value&15]
		}
		r.position++
		n++
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Explicit proxy handling avoids environment proxy inheritance and makes ALPN
// negotiation happen inside the selected tunnel before a Run can be submitted.
func (p *nativeTransportPool) dial(ctx context.Context, address string, trust *tls.Config) (net.Conn, error) {
	dialer := net.Dialer{}
	if p.proxy == nil {
		return dialer.DialContext(ctx, "tcp", address)
	}
	proxy := p.proxy
	port := proxy.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[proxy.Scheme]
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, err
	}
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	if proxy.Scheme == "https" {
		cfg := trust.Clone()
		cfg.ServerName = proxy.Hostname()
		cfg.NextProtos = []string{"http/1.1"}
		secured := tls.Client(conn, cfg)
		if err = secured.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secured
	}
	if proxy.Scheme == "socks5" || proxy.Scheme == "socks5h" {
		err = nativeSOCKS(ctx, conn, proxy, address)
	} else {
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
		if proxy.User != nil {
			password, _ := proxy.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
		}
		if err = req.Write(conn); err == nil {
			reader := bufio.NewReader(io.LimitReader(conn, nativeProxyHeaderBytes))
			var response *http.Response
			response, err = http.ReadResponse(reader, req)
			if err == nil && (response.StatusCode != 200 || reader.Buffered() != 0) {
				err = errors.New("native proxy tunnel rejected")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}

func nativeSOCKS(ctx context.Context, conn net.Conn, proxy *url.URL, address string) error {
	method := byte(0)
	if proxy.User != nil {
		method = 2
	}
	if err := nativeWriteAll(conn, []byte{5, 1, method}); err != nil {
		return err
	}
	var pair [2]byte
	if _, err := io.ReadFull(conn, pair[:]); err != nil {
		return err
	}
	if pair != [2]byte{5, method} {
		return errors.New("native SOCKS authentication rejected")
	}
	if method == 2 {
		password, _ := proxy.User.Password()
		user := proxy.User.Username()
		if len(user) > 255 || len(password) > 255 {
			return errors.New("native SOCKS credentials exceed limit")
		}
		message := append([]byte{1, byte(len(user))}, user...)
		message = append(message, byte(len(password)))
		message = append(message, password...)
		if err := nativeWriteAll(conn, message); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, pair[:]); err != nil {
			return err
		}
		if pair != [2]byte{1, 0} {
			return errors.New("native SOCKS credentials rejected")
		}
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil && proxy.Scheme == "socks5" {
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return e
		}
		if len(ips) == 0 {
			return errors.New("native SOCKS target resolution failed")
		}
		ip = ips[0].IP
	}
	request := []byte{5, 1, 0}
	if v4 := ip.To4(); v4 != nil {
		request = append(request, 1)
		request = append(request, v4...)
	} else if ip != nil {
		request = append(request, 4)
		request = append(request, ip.To16()...)
	} else {
		if len(host) > 255 {
			return errors.New("native SOCKS target exceeds limit")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = append(request, byte(port>>8), byte(port))
	if err = nativeWriteAll(conn, request); err != nil {
		return err
	}
	var head [4]byte
	if _, err = io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 5 || head[1] != 0 || head[2] != 0 {
		return errors.New("native SOCKS tunnel rejected")
	}
	size := 0
	switch head[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		var length [1]byte
		if _, err = io.ReadFull(conn, length[:]); err != nil {
			return err
		}
		size = int(length[0])
	default:
		return errors.New("invalid native SOCKS tunnel")
	}
	_, err = io.CopyN(io.Discard, conn, int64(size+2))
	return err
}
