package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/codex"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
	"github.com/tidwall/gjson"
)

// recorded captures what a fake upstream received.
type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

// harness wires a Router against a fake Anthropic API and a fake Codex
// backend so the whole request path can be exercised without network access.
type harness struct {
	router    *Router
	anthropic *recorded
	codex     *recorded
}

// newHarness builds the harness. anthropicHandler and codexHandler produce the
// fake upstream responses; either may be nil for a default.
func newHarness(t *testing.T, anthropicHandler, codexHandler http.HandlerFunc, authenticate bool) *harness {
	t.Helper()

	h := &harness{anthropic: &recorded{}, codex: &recorded{}}

	record := func(into *recorded, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		into.method = r.Method
		into.path = r.URL.Path
		into.query = r.URL.RawQuery
		into.header = r.Header.Clone()
		into.body = string(body)
	}

	if anthropicHandler == nil {
		anthropicHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":"anthropic"}`))
		}
	}
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(h.anthropic, r)
		anthropicHandler(w, r)
	}))
	t.Cleanup(anthropicServer.Close)

	if codexHandler == nil {
		codexHandler = func(w http.ResponseWriter, r *http.Request) {
			writeSSE(w, `{"type":"response.created","response":{"id":"resp_test"}}`)
			writeSSE(w, `{"type":"response.completed","response":{"id":"resp_test","usage":{}}}`)
		}
	}
	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(h.codex, r)
		codexHandler(w, r)
	}))
	t.Cleanup(codexServer.Close)

	cfg := config.Default()
	cfg.Upstream = anthropicServer.URL
	cfg.Codex.BaseURL = codexServer.URL
	cfg.AuthDir = t.TempDir()

	if authenticate {
		token := codex.Token{
			AccessToken: "test-access-token",
			AccountID:   "acct_test",
			Email:       "user@example.com",
			PlanType:    "pro",
			ExpiresAt:   time.Now().Add(24 * time.Hour),
			Type:        "codex",
		}
		data, err := json.Marshal(token)
		if err != nil {
			t.Fatalf("marshal token: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cfg.AuthDir, "codex.json"), data, 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
	}

	oauth := codex.NewClient(nil)
	store := codex.NewStore(oauth, codex.StoreOptions{Dir: cfg.AuthDir})
	backend := codex.NewBackend(cfg.Codex.BaseURL, "test-agent", "codex-tui", store, nil)
	registry := codex.NewRegistry(nil, "", true)

	router, err := New(cfg, registry, backend, nil, []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.router = router
	return h
}

func writeSSE(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(payload, "type").String(), payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *harness) do(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "sk-ant-test")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestModelListMergesCodexModels(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z"}
		],"has_more":false,"first_id":"claude-opus-5","last_id":"claude-opus-5"}`))
	}, nil, true)

	rec := h.do(t, http.MethodGet, "/v1/models?limit=1000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	ids := map[string]string{}
	gjson.Get(rec.Body.String(), "data").ForEach(func(_, model gjson.Result) bool {
		ids[model.Get("id").String()] = model.Get("display_name").String()
		return true
	})

	if _, ok := ids["claude-opus-5"]; !ok {
		t.Fatalf("upstream models were dropped: %v", ids)
	}
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Fatalf("codex models were not added: %v", ids)
	}
	if got := ids["gpt-5.5-high"]; !strings.Contains(got, "high") {
		t.Fatalf("effort variant display name = %q, want it to mention the effort", got)
	}
	if got := gjson.Get(rec.Body.String(), "first_id").String(); got != "claude-opus-5" {
		t.Fatalf("first_id = %q", got)
	}
	if got := gjson.Get(rec.Body.String(), "last_id").String(); got == "claude-opus-5" {
		t.Fatal("last_id was not updated after appending models")
	}
}

func TestModelListFallsBackToCodexOnlyWhenUpstreamRejects(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"no key"}}`))
	}, nil, true)

	rec := h.do(t, http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so the added models stay usable: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"gpt-5.5"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestModelListForwardsUpstreamErrorWithoutCodexLogin(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"no key"}}`))
	}, nil, false)

	rec := h.do(t, http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 forwarded", rec.Code)
	}
}

func TestModelListPaginatedPageIsPassedThrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"claude-sonnet-5"}],"has_more":false}`))
	}, nil, true)

	rec := h.do(t, http.MethodGet, "/v1/models?after_id=claude-opus-5", "")
	if strings.Contains(rec.Body.String(), "gpt-5.5") {
		t.Fatal("models must not be appended to a cursor page, which would duplicate them")
	}
}

