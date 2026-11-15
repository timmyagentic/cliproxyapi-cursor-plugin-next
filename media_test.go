package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9ZlYQAAAAASUVORK5CYII="

func imageFixtureFetcher(t *testing.T, handler http.HandlerFunc) (*publicImageFetcher, *[]string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := newPublicImageFetcher(context.Background(), &nativeTransportPool{})
	t.Cleanup(f.client.CloseIdleConnections)
	f.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dials := []string{}
	f.dial = func(ctx context.Context, address string) (net.Conn, error) {
		dials = append(dials, address)
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return f, &dials
}

func TestRemoteImagePinsPublicIPAndDoesNotSendCredentials(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(testImageBase64)
	f, dials := imageFixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "images.example" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("origin request lost its host or inherited credentials")
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	data, mimeType, err := f.fetch("http://images.example/image.png")
	if err != nil || mimeType != "image/png" || string(data) != string(png) {
		t.Fatalf("public image failed: %s %v", mimeType, err)
	}
	if len(*dials) != 1 || (*dials)[0] != "8.8.8.8:80" {
		t.Fatalf("hostname was not pinned to a checked address: %v", *dials)
	}
	chat := chatRequest{Messages: []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://images.example/image.png"}}}}}}
	conv, err := conversation(chat, nil, f.fetch)
	if err != nil || !strings.Contains(string(conv.Messages[0]), hex.EncodeToString(png)) || strings.Contains(string(conv.Messages[0]), "[attachment image:") {
		t.Fatalf("image bytes were not imported: %v", err)
	}
}

func TestRemoteImageUsesExplicitConnectRouteWithOriginalTLSIdentity(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(testImageBase64)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("origin identity or credential isolation changed")
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer origin.Close()
	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("image request bypassed the CONNECT tunnel")
			w.WriteHeader(400)
			return
		}
		connects <- r.Host
		upstream, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	defer proxy.Close()
	route, err := newNativeTransportPool(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	f := newPublicImageFetcher(context.Background(), route)
	defer f.client.CloseIdleConnections()
	f.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: origin.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12}
	data, _, err := f.fetch("https://example.com/image")
	if err != nil || string(data) != string(png) {
		t.Fatalf("explicit route/TLS image fetch failed: %v", err)
	}
	if target := <-connects; target != "8.8.8.8:443" {
		t.Fatalf("proxy received unchecked hostname instead of pinned address: %s", target)
	}
}

func TestRemoteImageRejectsPrivateMixedAndReboundDestinations(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.1.2", "192.168.1.2", "169.254.169.254", "100.64.0.1", "198.18.0.1", "192.0.2.1", "224.0.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::1", "2001:db8::1"} {
		if isPublicImageIP(net.ParseIP(ip)) {
			t.Fatalf("non-public address accepted: %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicImageIP(net.ParseIP(ip)) {
			t.Fatalf("public address rejected: %s", ip)
		}
	}
	f, dials := imageFixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/next", http.StatusFound) })
	lookups := 0
	f.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, _, err := f.fetch("http://images.example/first"); errorStatus(err) != 400 || len(*dials) != 1 {
		t.Fatalf("redirect re-resolution bypassed the IP check: err=%v dials=%v", err, *dials)
	}
	f.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
	}
	if _, _, err := f.fetch("http://other.example/image"); errorStatus(err) != 400 || len(*dials) != 1 {
		t.Fatalf("mixed public/private DNS was accepted: %v", err)
	}
}

type imageRoundTripper func(*http.Request) (*http.Response, error)

func (f imageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRemoteImageBoundsAndSafeErrors(t *testing.T) {
	f := newPublicImageFetcher(context.Background(), &nativeTransportPool{})
	t.Cleanup(f.client.CloseIdleConnections)
	for _, tc := range []struct {
		name, body, contentType string
		length                  int64
		status                  int
		want                    int
	}{
		{"declared size", "", "image/png", maxInputImageBytes + 1, 200, 413},
		{"stream size", strings.Repeat("x", maxInputImageBytes+1), "image/png", -1, 200, 413},
		{"HTML", "<html>SYNTHETIC_SECRET</html>", "text/html", -1, 200, 400},
		{"upstream error", "SYNTHETIC_SECRET", "text/plain", -1, 403, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.client.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			_, _, err := f.fetch("http://images.example/image?token=SYNTHETIC_SECRET")
			if errorStatus(err) != tc.want || strings.Contains(err.Error(), "SYNTHETIC_SECRET") {
				t.Fatalf("unsafe/unbounded image result: %v", err)
			}
		})
	}
	f.client.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for SYNTHETIC_SECRET")
	})
	if _, _, err := f.fetch("http://images.example/fail?token=SYNTHETIC_SECRET"); errorStatus(err) != 400 || err.Error() != unreadableInputImage {
		t.Fatalf("image network error was not classified as safe input failure: %v", err)
	}
	f, _ = imageFixtureFetcher(t, func(http.ResponseWriter, *http.Request) {})
	f.client.Timeout = 20 * time.Millisecond
	f.dial = func(ctx context.Context, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	started := time.Now()
	if _, _, err := f.fetch("http://images.example/slow"); errorStatus(err) != 400 || time.Since(started) > time.Second {
		t.Fatalf("image timeout did not bound the request: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := newPublicImageFetcher(ctx, &nativeTransportPool{})
	defer cancelled.client.CloseIdleConnections()
	if _, _, err := cancelled.fetch("http://images.example/cancelled"); errorStatus(err) != 499 {
		t.Fatalf("cancellation lost its status: %v", err)
	}
}

func TestFileAndUnsupportedMediaAreNotPlaceholders(t *testing.T) {
	text := "真实内容\ncredits = 8\n"
	chat := chatRequest{Messages: []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "file", "file": map[string]any{"filename": "fees.txt", "file_data": base64.StdEncoding.EncodeToString([]byte(text))}}}}}}
	conv, err := conversation(chat, nil)
	if err != nil || !strings.Contains(string(conv.Messages[0]), "真实内容") || !strings.Contains(string(conv.Messages[0]), "credits = 8") {
		t.Fatalf("file contents missing: %v", err)
	}
	for _, part := range []map[string]any{
		{"type": "file", "file": map[string]any{"filename": "name-only.txt"}},
		{"type": "file", "file": map[string]any{"file_id": "file-canary"}},
		{"type": "file", "file": map[string]any{"file_data": base64.StdEncoding.EncodeToString([]byte{0, 1, 2})}},
		{"type": "file", "file": map[string]any{"file_data": "data:application/pdf;base64,JVBERg=="}},
		{"type": "input_audio", "input_audio": map[string]any{"data": "canary"}},
		{"type": "video_url", "video_url": "https://example/video"},
		{"type": "image_url", "image_url": "https://example/image"},
	} {
		chat.Messages[0]["content"] = []any{part}
		if _, err := conversation(chat, nil); errorStatus(err) != 400 {
			t.Fatalf("unsupported attachment silently accepted: %v (%v)", part["type"], err)
		}
	}
}

func TestInlineImageStillImportsBytesWithoutOutboundRoute(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(testImageBase64)
	chat := chatRequest{Messages: []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": "data:image/png;base64," + testImageBase64}}}}}
	conv, err := conversation(chat, nil)
	if err != nil || len(conv.Messages) != 1 || !strings.Contains(string(conv.Messages[0]), hex.EncodeToString(png)) {
		t.Fatalf("inline image lost its byte payload: %v", err)
	}
}
