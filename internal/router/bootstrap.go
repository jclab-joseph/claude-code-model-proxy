package router

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// bootstrapPath is the endpoint Claude Code fetches its per-account startup
// data from. Among other things it carries additional_model_options, which is
// what populates the extra rows of the /model picker — the model list endpoint
// plays no part in that.
const bootstrapPath = "/api/claude_cli/bootstrap"

// serveBootstrap adds this proxy's models to the bootstrap response so they
// appear in the /model picker without any client-side configuration.
//
// This rewrites a response that belongs to Claude Code rather than to the
// Anthropic API proper, which is why it is experimental and off by default:
// the shape is undocumented and can change with any Claude Code release. If
// anything about the response is not what this code expects, it is forwarded
// untouched — a broken bootstrap response would take out far more than the
// model picker.
func (r *Router) serveBootstrap(w http.ResponseWriter, req *http.Request) {
	body, status, header, err := r.fetchUpstream(req, http.MethodGet)
	if err != nil {
		logx.Warnf("bootstrap: upstream request failed: %v", err)
		r.proxy.ServeHTTP(w, req)
		return
	}

	patched, ok := r.injectBootstrapModels(body, status, header)
	if !ok {
		copyHeader(w.Header(), header)
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	copyHeader(w.Header(), header)
	// The body was decoded and rewritten, so any encoding the upstream
	// declared no longer describes it.
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(patched)
}

// injectBootstrapModels appends the added models to additional_model_options.
// The second return value is false when the response must be passed through
// as it is.
func (r *Router) injectBootstrapModels(body []byte, status int, header http.Header) ([]byte, bool) {
	if status < 200 || status >= 300 {
		return nil, false
	}
	// A body still under a content coding cannot be parsed; forwarding it
	// unchanged is the only safe move.
	if encoding := strings.TrimSpace(header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		logx.Warnf("bootstrap: leaving the response alone, it is %s encoded", encoding)
		return nil, false
	}
	if !gjson.ValidBytes(body) {
		logx.Warnf("bootstrap: leaving the response alone, it is not JSON")
		return nil, false
	}

	options := r.registry.BootstrapOptions()
	if len(options) == 0 {
		return nil, false
	}

	existing := gjson.GetBytes(body, "additional_model_options")
	// The field is null when the account has no extra models, and an array
	// when it has; anything else means the shape changed under us.
	if existing.Exists() && existing.Type != gjson.Null && !existing.IsArray() {
		logx.Warnf("bootstrap: leaving the response alone, additional_model_options is %s", existing.Type)
		return nil, false
	}

	// Models the response already offers win: the account's own entries are
	// authoritative and must not be duplicated.
	present := map[string]bool{}
	existing.ForEach(func(_, option gjson.Result) bool {
		present[option.Get("model").String()] = true
		return true
	})

	patched := body
	if !existing.IsArray() {
		var err error
		if patched, err = sjson.SetRawBytes(patched, "additional_model_options", []byte(`[]`)); err != nil {
			logx.Warnf("bootstrap: could not initialise additional_model_options: %v", err)
			return nil, false
		}
	}

	added := 0
	for _, option := range options {
		if present[option.Model] {
			continue
		}
		entry, err := json.Marshal(option)
		if err != nil {
			logx.Warnf("bootstrap: could not encode %s: %v", option.Model, err)
			return nil, false
		}
		if patched, err = sjson.SetRawBytes(patched, "additional_model_options.-1", entry); err != nil {
			logx.Warnf("bootstrap: could not add %s: %v", option.Model, err)
			return nil, false
		}
		added++
	}
	if added == 0 {
		return nil, false
	}

	logx.Infof("bootstrap: added %d models to the /model picker", added)
	return patched, true
}
