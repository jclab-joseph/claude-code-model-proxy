package router

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func gzipBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}

func zlibBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zlib.NewWriter(&buffer)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buffer.Bytes()
}

// doEncoded sends a request whose body carries the given content coding.
func (h *harness) doEncoded(t *testing.T, target, encoding string, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestGzippedCodexRequestIsRouted(t *testing.T) {
	// Claude Code gzips request bodies once they grow, which is most real
	// sessions; an unread model name would send them all to Anthropic.
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"type":"response.created","response":{"id":"resp_gz"}}`)
		writeSSE(w, `{"type":"response.output_text.delta","delta":"Hi"}`)
		writeSSE(w, `{"type":"response.completed","response":{"id":"resp_gz","usage":{}}}`)
	}, true)

	payload := `{"model":"gpt-5.5-high","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("padding ", 2000) + `"}]}`
	rec := h.doEncoded(t, "/v1/messages", "gzip", gzipBytes(t, payload))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.codex.path != "/responses" {
		t.Fatalf("the request did not reach the Codex backend (anthropic saw %q)", h.anthropic.path)
	}
	if h.anthropic.method != "" {
		t.Fatal("a gzipped request for an added model must not go to Anthropic")
	}
	upstream := gjson.Parse(h.codex.body)
	if got := upstream.Get("model").String(); got != "gpt-5.5" {
		t.Fatalf("upstream model = %q", got)
	}
	if got := upstream.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("upstream effort = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "event: message_start") {
		t.Fatalf("stream = %s", rec.Body.String())
	}
}

func TestDeflatedCodexRequestIsRouted(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	payload := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`
	rec := h.doEncoded(t, "/v1/messages", "deflate", zlibBytes(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.codex.path != "/responses" {
		t.Fatal("a deflated request for an added model was not routed to Codex")
	}
}

func TestGzippedPassthroughKeepsTheOriginalBytes(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	payload := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`
	raw := gzipBytes(t, payload)
	rec := h.doEncoded(t, "/v1/messages", "gzip", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// Pass-through must stay byte exact: the compressed bytes and the
	// content coding both reach Anthropic as the client sent them.
	if h.anthropic.body != string(raw) {
		t.Fatalf("upstream received %d bytes, want the original %d compressed bytes",
			len(h.anthropic.body), len(raw))
	}
	if got := h.anthropic.header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("upstream Content-Encoding = %q, want gzip", got)
	}
	if got := h.anthropic.header.Get("Content-Length"); got != strconv.Itoa(len(raw)) {
		t.Fatalf("upstream Content-Length = %q, want %d", got, len(raw))
	}
}

func TestGzippedPassthroughIsDecompressedOnlyWhenRewritten(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	// A body that has to be rewritten cannot stay compressed, so the coding
	// is dropped along with it.
	payload := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"codex","signature":"ccmp1:SIG"},` +
		`{"type":"text","text":"answer"}]}]}`
	rec := h.doEncoded(t, "/v1/messages", "gzip", gzipBytes(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if got := h.anthropic.header.Get("Content-Encoding"); got != "" {
		t.Fatalf("upstream Content-Encoding = %q, want it dropped with the rewrite", got)
	}
	if !gjson.Valid(h.anthropic.body) {
		t.Fatalf("upstream body is not plain JSON: %q", h.anthropic.body[:min(80, len(h.anthropic.body))])
	}
	if strings.Contains(h.anthropic.body, "ccmp1:") {
		t.Fatalf("proxy-issued reasoning state reached Anthropic: %s", h.anthropic.body)
	}
}

func TestCountTokensAcceptsAGzippedBody(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	payload := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello there"}]}`
	rec := h.doEncoded(t, "/v1/messages/count_tokens", "gzip", gzipBytes(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "input_tokens").Int(); got < 1 {
		t.Fatalf("input_tokens = %d", got)
	}
	if h.anthropic.method != "" {
		t.Fatal("count_tokens for an added model must not reach Anthropic")
	}
}

func TestUndecodableBodyIsPassedThroughUntouched(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	// Brotli and zstd are response codings; a request body under one cannot
	// be inspected, and forwarding it beats failing the request.
	raw := []byte("\x83\x87\x01\x00 not decodable")
	rec := h.doEncoded(t, "/v1/messages", "br", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.anthropic.body != string(raw) {
		t.Fatal("an undecodable body must reach Anthropic byte for byte")
	}
	if got := h.anthropic.header.Get("Content-Encoding"); got != "br" {
		t.Fatalf("upstream Content-Encoding = %q, want br", got)
	}
}

func TestCorruptGzipIsPassedThroughUntouched(t *testing.T) {
	h := newHarness(t, nil, nil, true)

	raw := []byte("\x1f\x8b\x08 truncated")
	rec := h.doEncoded(t, "/v1/messages", "gzip", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.anthropic.body != string(raw) {
		t.Fatal("a corrupt body must reach Anthropic byte for byte")
	}
}

func TestReadBodyReportsTheDecodedForm(t *testing.T) {
	payload := `{"model":"gpt-5.5"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gzipBytes(t, payload)))
	req.Header.Set("Content-Encoding", "gzip")

	body, err := readBody(req)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if !body.Readable() || !body.Compressed() {
		t.Fatalf("body = %+v, want a readable compressed body", body)
	}
	if string(body.JSON) != payload {
		t.Fatalf("decoded = %q, want %q", body.JSON, payload)
	}
	if string(body.Raw) == payload {
		t.Fatal("Raw must keep the compressed bytes")
	}
	if body.Encoding != "gzip" {
		t.Fatalf("encoding = %q", body.Encoding)
	}
}

func TestReadBodyOnAnIdentityBody(t *testing.T) {
	payload := `{"model":"claude-opus-5"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))

	body, err := readBody(req)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if body.Compressed() {
		t.Fatal("a body with no Content-Encoding is not compressed")
	}
	// Raw and JSON are the same buffer, so nothing is copied needlessly.
	if string(body.JSON) != payload || string(body.Raw) != payload {
		t.Fatalf("body = %+v", body)
	}
}

func TestOversizedCompressedBodyIsRejected(t *testing.T) {
	// A small gzip stream that expands past the cap must not be buffered.
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for written := 0; written <= maxRequestBody; written += len(chunk) {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	if _, err := decodeBody(buffer.Bytes(), "gzip"); err == nil {
		t.Fatal("a body that decodes past the limit must be rejected")
	}
}

func TestDecodeBodyRejectsUnknownCodings(t *testing.T) {
	for _, encoding := range []string{"br", "zstd", "compress", "weird"} {
		if _, err := decodeBody([]byte("x"), encoding); err == nil {
			t.Fatalf("%q must be reported as unsupported", encoding)
		}
	}
}