func TestClaudeModelRequestIsPassedThroughByteForByte(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	body := `{"model":"claude-opus-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`
	rec := h.do(t, http.MethodPost, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.anthropic.body != body {
		t.Fatalf("upstream body was altered:\n got %s\nwant %s", h.anthropic.body, body)
	}
	if h.anthropic.path != "/v1/messages" {
		t.Fatalf("upstream path = %q", h.anthropic.path)
	}
	if got := h.anthropic.header.Get("x-api-key"); got != "sk-ant-test" {
		t.Fatalf("client credentials were not forwarded: %q", got)
	}
	if got := h.anthropic.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	if h.codex.method != "" {
		t.Fatal("a Claude model request must never reach the Codex backend")
	}
}

func TestUnknownPathIsPassedThrough(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	rec := h.do(t, http.MethodPost, "/v1/complete?beta=true", `{"prompt":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.anthropic.path != "/v1/complete" || h.anthropic.query != "beta=true" {
		t.Fatalf("upstream saw %s?%s", h.anthropic.path, h.anthropic.query)
	}
	if h.anthropic.body != `{"prompt":"x"}` {
		t.Fatalf("upstream body = %q", h.anthropic.body)
	}
}

func TestPassthroughDropsProxyIssuedThinking(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	body := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"codex","signature":"` + translate.SignatureMarker + `SIG"},` +
		`{"type":"text","text":"answer"}]}]}`
	h.do(t, http.MethodPost, "/v1/messages", body)

	if strings.Contains(h.anthropic.body, translate.SignatureMarker) {
		t.Fatalf("proxy-issued reasoning state reached Anthropic: %s", h.anthropic.body)
	}
	blocks := gjson.Get(h.anthropic.body, "messages.0.content").Array()
	if len(blocks) != 1 || blocks[0].Get("text").String() != "answer" {
		t.Fatalf("upstream body = %s", h.anthropic.body)
	}
}

func TestCodexStreamingRequest(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"resp_1"}}`)
		writeSSE(w, `{"type":"response.output_text.delta","delta":"Hi"}`)
		writeSSE(w, `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":1}}}`)
	}, true)

	body := `{"model":"gpt-5.5-high","max_tokens":1024,"stream":true,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`
	rec := h.do(t, http.MethodPost, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}

	stream := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream is missing %q:\n%s", want, stream)
		}
	}
	if !strings.Contains(stream, `"model":"gpt-5.5-high"`) {
		t.Fatalf("the client must see the model name it requested:\n%s", stream)
	}

	// Verify what the Codex backend received.
	if h.codex.path != "/responses" {
		t.Fatalf("codex path = %q, want /responses", h.codex.path)
	}
	upstream := gjson.Parse(h.codex.body)
	if got := upstream.Get("model").String(); got != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want the bare slug", got)
	}
	if got := upstream.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("upstream effort = %q, want high from the model suffix", got)
	}
	if got := upstream.Get("input.0.role").String(); got != "developer" {
		t.Fatalf("system prompt was not folded into a developer message: %s", h.codex.body)
	}
	if got := h.codex.header.Get("Authorization"); got != "Bearer test-access-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.codex.header.Get("Chatgpt-Account-Id"); got != "acct_test" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := h.codex.header.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q", got)
	}
	if h.codex.header.Get("Session_id") == "" {
		t.Fatal("Session_id header is missing")
	}
	if h.anthropic.method != "" {
		t.Fatal("a Codex model request must never reach Anthropic")
	}
}

