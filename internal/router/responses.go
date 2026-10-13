package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/codex"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/openrouter"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/tokenize"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
	"github.com/tidwall/gjson"
)

// sseReadLimit caps a single upstream event payload.
const sseReadLimit = 32 << 20

// resolveEffort decides the reasoning effort for one request, and is the only
// place that decides it.
//
// The order is: an effort pinned in the model name, then whatever the client
// asked for, then the model's default. Claude Code sends its /effort and
// --effort choice as output_config.effort, which is why a per-effort model id
// is not needed to control it. The result is always a concrete level, clamped
// to what the real model supports.
func resolveEffort(body []byte, resolution catalog.Resolution) string {
	effort := resolution.Effort
	if effort == "" {
		effort = translate.EffortFrom(body, resolution.Model.DefaultEffort)
	}
	return resolution.Model.ClampEffort(effort)
}

// serveProviderMessages answers a Messages request from the provider that
// owns the model. effort comes from resolveEffort, so the value logged
// upstream and the value sent are the same one.
func (r *Router) serveProviderMessages(w http.ResponseWriter, req *http.Request, provider *Provider, body []byte, resolution catalog.Resolution, effort string) {
	if provider.Upstream == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "the "+provider.Name+" provider is not configured")
		return
	}

	if logx.Enabled(logx.LevelDebug) {
		logx.Debugf("%s effort: model name=%q client=%q -> %q (supported: %s)",
			provider.Name, resolution.Effort, gjson.GetBytes(body, "output_config.effort").String(),
			effort, strings.Join(resolution.Model.ReasoningLevel, ","))
	}

	translated, err := translate.ClaudeToCodex(translate.Options{
		Model:         resolution.Model.ID,
		Effort:        effort,
		DefaultEffort: resolution.Model.DefaultEffort,
		Dialect:       provider.Dialect,
	}, body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Reusing the client's session id keeps a conversation on one upstream
	// session; Claude Code sends one per session in its metadata.
	sessionID := gjson.GetBytes(body, "metadata.user_id").String()
	if sessionID == "" {
		sessionID = codex.NewSessionID()
	}

	resp, err := provider.Upstream.Responses(req.Context(), translated.Body, sessionID)
	if err != nil {
		writeUpstreamError(w, provider.Name, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	streaming := gjson.GetBytes(body, "stream").Bool()
	if streaming {
		r.streamResponses(w, req, resp.Body, provider, resolution, translated.ToolNames)
		return
	}
	r.bufferResponses(w, resp.Body, provider, resolution, translated.ToolNames)
}

// streamResponses relays the upstream event stream as Anthropic SSE.
func (r *Router) streamResponses(w http.ResponseWriter, req *http.Request, upstream io.Reader, provider *Provider, resolution catalog.Resolution, toolNames map[string]string) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// Buffering proxies would defeat the point of streaming.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	_ = controller.Flush()

	translator := provider.Dialect.NewStreamTranslator(resolution.Requested, toolNames)
	write := func(frames []byte) bool {
		if len(frames) == 0 {
			return true
		}
		if _, err := w.Write(frames); err != nil {
			logx.Debugf("%s stream: client write failed: %v", provider.Name, err)
			return false
		}
		_ = controller.Flush()
		return true
	}

	err := scanSSE(upstream, func(payload []byte) bool {
		return write(translator.Event(payload))
	})
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, req.Context().Err()) {
		logx.Warnf("%s stream: upstream read failed: %v", provider.Name, err)
		write(translator.Error("api_error", "upstream stream ended: "+err.Error()))
	}
	write(translator.Close())
}

// bufferResponses answers a client that did not ask for streaming. The
// upstream request always streams, so the events go through the same
// translator as the streaming path and are then folded into one message.
func (r *Router) bufferResponses(w http.ResponseWriter, upstream io.Reader, provider *Provider, resolution catalog.Resolution, toolNames map[string]string) {
	translator := provider.Dialect.NewStreamTranslator(resolution.Requested, toolNames)
	assembler := translate.NewMessageAssembler()

	err := scanSSE(upstream, func(payload []byte) bool {
		assembler.Write(translator.Event(payload))
		return true
	})
	if err != nil && !errors.Is(err, io.EOF) {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream stream ended: "+err.Error())
		return
	}
	assembler.Write(translator.Close())

	if kind, message, failed := assembler.Err(); failed {
		writeAnthropicError(w, http.StatusBadGateway, kind, message)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(assembler.Message())
}

// writeLocalTokenCount answers count_tokens for an added model with a local
// estimate, since no provider exposes a counting endpoint.
func (r *Router) writeLocalTokenCount(w http.ResponseWriter, body []byte) {
	count := tokenize.EstimateClaudeRequest(body)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"input_tokens": count})
}

// statusError is a non-2xx answer from a provider's upstream.
type statusError interface {
	error
	HTTPStatus() int
	ResponseBody() []byte
}

// writeUpstreamError maps a provider failure onto an Anthropic error response.
func writeUpstreamError(w http.ResponseWriter, providerName string, err error) {
	if errors.Is(err, codex.ErrNotAuthenticated) || errors.Is(err, openrouter.ErrNoAPIKey) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}

	var apiErr statusError
	if errors.As(err, &apiErr) {
		status := apiErr.HTTPStatus()
		kind := "api_error"
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = "authentication_error"
		case http.StatusTooManyRequests:
			kind = "rate_limit_error"
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			kind = "invalid_request_error"
		}
		message := string(bytes.TrimSpace(apiErr.ResponseBody()))
		// Surface the upstream message rather than its JSON envelope when
		// one is available.
		if text := gjson.Get(message, "error.message").String(); text != "" {
			message = text
		} else if text := gjson.Get(message, "detail").String(); text != "" {
			message = text
		}
		if message == "" {
			message = http.StatusText(status)
		}
		logx.Warnf("%s backend error %d: %s", providerName, status, message)
		writeAnthropicError(w, status, kind, message)
		return
	}

	logx.Errorf("%s request failed: %v", providerName, err)
	writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
}

// scanSSE reads a Server-Sent Events stream and invokes onData once per event
// with the concatenated data payload. It stops early when onData returns
// false, which happens when the client has gone away.
func scanSSE(upstream io.Reader, onData func(payload []byte) bool) error {
	reader := bufio.NewReaderSize(upstream, 64<<10)
	var payload []byte

	flush := func() bool {
		if len(payload) == 0 {
			return true
		}
		data := payload
		payload = nil
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return true
		}
		return onData(data)
	}

	for {
		line, err := readSSELine(reader)
		if len(line) > 0 || err == nil {
			trimmed := bytes.TrimRight(line, "\r")
			switch {
			case len(trimmed) == 0:
				if !flush() {
					return nil
				}
			case bytes.HasPrefix(trimmed, []byte("data:")):
				value := bytes.TrimPrefix(trimmed, []byte("data:"))
				value = bytes.TrimPrefix(value, []byte(" "))
				if len(payload) > 0 {
					payload = append(payload, '\n')
				}
				payload = append(payload, value...)
			default:
				// event:, id: and comment lines carry no data the
				// translator needs; the payload's own type field is used.
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !flush() {
					return nil
				}
				return nil
			}
			return err
		}
	}
}

// readSSELine reads one line without the trailing newline, joining the pieces
// of a line longer than the reader's buffer.
func readSSELine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > sseReadLimit {
			return nil, errors.New("sse: event line exceeds limit")
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return bytes.TrimSuffix(line, []byte("\n")), err
		}
		return bytes.TrimSuffix(line, []byte("\n")), nil
	}
}
