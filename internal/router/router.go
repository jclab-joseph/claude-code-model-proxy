// Package router serves the Anthropic API surface: it forwards traffic to the
// real API untouched, extends the advertised model list, and answers requests
// for the added models from their own provider.
package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/codex"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
	"github.com/tidwall/gjson"
)

// maxRequestBody bounds the request bodies the proxy buffers to inspect.
// Anthropic's own limit is well below this.
const maxRequestBody = 64 << 20

// Router implements http.Handler for the intercepted Anthropic endpoints.
type Router struct {
	cfg      *config.Config
	registry *codex.Registry
	backend  *codex.Backend

	upstream *url.URL
	proxy    *httputil.ReverseProxy
	client   *http.Client

	// caPEM is served from the status endpoint so a client can fetch the
	// trust anchor without knowing where it lives on disk.
	caPEM []byte
}

// New builds a router. transport carries pass-through traffic to the upstream
// API and must not itself route through this proxy. caPEM may be nil.
func New(cfg *config.Config, registry *codex.Registry, backend *codex.Backend, transport http.RoundTripper, caPEM []byte) (*Router, error) {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("router: parse upstream %q: %w", cfg.Upstream, err)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}

	r := &Router{
		cfg:      cfg,
		registry: registry,
		backend:  backend,
		upstream: upstream,
		client:   &http.Client{Transport: transport},
		caPEM:    caPEM,
	}

	r.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			if upstream.Path != "" && upstream.Path != "/" {
				pr.Out.URL.Path = strings.TrimSuffix(upstream.Path, "/") + pr.Out.URL.Path
			}
			// The upstream TLS certificate and API routing both key off the
			// Host header, so it must name the real API, not the proxy.
			pr.Out.Host = upstream.Host
		},
		// Server-sent events must not sit in a buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logx.Errorf("passthrough %s %s: %v", req.Method, req.URL.Path, err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
		},
	}
	return r, nil
}

// ServeHTTP dispatches one request.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	switch {
	case strings.HasPrefix(path, "/__ccmproxy/"):
		r.serveAdmin(w, req)
		return
	case req.Method == http.MethodGet && (path == "/v1/models" || path == "/v1/models/"):
		r.serveModels(w, req)
		return
	case req.Method == http.MethodPost && path == "/v1/messages":
		r.serveMessages(w, req)
		return
	case req.Method == http.MethodPost && path == "/v1/messages/count_tokens":
		r.serveCountTokens(w, req)
		return
	}

	logx.Debugf("passthrough %s %s", req.Method, path)
	r.proxy.ServeHTTP(w, req)
}

// serveMessages routes a Messages request either to the Codex backend or to
// Anthropic, based on the model the body names.
func (r *Router) serveMessages(w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := gjson.GetBytes(body, "model").String()
	resolution, isCodex := r.registry.Resolve(model)
	if !isCodex {
		// Reasoning state this proxy issued would be rejected by Anthropic,
		// so it is dropped when a session switches back to a Claude model.
		if cleaned, changed := translate.StripProxySignatures(body); changed {
			logx.Debugf("dropped proxy-issued thinking blocks before passing %s upstream", model)
			body = cleaned
		}
		restoreBody(req, body)
		logx.Debugf("passthrough messages model=%s", model)
		r.proxy.ServeHTTP(w, req)
		return
	}

	logx.Infof("codex messages model=%s -> %s effort=%s", model, resolution.Model.ID, effortOrDefault(resolution))
	r.serveCodexMessages(w, req, body, resolution)
}

// serveCountTokens answers token counts for Codex models locally and lets
// Anthropic answer for its own.
func (r *Router) serveCountTokens(w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if _, isCodex := r.registry.Resolve(model); !isCodex {
		if cleaned, changed := translate.StripProxySignatures(body); changed {
			body = cleaned
		}
		restoreBody(req, body)
		r.proxy.ServeHTTP(w, req)
		return
	}

	r.writeCodexTokenCount(w, body)
}

func (r *Router) serveAdmin(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/__ccmproxy/ca":
		if len(r.caPEM) == 0 {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="ccmproxy-ca.crt"`)
		_, _ = w.Write(r.caPEM)
	case "/__ccmproxy/status":
		status := map[string]any{
			"upstream":        r.cfg.Upstream,
			"intercept_hosts": r.cfg.InterceptHosts,
			"codex_models":    r.registry.AdvertisedIDs(),
			"codex_logged_in": r.backend != nil && r.backend.Store.Authenticated(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	default:
		http.NotFound(w, req)
	}
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("request has no body")
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(body) > maxRequestBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
	}
	return body, nil
}

// restoreBody puts a buffered body back on the request so it can be proxied.
func restoreBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// A buffered body is no longer chunked.
	req.TransferEncoding = nil
}

func effortOrDefault(resolution codex.Resolution) string {
	if resolution.Effort != "" {
		return resolution.Effort
	}
	return "auto"
}

// writeAnthropicError renders an error in the shape Anthropic clients expect.
func writeAnthropicError(w http.ResponseWriter, status int, kind, message string) {
	payload := map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    kind,
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
