// Package proxy exposes the router through the two entry points a client can
// reach it by: an HTTP CONNECT proxy that terminates TLS for the intercepted
// hosts, and a direct TLS listener presenting the intercepted host's name.
package proxy

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/ca"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
)

// tunnelIdleTimeout closes a pass-through tunnel that has gone quiet in both
// directions. It is generous because API responses can stream for minutes.
const tunnelIdleTimeout = 10 * time.Minute

// Server is the HTTP CONNECT proxy.
type Server struct {
	cfg     *config.Config
	ca      *ca.Bundle
	handler http.Handler

	// transport forwards plain-HTTP proxy requests for hosts that are not
	// intercepted.
	transport http.RoundTripper

	// dial opens the tunnels for hosts that are not intercepted. It goes
	// through the proxy's own resolver so tunnelled traffic is not subject
	// to a hosts file either.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// serverPool reuses the embedded HTTP server across intercepted
	// connections so its settings are defined in one place.
	once     sync.Once
	embedded *http.Server
}

// Dialer opens outbound connections. The proxy's own resolver satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewServer builds the CONNECT proxy around handler. transport forwards
// plain-HTTP requests for hosts that are not intercepted and must not route
// through this proxy; dial opens the CONNECT tunnels. Both fall back to the
// operating system's defaults when nil.
func NewServer(cfg *config.Config, authority *ca.Bundle, handler http.Handler, transport http.RoundTripper, dial Dialer) *Server {
	if transport == nil {
		transport = http.DefaultTransport
	}
	dialContext := (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	if dial != nil {
		dialContext = dial.DialContext
	}
	return &Server{
		cfg:       cfg,
		ca:        authority,
		handler:   handler,
		transport: transport,
		dial:      dialContext,
	}
}

// ServeHTTP handles both CONNECT tunnels and absolute-URI proxy requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !s.authorized(req) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="ccmproxy"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}

	if req.Method == http.MethodConnect {
		s.handleConnect(w, req)
		return
	}
	s.handleAbsolute(w, req)
}

func (s *Server) authorized(req *http.Request) bool {
	if s.cfg.ProxyUsername == "" || s.cfg.ProxyPassword == "" {
		return true
	}
	header := req.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	user, password, found := strings.Cut(string(decoded), ":")
	if !found {
		return false
	}
	// Constant-time comparison keeps the check free of a timing signal.
	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.ProxyUsername)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.ProxyPassword)) == 1
	return userMatch && passMatch
}

func (s *Server) handleConnect(w http.ResponseWriter, req *http.Request) {
	target := req.Host
	if target == "" {
		target = req.URL.Host
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	if !s.cfg.Intercepts(target) {
		s.tunnel(w, req, target)
		return
	}

	client, err := hijack(w)
	if err != nil {
		logx.Errorf("connect %s: %v", target, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		return
	}

	host, _, _ := net.SplitHostPort(target)
	tlsConn := tls.Server(client, &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.ca.GetCertificate(host),
		// Only HTTP/1.1 is negotiated: the router is served by net/http's
		// HTTP/1 server on this hijacked connection.
		NextProtos: []string{"http/1.1"},
	})

	logx.Debugf("intercepting TLS for %s", target)
	s.serveIntercepted(tlsConn)
}

// serveIntercepted hands one already-accepted connection to net/http, which
// then owns it: the call returns immediately and the connection is served in
// the background for as long as the client keeps it alive.
func (s *Server) serveIntercepted(conn net.Conn) {
	s.once.Do(func() {
		s.embedded = &http.Server{
			Handler:           s.handler,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       5 * time.Minute,
			// No write timeout: streamed responses are long-lived.
		}
	})
	// Serve returns as soon as the listener is drained; the connection
	// goroutine it started keeps running.
	if err := s.embedded.Serve(&singleConnListener{conn: conn}); err != nil && !errors.Is(err, io.EOF) {
		logx.Debugf("intercepted connection: %v", err)
	}
}

// tunnel relays bytes for hosts that are not intercepted, so unrelated
// traffic passes through untouched.
func (s *Server) tunnel(w http.ResponseWriter, req *http.Request, target string) {
	dialCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	upstream, err := s.dial(dialCtx, "tcp", target)
	if err != nil {
		logx.Warnf("tunnel %s: %v", target, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	client, err := hijack(w)
	if err != nil {
		_ = upstream.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}

	logx.Debugf("tunnelling %s", target)
	pipe(client, upstream)
}

// handleAbsolute serves a plain-HTTP proxy request.
func (s *Server) handleAbsolute(w http.ResponseWriter, req *http.Request) {
	if !req.URL.IsAbs() {
		http.Error(w, "this is a proxy endpoint; requests must use an absolute URI or CONNECT", http.StatusBadRequest)
		return
	}

	if s.cfg.Intercepts(req.URL.Host) {
		// Serve it locally, exactly as an intercepted TLS request.
		req.RequestURI = ""
		s.handler.ServeHTTP(w, req)
		return
	}

	outbound := req.Clone(req.Context())
	outbound.RequestURI = ""
	outbound.Header.Del("Proxy-Authorization")
	outbound.Header.Del("Proxy-Connection")

	resp, err := s.transport.RoundTrip(outbound)
	if err != nil {
		logx.Warnf("forward %s: %v", req.URL, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func hijack(w http.ResponseWriter) (net.Conn, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("proxy: connection cannot be hijacked")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("proxy: hijack: %w", err)
	}
	// A client that pipelined bytes after CONNECT would have them sitting in
	// the buffer; refusing is safer than silently dropping them.
	if buffered.Reader.Buffered() > 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy: client sent data before the tunnel was established")
	}
	return conn, nil
}

// pipe copies in both directions until either side closes.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		_ = src.SetDeadline(time.Now().Add(tunnelIdleTimeout))
		_, _ = io.Copy(dst, src)
		// Half-closing lets the peer observe the end of the stream.
		if closer, ok := dst.(*net.TCPConn); ok {
			_ = closer.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// singleConnListener presents one existing connection as a net.Listener so
// net/http's server can drive it. It yields that connection once and then
// reports EOF, which ends Serve's accept loop without disturbing the
// connection goroutine it already started.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn != nil {
		return conn, nil
	}
	return nil, io.EOF
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
