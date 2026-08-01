// Package provider defines catalogue capabilities and their shared HTTP boundary.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/keys-i/blackbeard/src/internal/version"
)

const (
	MaxResponseBody = int64(32 << 20)
)

var (
	ErrBodyTooLarge     = errors.New("provider response exceeds its decompressed body limit")
	ErrPrivateEndpoint  = errors.New("private or local provider endpoint is not allowed")
	ErrTooManyRedirects = errors.New("provider response exceeds two redirects")
)

// Client fetches bounded resources from one immutable HTTPS origin.
type Client struct {
	base *url.URL
	http *http.Client
}

// FetchOptions contains the policy that varies by provider resource.
type FetchOptions struct {
	MaxBody             int64
	ETag                string
	LastModified        string
	ValidateContentType func(mediaType string) error
}

// Response contains a bounded body and cache validators safe to persist.
type Response struct {
	Status       int
	Body         []byte
	ETag         string
	LastModified string
	ContentType  string
	NotModified  bool
}

// ValidateMetainfoContentType accepts the media types used for torrent
// metainfo. An absent type remains acceptable because several catalogues omit
// it; the bencode boundary still validates the body before use.
func ValidateMetainfoContentType(mediaType string) error {
	switch strings.ToLower(mediaType) {
	case "", "application/x-bittorrent", "application/octet-stream":
		return nil
	default:
		return fmt.Errorf("unexpected torrent metainfo content type %q", mediaType)
	}
}

// RateLimitError reports a 429 without hiding a sleep inside the HTTP layer.
type RateLimitError struct {
	RetryAfter time.Duration
	Valid      bool
}

func (e *RateLimitError) Error() string {
	if e.Valid {
		return fmt.Sprintf("provider rate limited request; retry after %s", e.RetryAfter)
	}
	return "provider rate limited request"
}

// StatusError reports a non-success HTTP response without reflecting its body.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("provider returned HTTP status %d", e.Status)
}

// NewClient constructs a public-network-only client. It neither disables TLS
// verification nor inherits proxy settings that could bypass address checks.
func NewClient(origin string) (*Client, error) {
	return newClient(origin, clientOptions{})
}

type clientOptions struct {
	allowPrivate bool
	rootCAs      *x509.CertPool
}

func newClient(origin string, opts clientOptions) (*Client, error) {
	base, err := parseOrigin(origin)
	if err != nil {
		return nil, err
	}
	hostname := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	if !opts.allowPrivate && (hostname == "localhost" || strings.HasSuffix(hostname, ".localhost")) {
		return nil, ErrPrivateEndpoint
	}
	if ip, err := netip.ParseAddr(base.Hostname()); err == nil && !opts.allowPrivate && unsafeAddress(ip) {
		return nil, ErrPrivateEndpoint
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := dialer.DialContext
	if !opts.allowPrivate {
		dialContext = publicDialContext(dialer)
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    opts.rootCAs,
		},
	}
	c := &Client{base: base}
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 2 {
				return ErrTooManyRedirects
			}
			if req.URL.User != nil || !sameOrigin(base, req.URL) {
				return errors.New("provider redirect changed origin")
			}
			return nil
		},
	}
	return c, nil
}

func parseOrigin(origin string) (*url.URL, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("parse provider origin: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("provider origin must be an HTTPS scheme and authority only")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return nil, errors.New("provider origin has an invalid port")
		}
	}
	u.Path = "/"
	return u, nil
}

// Fetch performs one bounded GET. The caller must provide the total deadline.
func (c *Client) Fetch(ctx context.Context, path string, opts FetchOptions) (Response, error) {
	var result Response
	if ctx == nil {
		return result, errors.New("provider request context is nil")
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("provider request context has no deadline")
	}
	if opts.MaxBody <= 0 || opts.MaxBody > MaxResponseBody {
		return result, fmt.Errorf("provider response body limit must be between 1 and %d bytes", MaxResponseBody)
	}
	target, err := c.resolve(path)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return result, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())
	if opts.ETag != "" {
		if !validHeaderValue(opts.ETag) {
			return result, errors.New("provider ETag cache validator is invalid")
		}
		req.Header.Set("If-None-Match", opts.ETag)
	}
	if opts.LastModified != "" {
		modified, err := http.ParseTime(opts.LastModified)
		if err != nil {
			return result, errors.New("provider Last-Modified cache validator is invalid")
		}
		req.Header.Set("If-Modified-Since", modified.UTC().Format(http.TimeFormat))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return result, fmt.Errorf("fetch provider resource: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	result.Status = resp.StatusCode
	result.ETag, err = responseValidator(resp.Header.Get("ETag"))
	if err != nil {
		return Response{}, err
	}
	result.LastModified, err = responseLastModified(resp.Header.Get("Last-Modified"))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		delay, valid := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return result, &RateLimitError{RetryAfter: delay, Valid: valid}
	}
	if resp.StatusCode != http.StatusOK {
		return result, &StatusError{Status: resp.StatusCode}
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		result.ContentType, _, err = mime.ParseMediaType(contentType)
		if err != nil {
			return Response{}, fmt.Errorf("parse provider content type: %w", err)
		}
	}
	if opts.ValidateContentType != nil {
		if err := opts.ValidateContentType(result.ContentType); err != nil {
			return Response{}, fmt.Errorf("validate provider content type: %w", err)
		}
	}

	result.Body, err = io.ReadAll(io.LimitReader(resp.Body, opts.MaxBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("read provider response: %w", err)
	}
	if int64(len(result.Body)) > opts.MaxBody {
		return Response{}, ErrBodyTooLarge
	}
	return result, nil
}

func (c *Client) resolve(path string) (*url.URL, error) {
	ref, err := url.ParseRequestURI(path)
	if err != nil {
		return nil, fmt.Errorf("parse provider path: %w", err)
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "#") || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.Fragment != "" {
		return nil, errors.New("provider path must be an absolute path on the configured origin")
	}
	target := c.base.ResolveReference(ref)
	if !sameOrigin(c.base, target) {
		return nil, errors.New("provider path changed origin")
	}
	return target, nil
}

func sameOrigin(a, b *url.URL) bool {
	return b.Scheme == "https" && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	return "443"
}

func publicDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse provider network address: %w", err)
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve provider host: %w", err)
		}
		var lastErr error
		for _, address := range addresses {
			address = address.Unmap()
			if unsafeAddress(address) || network == "tcp4" && !address.Is4() || network == "tcp6" && !address.Is6() {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, ErrPrivateEndpoint
	}
}

func unsafeAddress(address netip.Addr) bool {
	address = address.Unmap()
	if nat64WellKnown.Contains(address) {
		raw := address.As16()
		if unsafeAddress(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]})) {
			return true
		}
	}
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast() {
		return true
	}
	for _, network := range specialNetworks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

var specialNetworks = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func userAgent() string {
	return "blackbeard/" + version.Version + " (+https://github.com/keys-i/blackbeard)"
}

func validHeaderValue(value string) bool {
	if len(value) > 1024 {
		return false
	}
	for _, c := range []byte(value) {
		if c < ' ' && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

func responseValidator(value string) (string, error) {
	if !validHeaderValue(value) {
		return "", errors.New("provider response cache validator is invalid")
	}
	return value, nil
}

func responseLastModified(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !validHeaderValue(value) {
		return "", errors.New("provider response Last-Modified validator is invalid")
	}
	modified, err := http.ParseTime(value)
	if err != nil {
		return "", errors.New("provider response Last-Modified validator is invalid")
	}
	return modified.UTC().Format(http.TimeFormat), nil
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(0, when.Sub(now)), true
}
