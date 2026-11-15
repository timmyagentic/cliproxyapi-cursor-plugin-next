package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const imageFetchTimeout = 10 * time.Second
const imagePreparationTimeout = 15 * time.Second
const unreadableInputImage = "Input image could not be retrieved; provide another URL or an image data URL, or retry manually"

// Routing is reused from the explicit native transport, but image requests use
// their own client and no Cursor/host headers. DNS is checked locally and only
// a validated numeric address is sent through the configured proxy tunnel.
type publicImageFetcher struct {
	lookup func(context.Context, string) ([]net.IPAddr, error)
	dial   func(context.Context, string) (net.Conn, error)
	client *http.Client
	ctx    context.Context
}

func newPublicImageFetcher(ctx context.Context, route *nativeTransportPool) *publicImageFetcher {
	f := &publicImageFetcher{ctx: ctx, lookup: net.DefaultResolver.LookupIPAddr}
	f.dial = func(ctx context.Context, address string) (net.Conn, error) {
		return route.dial(ctx, address, &tls.Config{MinVersion: tls.VersionTLS12})
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: f.dialPublic, DisableKeepAlives: true,
		DisableCompression: true, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	f.client = &http.Client{Transport: transport, Timeout: imageFetchTimeout}
	f.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		if len(via) >= 3 {
			return failure(400, "Remote image redirect limit exceeded")
		}
		if _, err := publicImageURL(req.URL.String()); err != nil {
			return err
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return failure(400, "Remote image redirects may not downgrade HTTPS")
		}
		return nil
	}
	return f
}

func publicImageURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.Contains(u.Hostname(), "%") {
		return nil, failure(400, "Remote image must be an HTTP(S) URL without credentials or an address zone")
	}
	return u, nil
}

var nonPublicImageRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func isPublicImageIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	// Only the currently allocated IPv6 global-unicast range is accepted;
	// this also excludes NAT64, local translation and discard-only prefixes.
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range nonPublicImageRanges {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func (f *publicImageFetcher) dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(f.ctx, cancel)
	defer stop()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, failure(400, "Invalid remote image address")
	}
	var addresses []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	} else {
		addresses, err = f.lookup(ctx, host)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(addresses) == 0 || len(addresses) > 16 {
		return nil, failure(400, "Remote image hostname could not be resolved safely")
	}
	for _, addr := range addresses {
		if addr.Zone != "" || !isPublicImageIP(addr.IP) {
			return nil, failure(400, "Remote image hostname resolves to a non-public address")
		}
	}
	for _, addr := range addresses {
		conn, dialErr := f.dial(ctx, net.JoinHostPort(addr.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, failure(502, "Remote image connection failed")
}

func imageFetchError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	var policy *statusError
	if errors.As(err, &policy) && (policy.status == 400 || policy.status == 413) {
		return failure(policy.status, "Remote image URL was rejected by its public-address or redirect policy")
	}
	// The requested attachment is unreadable; Cursor has not failed. A 5xx here
	// would incorrectly let CPA retry/cool down the model account for a user CDN.
	return failure(400, unreadableInputImage)
}

func (f *publicImageFetcher) fetch(raw string) ([]byte, string, error) {
	u, err := publicImageURL(raw)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", failure(400, "Invalid remote image URL")
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp")
	resp, err := f.client.Do(req)
	if err != nil {
		// net/http errors may contain the signed URL or proxy credentials.
		return nil, "", imageFetchError(f.ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", failure(400, unreadableInputImage)
	}
	if resp.ContentLength > maxInputImageBytes {
		return nil, "", failure(413, "Remote image exceeds the 8 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInputImageBytes+1))
	if err != nil {
		return nil, "", imageFetchError(f.ctx, err)
	}
	if len(data) > maxInputImageBytes {
		return nil, "", failure(413, "Remote image exceeds the 8 MiB limit")
	}
	detected := strings.Split(http.DetectContentType(data), ";")[0]
	if !imageMIME(detected) {
		return nil, "", failure(400, "Remote URL did not return a supported image")
	}
	if declared := resp.Header.Get("Content-Type"); declared != "" {
		kind, _, parseErr := mime.ParseMediaType(declared)
		if parseErr != nil || (kind != detected && kind != "application/octet-stream") {
			return nil, "", failure(400, "Remote image content type does not match its bytes")
		}
	}
	return data, detected, nil
}
