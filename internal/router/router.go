// Package router serves the Anthropic API surface: it forwards traffic to the
// real API untouched, extends the advertised model list, and answers requests
// for the added models from their own provider.
package router

import (
	"encoding/json"
	"fmt"
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
	case r.cfg.ExperimentalModifyBootstrapModels && req.Method == http.MethodGet && path == bootstrapPath:
		r.serveBootstrap(w, req)
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

	model := gjson.GetBytes(body.JSON, "model").String()
	resolution, isCodex := r.registry.Resolve(model)
	if !isCodex {
		if model == "" && body.Compressed() && !body.Readable() {
			// Worth saying out loud: an unreadable body is why a request for
			// an added model would silently go to Anthropic instead.
			logx.Warnf("could not read the model from a %s encoded body; routing to %s", body.Encoding, r.cfg.Upstream)
		}
		// Reasoning state this proxy issued would be rejected by Anthropic,
		// so it is dropped when a session switches back to a Claude model.
		if cleaned, changed := translate.StripProxySignatures(body.JSON); changed {
			logx.Debugf("dropped proxy-issued thinking blocks before passing %s upstream", model)
			body.forwardModified(req, cleaned)
		} else {
			body.forward(req)
		}
		logx.Debugf("passthrough messages model=%s", model)
		r.proxy.ServeHTTP(w, req)
		return
	}

	logx.Infof("codex messages model=%s -> %s effort=%s (%s body)",
		model, resolution.Model.ID, effortOrDefault(resolution), bodyEncodingLabel(body))
	r.serveCodexMessages(w, req, body.JSON, resolution)
}

// serveCountTokens answers token counts for Codex models locally and lets
// Anthropic answer for its own.
func (r *Router) serveCountTokens(w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	model := gjson.GetBytes(body.JSON, "model").String()
	if _, isCodex := r.registry.Resolve(model); !isCodex {
		if cleaned, changed := translate.StripProxySignatures(body.JSON); changed {
			body.forwardModified(req, cleaned)
		} else {
			body.forward(req)
		}
		r.proxy.ServeHTTP(w, req)
		return
	}

	r.writeCodexTokenCount(w, body.JSON)
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

// bodyEncodingLabel describes how the client sent the body, which is the
// detail that decides whether the model name can be read at all.
func bodyEncodingLabel(body *requestBody) string {
	if !body.Compressed() {
		return "identity"
	}
	return body.Encoding
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