func TestCodexNonStreamingRequest(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"resp_2"}}`)
		writeSSE(w, `{"type":"response.completed","response":{"id":"resp_2","output":[{"type":"message","content":[{"type":"output_text","text":"buffered answer"}]}],"usage":{"input_tokens":9,"output_tokens":2}}}`)
	}, true)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON for a non-streaming request", got)
	}

	message := gjson.Parse(rec.Body.String())
	if got := message.Get("content.0.text").String(); got != "buffered answer" {
		t.Fatalf("content = %s", rec.Body.String())
	}
	if got := message.Get("stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q", got)
	}
	// The upstream request still streams, because that is all Codex serves.
	if !gjson.Get(h.codex.body, "stream").Bool() {
		t.Fatal("the upstream request must stream")
	}
}

func TestCodexThreadContinueIsRefused(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	// What Claude Code sends after a switch from a Claude model: no system
	// prompt and no tools, because it expects Anthropic to supply them.
	body := `{"model":"gpt-5.5","max_tokens":100,"stream":true,"thread":{"type":"continue","previous_message_id":"msg_011abc"},"messages":[{"role":"user","content":"go on"}]}`
	rec := h.do(t, http.MethodPost, "/v1/messages", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Claude Code keys its stateless resend on this exact code.
	if got := gjson.Get(rec.Body.String(), "error.details.error_code").String(); got != "thread_unsupported_request" {
		t.Fatalf("error_code = %q, want thread_unsupported_request: %s", got, rec.Body.String())
	}
	if h.codex.method != "" || h.anthropic.method != "" {
		t.Fatal("a refused continue must not reach any upstream")
	}
}

func TestCodexThreadCreateIsServed(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	body := `{"model":"gpt-5.5","max_tokens":100,"stream":true,"thread":{"type":"create"},"messages":[{"role":"user","content":"hi"}]}`
	if rec := h.do(t, http.MethodPost, "/v1/messages", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.codex.method == "" {
		t.Fatal("a thread create carries the whole conversation and must reach Codex")
	}
}

func TestClaudeThreadContinueIsPassedThrough(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	body := `{"model":"claude-opus-5","max_tokens":100,"thread":{"type":"continue","previous_message_id":"msg_011abc"},"messages":[{"role":"user","content":"go on"}]}`
	if rec := h.do(t, http.MethodPost, "/v1/messages", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.anthropic.body != body {
		t.Fatalf("Anthropic must receive the continue unchanged, got %s", h.anthropic.body)
	}
}

func TestCodexBackendErrorIsMapped(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"usage limit reached"}}`))
	}, true)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","stream":true,"messages":[]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 preserved", rec.Code)
	}
	body := gjson.Parse(rec.Body.String())
	if got := body.Get("error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q", got)
	}
	if got := body.Get("error.message").String(); got != "usage limit reached" {
		t.Fatalf("error.message = %q", got)
	}
}

func TestCodexRequestWithoutCredentials(t *testing.T) {
	h := newHarness(t, nil, nil, false)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","messages":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no Codex credentials are stored", rec.Code)
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "authentication_error" {
		t.Fatalf("error.type = %q", got)
	}
}

func TestCountTokensForCodexModelIsAnsweredLocally(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	rec := h.do(t, http.MethodPost, "/v1/messages/count_tokens", `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello there friend"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "input_tokens").Int(); got < 1 {
		t.Fatalf("input_tokens = %d, want a positive estimate", got)
	}
	if h.anthropic.method != "" {
		t.Fatal("count_tokens for a Codex model must not reach Anthropic")
	}
}

func TestCountTokensForClaudeModelIsPassedThrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":17}`))
	}, nil, true)

	rec := h.do(t, http.MethodPost, "/v1/messages/count_tokens", `{"model":"claude-opus-5","messages":[]}`)
	if got := gjson.Get(rec.Body.String(), "input_tokens").Int(); got != 17 {
		t.Fatalf("input_tokens = %d, want the upstream answer", got)
	}
	if h.anthropic.path != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path = %q", h.anthropic.path)
	}
}

