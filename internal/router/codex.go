package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/codex"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/tokenize"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
	"github.com/tidwall/gjson"
)

// sseReadLimit caps a single upstream event payload.
const sseReadLimit = 32 << 20

// serveCodexMessages answers a Messages request from the Codex backend.
func (r *Router) serveCodexMessages(w http.ResponseWriter, req *http.Request, body []byte, resolution codex.Resolution) {
	if r.backend == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "the Codex backend is not configured")
		return
	}

	translated, err := translate.ClaudeToCodex(translate.Options{
		Model:         resolution.Model.ID,
		Effort:        resolution.Effort,
		DefaultEffort: resolution.Model.DefaultEffort,
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

	resp, err := r.backend.Responses(req.Context(), translated.Body, sessionID)
	if err != nil {
		writeCodexError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	streaming := gjson.GetBytes(body, "stream").Bool()
	if streaming {
		r.streamCodex(w, req, resp.Body, resolution, translated.ToolNames)
		return
	}
	r.bufferCodex(w, resp.Body, resolution, translated.ToolNames)
}

// streamCodex relays the upstream event stream as Anthropic SSE.
func (r *Router) streamCodex(w http.ResponseWriter, req *http.Request, upstream io.Reader, resolution codex.Resolution, toolNames map[string]string) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// Buffering proxies would defeat the point of streaming.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	_ = controller.Flush()

	translator := translate.NewStreamTranslator(resolution.Requested, toolNames)
	write := func(frames []byte) bool {
		if len(frames) == 0 {
			return true
		}
		if _, err := w.Write(frames); err != nil {
			logx.Debugf("codex stream: client write failed: %v", err)
			return false
		}
		_ = controller.Flush()
		return true
	}

	err := scanSSE(upstream, func(payload []byte) bool {
		return write(translator.Event(payload))
	})
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, req.Context().Err()) {
		logx.Warnf("codex stream: upstream read failed: %v", err)
		write(translator.Error("api_error", "upstream stream ended: "+err.Error()))
	}
	write(translator.Close())
}

// bufferCodex answers a client that did not ask for streaming. The Codex
// backend only serves streaming responses, so the events go through the same
// translator as the streaming path and are then folded into one message.
func (r *Router) bufferCodex(w http.ResponseWriter, upstream io.Reader, resolution codex.Resolution, toolNames map[string]string) {
	translator := translate.NewStreamTranslator(resolution.Requested, toolNames)
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

// writeCodexTokenCount answers count_tokens for a Codex model with a local
// estimate, since the backend exposes no counting endpoint.
func (r *Router) writeCodexTokenCount(w http.ResponseWriter, body []byte) {
	count := tokenize.EstimateClaudeRequest(body)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"input_tokens": count})
}

// writeCodexError maps a backend failure onto an Anthropic error response.
func writeCodexError(w http.ResponseWriter, err error) {
	if errors.Is(err, codex.ErrNotAuthenticated) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}

	var apiErr *codex.APIError
	if errors.As(err, &apiErr) {
		kind := "api_error"
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = "authentication_error"
		case http.StatusTooManyRequests:
			kind = "rate_limit_error"
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			kind = "invalid_request_error"
		}
		message := string(bytes.TrimSpace(apiErr.Body))
		// Surface the upstream message rather than its JSON envelope when
		// one is available.
		if text := gjson.Get(message, "error.message").String(); text != "" {
			message = text
		} else if text := gjson.Get(message, "detail").String(); text != "" {
			message = text
		}
		if message == "" {
			message = http.StatusText(apiErr.StatusCode)
		}
		logx.Warnf("codex backend error %d: %s", apiErr.StatusCode, message)
		writeAnthropicError(w, apiErr.StatusCode, kind, message)
		return
	}

	logx.Errorf("codex request failed: %v", err)
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
