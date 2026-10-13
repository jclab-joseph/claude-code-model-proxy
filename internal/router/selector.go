package router

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// selectorPath matches the served model catalog newer Claude Code versions
// fetch for their /model picker. "cc" is Claude Code's surface.
var selectorPath = regexp.MustCompile(`^/api/(?:oauth/)?organizations/[^/]+/model_selector/cc$`)

// selectorSurface is the model_selector_config entry Claude Code reads.
const selectorSurface = "cc"

// serveModelSelector adds this proxy's models to the served model catalog.
//
// Once an account is served a catalog, Claude Code builds the /model picker
// from it alone and ignores the bootstrap response's additional_model_options,
// so the bootstrap rewrite on its own no longer puts the models in the picker.
// It is part of the same experiment as serveBootstrap, with the same rule: a
// response that is not what this code expects is forwarded untouched.
func (r *Router) serveModelSelector(w http.ResponseWriter, req *http.Request) {
	body, status, header, err := r.fetchUpstream(req, http.MethodGet)
	if err != nil {
		logx.Warnf("model selector: upstream request failed: %v", err)
		r.proxy.ServeHTTP(w, req)
		return
	}

	patched, ok := r.injectSelectorModels(body, status, header)
	if !ok {
		copyHeader(w.Header(), header)
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	copyHeader(w.Header(), header)
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(patched)
}

// injectSelectorModels appends the added models to the Claude Code surface's
// model list. The second return value is false when the response must be
// passed through as it is.
func (r *Router) injectSelectorModels(body []byte, status int, header http.Header) ([]byte, bool) {
	if status < 200 || status >= 300 {
		return nil, false
	}
	if encoding := strings.TrimSpace(header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		logx.Warnf("model selector: leaving the response alone, it is %s encoded", encoding)
		return nil, false
	}
	if !gjson.ValidBytes(body) {
		logx.Warnf("model selector: leaving the response alone, it is not JSON")
		return nil, false
	}

	var rows []catalog.SelectorRow
	for _, provider := range r.providers {
		rows = append(rows, provider.Registry.SelectorRows()...)
	}
	if len(rows) == 0 {
		return nil, false
	}

	surfaces := gjson.GetBytes(body, "model_selector_config")
	if !surfaces.IsArray() {
		logx.Warnf("model selector: leaving the response alone, model_selector_config is %s", surfaces.Type)
		return nil, false
	}
	index := -1
	surfaces.ForEach(func(i, surface gjson.Result) bool {
		if surface.Get("id").String() == selectorSurface {
			index = int(i.Int())
			return false
		}
		return true
	})
	if index < 0 {
		logx.Warnf("model selector: leaving the response alone, it has no %q surface", selectorSurface)
		return nil, false
	}
	modelsPath := "model_selector_config." + strconv.Itoa(index) + ".models"
	existing := gjson.GetBytes(body, modelsPath)
	if existing.Exists() && existing.Type != gjson.Null && !existing.IsArray() {
		logx.Warnf("model selector: leaving the response alone, models is %s", existing.Type)
		return nil, false
	}

	// The account's own rows win and are never duplicated.
	present := map[string]bool{}
	existing.ForEach(func(_, row gjson.Result) bool {
		present[row.Get("id").String()] = true
		return true
	})

	patched := body
	if !existing.IsArray() {
		var err error
		if patched, err = sjson.SetRawBytes(patched, modelsPath, []byte(`[]`)); err != nil {
			logx.Warnf("model selector: could not initialise models: %v", err)
			return nil, false
		}
	}
	added := 0
	for _, row := range rows {
		if present[row.ID] {
			continue
		}
		entry, err := json.Marshal(row)
		if err != nil {
			logx.Warnf("model selector: could not encode %s: %v", row.ID, err)
			return nil, false
		}
		if patched, err = sjson.SetRawBytes(patched, modelsPath+".-1", entry); err != nil {
			logx.Warnf("model selector: could not add %s: %v", row.ID, err)
			return nil, false
		}
		added++
	}
	if added == 0 {
		return nil, false
	}

	logx.Infof("model selector: added %d models to the /model picker", added)
	return patched, true
}
