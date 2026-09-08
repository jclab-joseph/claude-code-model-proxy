package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/ca"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
)

// startProxy runs the CONNECT proxy on a loopback port and returns its
// address together with the authority it mints certificates from.
func startProxy(t *testing.T, cfg *config.Config, handler http.Handler) (string, *ca.Bundle) {
	t.Helper()

	authority, err := ca.Open(t.TempDir(), cfg.InterceptHosts)
	if err != nil {
		t.Fatalf("ca.Open: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := &http.Server{Handler: NewServer(cfg, authority, handler, nil, nil)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	return listener.Addr().String(), authority
}

// clientThrough builds a client that sends everything through the proxy and
// trusts the given roots.
func clientThrough(t *testing.T, proxyAddr string, roots *x509.CertPool, credentials *url.Userinfo) *http.Client {
	t.Helper()
	proxyURL := &url.URL{Scheme: "http", Host: proxyAddr, User: credentials}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
	}
}

func poolFor(authority *ca.Bundle) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(authority.Root())
	return pool
}

func TestInterceptedHostIsServedLocally(t *testing.T) {
	// The handler stands in for the router; reaching it proves TLS for
	// api.anthropic.com was terminated by the proxy.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "served %s %s host=%s", r.Method, r.URL.Path, r.Host)
	})

	cfg := config.Default()
	addr, authority := startProxy(t, cfg, handler)
	client := clientThrough(t, addr, poolFor(authority), nil)

	// No DNS lookup happens: the proxy answers the CONNECT itself.
	resp, err := client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	want := "served GET /v1/messages host=api.anthropic.com"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestInterceptedConnectionServesKeepAliveRequests(t *testing.T) {
	var count int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		fmt.Fprintf(w, "request %d", count)
	})

	cfg := config.Default()
	addr, authority := startProxy(t, cfg, handler)
	client := clientThrough(t, addr, poolFor(authority), nil)

	for i := 1; i <= 3; i++ {
		resp, err := client.Get("https://api.anthropic.com/v1/models")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if got, want := string(body), fmt.Sprintf("request %d", i); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	}
	if count != 3 {
		t.Fatalf("handler ran %d times, want 3", count)
	}
}

func TestUninterceptedHostIsTunnelled(t *testing.T) {
	// A TLS server the proxy has no certificate for: if the proxy tried to
	// intercept it, the client's verification would fail.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("straight from upstream"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	addr, _ := startProxy(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the router handled a host that should have been tunnelled")
	}))

	// Trust only the upstream's own certificate.
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	client := clientThrough(t, addr, pool, nil)

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("tunnelled request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "straight from upstream" {
		t.Fatalf("body = %q", body)
	}
}

func TestProxyAuthentication(t *testing.T) {
	cfg := config.Default()
	cfg.ProxyUsername = "user"
	cfg.ProxyPassword = "secret"

	addr, authority := startProxy(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("authorized"))
	}))
	pool := poolFor(authority)

	if _, err := clientThrough(t, addr, pool, nil).Get("https://api.anthropic.com/v1/models"); err == nil {
		t.Fatal("a request without proxy credentials must be refused")
	}
	if _, err := clientThrough(t, addr, pool, url.UserPassword("user", "wrong")).Get("https://api.anthropic.com/v1/models"); err == nil {
		t.Fatal("a request with wrong proxy credentials must be refused")
	}

	resp, err := clientThrough(t, addr, pool, url.UserPassword("user", "secret")).Get("https://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatalf("authorized request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "authorized" {
		t.Fatalf("body = %q", body)
	}
}

func TestPlainHTTPProxyRequestForInterceptedHost(t *testing.T) {
	cfg := config.Default()
	addr, _ := startProxy(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "local %s", r.URL.Path)
	}))

	client := clientThrough(t, addr, nil, nil)
	resp, err := client.Get("http://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatalf("plain HTTP proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "local /v1/models" {
		t.Fatalf("body = %q", body)
	}
}

func TestDirectTLSListenerPresentsInterceptedHostname(t *testing.T) {
	authority, err := ca.Open(t.TempDir(), []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("ca.Open: %v", err)
	}

	// Serve on a loopback port; a hosts-file redirect would send real
	// traffic here, so the certificate must be valid for the API hostname.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "tls", addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("direct tls"))
		}), authority, "api.anthropic.com")
	}()

	waitForListener(t, addr)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: poolFor(authority)},
			// Resolve the API hostname to the local listener, the way a
			// hosts-file entry would.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}

	resp, err := client.Get("https://api.anthropic.com/v1/models")
	if err != nil {
		t.Fatalf("direct TLS request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct tls" {
		t.Fatalf("body = %q", body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}
