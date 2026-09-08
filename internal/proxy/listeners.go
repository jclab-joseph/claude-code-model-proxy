package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/ca"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
)

// newServer builds an HTTP server tuned for long streaming responses.
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
}

// Serve runs handler on addr until ctx is cancelled. When authority is
// non-nil the listener speaks TLS, presenting a certificate for whichever
// name the client asks for and falling back to fallbackHost without SNI.
// This is the entry point for clients redirected by DNS or a hosts file
// rather than by proxy settings.
func Serve(ctx context.Context, name, addr string, handler http.Handler, authority *ca.Bundle, fallbackHost string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: listen on %s: %w", name, addr, err)
	}

	if authority != nil {
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: authority.GetCertificate(fallbackHost),
			NextProtos:     []string{"http/1.1"},
		})
	}

	server := newServer(handler)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logx.Infof("%s listening on %s", name, addr)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
