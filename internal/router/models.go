package router

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// addedModelCreatedAt is the created_at stamp reported for the added models.
// The list endpoint's schema requires the field; the value carries no meaning
// for models that are not Anthropic's.
var addedModelCreatedAt = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

// serveModels answers the model list with the upstream catalog plus the models
// this proxy adds.
func (r *Router) serveModels(w http.ResponseWriter, req *http.Request) {
	upstreamBody, upstreamStatus, upstreamHeader, err := r.fetchUpstream(req, http.MethodGet)

	// Pagination cursors mean this is not the first page; extending it would
	// duplicate entries, so such pages are passed through as they are.
	paginated := req.URL.Query().Get("after_id") != "" || req.URL.Query().Get("before_id") != ""

	switch {
	case err != nil, upstreamStatus < 200 || upstreamStatus >= 300:
		// Anthropic credentials may be absent or invalid while the added
		// provider is perfectly usable, so the added models are still
		// listed rather than surfacing an empty picker.
		if err != nil {
			logx.Warnf("models: upstream request failed: %v", err)
		} else {
			logx.Warnf("models: upstream returned %d", upstreamStatus)
		}
		if r.anyReady() {
			r.writeModelList(w, nil)
			return
		}
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "model list request failed: "+err.Error())
			return
		}
		copyHeader(w.Header(), upstreamHeader)
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write(upstreamBody)
		return

	case paginated:
		copyHeader(w.Header(), upstreamHeader)
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write(upstreamBody)
		return
	}

	r.writeModelList(w, upstreamBody)
}

// writeModelList renders the merged list. upstreamBody may be nil, in which
// case only the added models are reported.
func (r *Router) writeModelList(w http.ResponseWriter, upstreamBody []byte) {
	body := upstreamBody
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "data").IsArray() {
		body = []byte(`{"data":[],"has_more":false,"first_id":null,"last_id":null}`)
	}

	entries := r.addedModelEntries()
	for _, entry := range entries {
		body, _ = sjson.SetRawBytes(body, "data.-1", entry)
	}

	data := gjson.GetBytes(body, "data").Array()
	if len(data) > 0 {
		body, _ = sjson.SetBytes(body, "first_id", data[0].Get("id").String())
		body, _ = sjson.SetBytes(body, "last_id", data[len(data)-1].Get("id").String())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// addedModelEntries renders every provider's models as Anthropic model
// objects.
func (r *Router) addedModelEntries() [][]byte {
	entries := make([][]byte, 0, 16)
	for _, provider := range r.providers {
		for _, id := range provider.Registry.AdvertisedIDs() {
			entry := []byte(`{"type":"model","id":"","display_name":"","created_at":""}`)
			entry, _ = sjson.SetBytes(entry, "id", id)
			entry, _ = sjson.SetBytes(entry, "display_name", displayNameFor(provider.Registry, id))
			entry, _ = sjson.SetBytes(entry, "created_at", addedModelCreatedAt)
			entries = append(entries, entry)
		}
	}
	return entries
}

// displayNameFor labels a model id, appending the reasoning effort when the id
// is an effort variant.
func displayNameFor(registry *catalog.Registry, id string) string {
	resolution, ok := registry.Resolve(id)
	if !ok {
		return id
	}
	if resolution.Effort == "" {
		return resolution.Model.DisplayName
	}
	return resolution.Model.DisplayName + " · " + resolution.Effort
}

// fetchUpstream forwards a bodyless request to the upstream API and buffers
// the response, so the proxy can inspect and rewrite it.
func (r *Router) fetchUpstream(req *http.Request, method string) (body []byte, status int, header http.Header, err error) {
	target := r.upstream.String() + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}

	outbound, err := http.NewRequestWithContext(req.Context(), method, target, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	for name, values := range req.Header {
		if isHopByHop(name) {
			continue
		}
		for _, value := range values {
			outbound.Header.Add(name, value)
		}
	}
	// The buffered response is decoded here, so ask for it uncompressed.
	outbound.Header.Del("Accept-Encoding")
	outbound.Host = r.upstream.Host

	resp, err := r.client.Do(outbound)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, nil, err
	}
	return body, resp.StatusCode, resp.Header, nil
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		if isHopByHop(name) || strings.EqualFold(name, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func isHopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "host":
		return true
	}
	return false
}
