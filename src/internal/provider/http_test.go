package provider

import (
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestClientFetchAndCacheValidation(t *testing.T) {
	modified := time.Date(2026, time.July, 30, 1, 2, 3, 0, time.UTC).Format(http.TimeFormat)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog" || r.URL.RawQuery != "page=1" {
			t.Errorf("request target = %q", r.URL.RequestURI())
		}
		if got := r.Header.Get("User-Agent"); got != userAgent() {
			t.Errorf("User-Agent = %q", got)
		}
		if r.Header.Get("If-None-Match") != `"old"` || r.Header.Get("If-Modified-Since") != modified {
			t.Errorf("conditional headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("ETag", `"new"`)
		w.Header().Set("Last-Modified", modified)
		_, _ = w.Write([]byte("<catalog/>"))
	}))
	defer server.Close()

	client := tlsTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Fetch(ctx, "/catalog?page=1", FetchOptions{
		MaxBody:      1024,
		ETag:         `"old"`,
		LastModified: modified,
		ValidateContentType: func(mediaType string) error {
			if mediaType != "application/xml" {
				return fmt.Errorf("unexpected %q", mediaType)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusOK || string(response.Body) != "<catalog/>" || response.ETag != `"new"` || response.LastModified != modified || response.ContentType != "application/xml" {
		t.Fatalf("response = %#v", response)
	}
}

func TestValidateMetainfoContentType(t *testing.T) {
	for _, mediaType := range []string{"", "application/x-bittorrent", "Application/Octet-Stream"} {
		if err := ValidateMetainfoContentType(mediaType); err != nil {
			t.Fatalf("accepted content type %q: %v", mediaType, err)
		}
	}
	if err := ValidateMetainfoContentType("text/html"); err == nil {
		t.Fatal("text/html accepted as torrent metadata")
	}
}

func TestClientRejectsUnsafeOriginsAndPaths(t *testing.T) {
	for _, origin := range []string{
		"http://example.com",
		"https://user@example.com",
		"https://example.com/path",
		"https://127.0.0.1",
		"https://[::1]",
		"https://169.254.169.254",
		"https://localhost",
	} {
		if _, err := NewClient(origin); err == nil {
			t.Errorf("NewClient(%q) succeeded", origin)
		}
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unsafe path reached server")
	}))
	defer server.Close()
	client := tlsTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, path := range []string{"https://example.com/x", "//example.com/x", "relative", "/ok#fragment"} {
		if _, err := client.Fetch(ctx, path, FetchOptions{MaxBody: 1}); err == nil {
			t.Errorf("Fetch(%q) succeeded", path)
		}
	}
}

func TestClientRedirectPolicy(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("cross-origin redirect was followed")
	}))
	defer other.Close()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/one":
			http.Redirect(w, r, "/two", http.StatusFound)
		case "/two":
			http.Redirect(w, r, "/done", http.StatusFound)
		case "/done":
			_, _ = w.Write([]byte("ok"))
		case "/three":
			http.Redirect(w, r, "/one", http.StatusFound)
		case "/away":
			http.Redirect(w, r, other.URL, http.StatusFound)
		}
	}))
	defer server.Close()
	client := tlsTestClient(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Fetch(ctx, "/one", FetchOptions{MaxBody: 8})
	if err != nil || string(response.Body) != "ok" {
		t.Fatalf("two redirects: response=%#v error=%v", response, err)
	}
	if _, err := client.Fetch(ctx, "/three", FetchOptions{MaxBody: 8}); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("three redirects error = %v", err)
	}
	if _, err := client.Fetch(ctx, "/away", FetchOptions{MaxBody: 8}); err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
}

func TestClientCapsDecompressedBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write([]byte(strings.Repeat("x", 4096)))
		_ = writer.Close()
	}))
	defer server.Close()
	client := tlsTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Fetch(ctx, "/", FetchOptions{MaxBody: 32}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("error = %v, want ErrBodyTooLarge", err)
	}
}

func TestClientVerifiesTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("untrusted"))
	}))
	defer server.Close()
	client, err := newClient(server.URL, clientOptions{allowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Fetch(ctx, "/private/path?token=do-not-log", FetchOptions{MaxBody: 16}); err == nil {
		t.Fatal("fetch with an untrusted server certificate succeeded")
	} else if strings.Contains(err.Error(), "private/path") || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("request target leaked in error: %v", err)
	}
}

func TestClientCancellationAndDeadlineRequirement(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := tlsTestClient(t, server)

	if _, err := client.Fetch(context.Background(), "/", FetchOptions{MaxBody: 1}); err == nil || !strings.Contains(err.Error(), "no deadline") {
		t.Fatalf("missing deadline error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Fetch(ctx, "/", FetchOptions{MaxBody: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestClientNotModifiedAndRateLimit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cached" {
			w.Header().Set("ETag", `"same"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := tlsTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	response, err := client.Fetch(ctx, "/cached", FetchOptions{MaxBody: 1})
	if err != nil || !response.NotModified || response.Status != http.StatusNotModified || response.ETag != `"same"` || response.Body != nil {
		t.Fatalf("304 response=%#v error=%v", response, err)
	}
	_, err = client.Fetch(ctx, "/limited", FetchOptions{MaxBody: 1})
	var limited *RateLimitError
	if !errors.As(err, &limited) || !limited.Valid || limited.RetryAfter != 7*time.Second {
		t.Fatalf("429 error = %#v (%v)", limited, err)
	}
}

func TestClientRejectsPartialResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("partial"))
	}))
	defer server.Close()
	client := tlsTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.Fetch(ctx, "/", FetchOptions{MaxBody: 16})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusPartialContent {
		t.Fatalf("partial response error = %v", err)
	}
}

func TestUnsafeAddress(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "168.63.129.16", "169.254.169.254", "100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1", "2001:db8::1", "::1"} {
		if !unsafeAddress(netip.MustParseAddr(value)) {
			t.Errorf("%s considered safe", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "64:ff9b::101:101", "2606:4700:4700::1111"} {
		if unsafeAddress(netip.MustParseAddr(value)) {
			t.Errorf("%s considered unsafe", value)
		}
	}
}

func FuzzParseRetryAfter(f *testing.F) {
	f.Add("120")
	f.Add("Wed, 21 Oct 2015 07:28:00 GMT")
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = parseRetryAfter(value, time.Unix(0, 0))
	})
}

func tlsTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client, err := newClient(server.URL, clientOptions{allowPrivate: true, rootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	return client
}
