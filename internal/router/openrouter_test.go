package router

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
	"github.com/tidwall/gjson"
)

func TestOpenRouterStreamingRequest(t *testing.T) {
	h := newHarnessWithOpenRouter(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"gen-1"}}`)
		writeSSE(w, `{"type":"response.reasoning_text.delta","output_index":0,"delta":"thinking"}`)
		writeSSE(w, `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"thinking"}]}}`)
		writeSSE(w, `{"type":"response.output_text.delta","output_index":1,"delta":"Hi"}`)
		writeSSE(w, `{"type":"response.completed","response":{"id":"gen-1","usage":{"input_tokens":3,"output_tokens":1}}}`)
	}, true)

	body := `{"model":"openrouter/z-ai/glm-5.3-flash","max_tokens":1024,"stream":true,"system":"be brief","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`
	rec := h.do(t, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	stream := rec.Body.String()
	if !strings.Contains(stream, `"model":"openrouter/z-ai/glm-5.3-flash"`) {
		t.Fatalf("the client must see the model name it requested:\n%s", stream)
	}
	if !strings.Contains(stream, `"signature":"`+translate.OpenRouter.Marker+`"`) {
		t.Fatalf("the thinking block must carry the OpenRouter marker:\n%s", stream)
	}

	if h.openrouter.path != "/responses" {
		t.Fatalf("OpenRouter path = %q, want /responses", h.openrouter.path)
	}
	if got := h.openrouter.header.Get("Authorization"); got != "Bearer "+testOpenRouterKey {
		t.Fatalf("Authorization = %q", got)
	}
	// The client's Anthropic credentials must never reach OpenRouter.
	for _, name := range []string{"X-Api-Key", "Anthropic-Version"} {
		if got := h.openrouter.header.Get(name); got != "" {
			t.Fatalf("%s leaked to OpenRouter: %q", name, got)
		}
	}

	upstream := gjson.Parse(h.openrouter.body)
	if got := upstream.Get("model").String(); got != "z-ai/glm-5.3-flash" {
		t.Fatalf("upstream model = %q, want the bare OpenRouter id", got)
	}
	if got := upstream.Get("provider.data_collection").String(); got != "deny" {
		t.Fatalf("provider.data_collection = %q, want deny: %s", got, upstream.Get("provider").Raw)
	}
	if got := upstream.Get("provider.sort").String(); got != "price" {
		t.Fatalf("configured routing preferences were not sent: %s", upstream.Get("provider").Raw)
	}
	if got := upstream.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want the client's high", got)
	}
	if h.codex.method != "" || h.anthropic.method != "" {
		t.Fatal("an OpenRouter model request must reach only OpenRouter")
	}
}

func TestOpenRouterReplaysItsOwnReasoning(t *testing.T) {
	h := newHarnessWithOpenRouter(t, nil, nil, nil, true)

	body := `{"model":"z-ai/glm-5.3-flash","max_tokens":100,"stream":true,"messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"mine","signature":"` + translate.OpenRouter.Marker + `"},` +
		`{"type":"thinking","thinking":"codex","signature":"` + translate.Codex.Marker + `ENC"},` +
		`{"type":"text","text":"a"}]},` +
		`{"role":"user","content":"next"}]}`
	if rec := h.do(t, http.MethodPost, "/v1/messages", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var reasoning []string
	for _, item := range gjson.Get(h.openrouter.body, "input").Array() {
		if item.Get("type").String() == "reasoning" {
			reasoning = append(reasoning, item.Get("content.0.text").String())
		}
	}
	if len(reasoning) != 1 || reasoning[0] != "mine" {
		t.Fatalf("replayed reasoning = %v, want only OpenRouter's own", reasoning)
	}
}

func TestOpenRouterThreadContinueIsRefused(t *testing.T) {
	h := newHarnessWithOpenRouter(t, nil, nil, nil, true)

	body := `{"model":"z-ai/glm-5.3-flash","max_tokens":100,"thread":{"type":"continue","previous_message_id":"msg_011abc"},"messages":[{"role":"user","content":"go on"}]}`
	rec := h.do(t, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusBadRequest || gjson.Get(rec.Body.String(), "error.details.error_code").String() != "thread_unsupported_request" {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.openrouter.method != "" {
		t.Fatal("a refused continue must not reach OpenRouter")
	}
}

func TestOpenRouterErrorIsMapped(t *testing.T) {
	h := newHarnessWithOpenRouter(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Rate limit exceeded"}}`))
	}, true)

	rec := h.do(t, http.MethodPost, "/v1/messages", `{"model":"z-ai/glm-5.3-flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q", got)
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "Rate limit exceeded" {
		t.Fatalf("error.message = %q", got)
	}
}

func TestModelListAndStatusIncludeOpenRouter(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	rec := h.do(t, http.MethodGet, "/v1/models", "")
	var ids []string
	for _, entry := range gjson.Get(rec.Body.String(), "data").Array() {
		ids = append(ids, entry.Get("id").String())
	}
	if !strings.Contains(strings.Join(ids, " "), "z-ai/glm-5.3-flash") {
		t.Fatalf("model list = %v", ids)
	}

	status := gjson.Parse(h.do(t, http.MethodGet, "/__ccmproxy/status", "").Body.String())
	if got := status.Get(`providers.#(name=="openrouter").ready`).Bool(); !got {
		t.Fatalf("status = %s", status.Raw)
	}
	if !status.Get("codex_logged_in").Exists() {
		t.Fatalf("the codex_ keys must be kept: %s", status.Raw)
	}
}