func TestAdminEndpoints(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	rec := h.do(t, http.MethodGet, "/__ccmproxy/ca", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("CA endpoint returned %d: %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodGet, "/__ccmproxy/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d", rec.Code)
	}
	if !gjson.Get(rec.Body.String(), "codex_logged_in").Bool() {
		t.Fatalf("status = %s", rec.Body.String())
	}
	if h.anthropic.method != "" {
		t.Fatal("admin endpoints must not reach Anthropic")
	}
}

func TestCodexNonStreamingWhenTerminalEventOmitsOutput(t *testing.T) {
	// This is the shape the production Codex backend actually sends: the
	// items are streamed and response.completed does not repeat them.
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"resp_real"}}`)
		writeSSE(w, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call_1","name":"Read"}}`)
		writeSSE(w, `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc","delta":"{\"file_path\":\"/tmp/x\"}"}`)
		writeSSE(w, `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}`)
		writeSSE(w, `{"type":"response.completed","response":{"id":"resp_real","usage":{"input_tokens":64,"output_tokens":32}}}`)
	}, true)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","max_tokens":300,"messages":[{"role":"user","content":"read it"}],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	message := gjson.Parse(rec.Body.String())
	blocks := message.Get("content").Array()
	if len(blocks) != 1 {
		t.Fatalf("content = %s, want the streamed tool call", message.Get("content").Raw)
	}
	if blocks[0].Get("type").String() != "tool_use" || blocks[0].Get("name").String() != "Read" {
		t.Fatalf("block = %s", blocks[0].Raw)
	}
	if got := blocks[0].Get("input.file_path").String(); got != "/tmp/x" {
		t.Fatalf("input.file_path = %q", got)
	}
	if got := message.Get("stop_reason").String(); got != "tool_use" {
		t.Fatalf("stop_reason = %q", got)
	}
	if got := message.Get("usage.output_tokens").Int(); got != 32 {
		t.Fatalf("output_tokens = %d", got)
	}
}

func TestCodexStreamErrorMidStream(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"resp_err"}}`)
		writeSSE(w, `{"type":"error","error":{"type":"overloaded_error","message":"try again"}}`)
	}, true)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","stream":false,"messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an error that arrived mid-stream", rec.Code)
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "try again" {
		t.Fatalf("error.message = %q", got)
	}
}

// codexUpstream returns the body the Codex backend received.
func (h *harness) codexUpstream(t *testing.T) gjson.Result {
	t.Helper()
	if h.codex.body == "" {
		t.Fatal("the Codex backend received no request")
	}
	return gjson.Parse(h.codex.body)
}

func TestClientEffortIsForwarded(t *testing.T) {
	// This is how Claude Code's /effort and --effort arrive, so a bare model
	// id is enough to control effort; per-effort ids are not needed.
	for _, effort := range []string{"low", "medium", "high", "xhigh"} {
		h := newHarness(t, nil, nil, true)
		h.do(t, http.MethodPost, "/v1/messages", `{
			"model":"gpt-5.5",
			"max_tokens":100,
			"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"adaptive"},
			"output_config":{"effort":"`+effort+`"}
		}`)
		if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != effort {
			t.Fatalf("output_config.effort %q became reasoning.effort %q", effort, got)
		}
	}
}

func TestClientEffortIsClampedToWhatTheModelSupports(t *testing.T) {
	// Claude Code offers the effort levels of the model the picker row says
	// to behave as, which can exceed the real model's range.
	h := newHarness(t, nil, nil, true)
	h.do(t, http.MethodPost, "/v1/messages", `{
		"model":"gpt-5.5",
		"messages":[],
		"output_config":{"effort":"max"}
	}`)
	// gpt-5.5 stops at xhigh.
	if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want it clamped to xhigh", got)
	}

	// A model that does support max passes it through.
	h = newHarness(t, nil, nil, true)
	h.do(t, http.MethodPost, "/v1/messages", `{
		"model":"gpt-6-astra",
		"messages":[],
		"output_config":{"effort":"max"}
	}`)
	if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max", got)
	}
}

func TestUnknownClientEffortIsClamped(t *testing.T) {
	// "ultra" exists in Claude Code but in no Codex model.
	h := newHarness(t, nil, nil, true)
	h.do(t, http.MethodPost, "/v1/messages", `{
		"model":"gpt-6-astra",
		"messages":[],
		"output_config":{"effort":"ultra"}
	}`)
	if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want the highest supported level", got)
	}
}

func TestModelNameEffortOverridesTheClient(t *testing.T) {
	// A per-effort id is still honoured, and is an explicit override.
	h := newHarness(t, nil, nil, true)
	h.do(t, http.MethodPost, "/v1/messages", `{
		"model":"gpt-5.5-low",
		"messages":[],
		"output_config":{"effort":"high"}
	}`)
	if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want the model name to win", got)
	}
}

func TestEffortFallsBackWithoutAnyClientPreference(t *testing.T) {
	h := newHarness(t, nil, nil, true)
	h.do(t, http.MethodPost, "/v1/messages", `{"model":"gpt-5.5","messages":[]}`)
	if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != "medium" {
		t.Fatalf("reasoning.effort = %q, want the model default", got)
	}
}

func TestResolveEffortAlwaysReturnsAConcreteLevel(t *testing.T) {
	registry := codex.NewRegistry(nil, "", true)
	resolve := func(model, body string) string {
		resolution, ok := registry.Resolve(model)
		if !ok {
			t.Fatalf("%q is not a Codex model", model)
		}
		return resolveEffort([]byte(body), resolution)
	}

	cases := []struct {
		name  string
		model string
		body  string
		want  string
	}{
		{"model default", "gpt-5.5", `{}`, "medium"},
		{"client choice", "gpt-5.5", `{"output_config":{"effort":"high"}}`, "high"},
		{"model name pin", "gpt-5.5-low", `{"output_config":{"effort":"high"}}`, "low"},
		{"clamped client choice", "gpt-5.5", `{"output_config":{"effort":"max"}}`, "xhigh"},
		{"thinking budget", "gpt-5.5", `{"thinking":{"type":"enabled","budget_tokens":40000}}`, "xhigh"},
		{"thinking disabled", "gpt-5.5", `{"thinking":{"type":"disabled"}}`, "low"},
		{"adaptive with no effort", "gpt-5.5", `{"thinking":{"type":"adaptive"}}`, "xhigh"},
		{"unparseable body", "gpt-5.5", `not json`, "medium"},
	}
	for _, c := range cases {
		got := resolve(c.model, c.body)
		if got != c.want {
			t.Fatalf("%s: resolveEffort = %q, want %q", c.name, got, c.want)
		}
		// An empty result would silently hand the decision to a second
		// place, which is the bug this consolidation removes.
		if got == "" {
			t.Fatalf("%s: resolveEffort returned no level", c.name)
		}
	}
}

func TestTheEffortSentUpstreamIsTheOneResolved(t *testing.T) {
	registry := codex.NewRegistry(nil, "", true)

	for _, body := range []string{
		`{"model":"gpt-5.5","messages":[]}`,
		`{"model":"gpt-5.5","messages":[],"output_config":{"effort":"high"}}`,
		`{"model":"gpt-5.5","messages":[],"output_config":{"effort":"max"}}`,
		`{"model":"gpt-5.5-low","messages":[],"output_config":{"effort":"high"}}`,
		`{"model":"gpt-6-astra","messages":[],"output_config":{"effort":"ultra"}}`,
		`{"model":"gpt-5.5","messages":[],"thinking":{"type":"enabled","budget_tokens":2048}}`,
	} {
		model := gjson.Get(body, "model").String()
		resolution, ok := registry.Resolve(model)
		if !ok {
			t.Fatalf("%q is not a Codex model", model)
		}
		want := resolveEffort([]byte(body), resolution)

		h := newHarness(t, nil, nil, true)
		h.do(t, http.MethodPost, "/v1/messages", body)
		if got := h.codexUpstream(t).Get("reasoning.effort").String(); got != want {
			t.Fatalf("%s: sent reasoning.effort %q but resolveEffort said %q", body, got, want)
		}
	}
}
