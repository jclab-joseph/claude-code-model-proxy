package translate

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MessageAssembler folds the Anthropic SSE frames a StreamTranslator produces
// back into a single non-streaming message.
//
// A client that did not ask for streaming still has to be answered from a
// stream, because that is all the Codex backend serves. Rebuilding the message
// from the translated frames rather than from the terminal upstream payload
// keeps one translation path for both cases: the real backend does not repeat
// the output items in response.completed, so anything reading only that event
// would return an empty message.
type MessageAssembler struct {
	id     string
	model  string
	blocks []*assembledBlock
	open   *assembledBlock

	stopReason   string
	stopSequence string
	usage        []byte

	errorType    string
	errorMessage string
}

type assembledBlock struct {
	kind      string
	text      strings.Builder
	thinking  strings.Builder
	signature string
	toolID    string
	toolName  string
	arguments strings.Builder
}

// NewMessageAssembler returns an assembler for one response.
func NewMessageAssembler() *MessageAssembler {
	return &MessageAssembler{}
}

// Write consumes SSE frames emitted by a StreamTranslator.
func (a *MessageAssembler) Write(frames []byte) {
	for _, payload := range ssePayloads(frames) {
		a.event(payload)
	}
}

// Err reports the error type and message if the stream carried one.
func (a *MessageAssembler) Err() (kind string, message string, ok bool) {
	if a.errorMessage == "" {
		return "", "", false
	}
	return a.errorType, a.errorMessage, true
}

func (a *MessageAssembler) event(payload []byte) {
	event := gjson.ParseBytes(payload)
	switch event.Get("type").String() {
	case "message_start":
		a.id = event.Get("message.id").String()
		a.model = event.Get("message.model").String()

	case "content_block_start":
		block := &assembledBlock{
			kind:     event.Get("content_block.type").String(),
			toolID:   event.Get("content_block.id").String(),
			toolName: event.Get("content_block.name").String(),
		}
		if text := event.Get("content_block.text").String(); text != "" {
			block.text.WriteString(text)
		}
		a.blocks = append(a.blocks, block)
		a.open = block

	case "content_block_delta":
		if a.open == nil {
			return
		}
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			a.open.text.WriteString(delta.Get("text").String())
		case "thinking_delta":
			a.open.thinking.WriteString(delta.Get("thinking").String())
		case "signature_delta":
			a.open.signature = delta.Get("signature").String()
		case "input_json_delta":
			a.open.arguments.WriteString(delta.Get("partial_json").String())
		}

	case "content_block_stop":
		a.open = nil

	case "message_delta":
		if reason := event.Get("delta.stop_reason"); reason.Exists() {
			a.stopReason = reason.String()
		}
		if sequence := event.Get("delta.stop_sequence"); sequence.Type == gjson.String {
			a.stopSequence = sequence.String()
		}
		if usage := event.Get("usage"); usage.IsObject() {
			a.usage = []byte(usage.Raw)
		}

	case "error":
		a.errorType = event.Get("error.type").String()
		a.errorMessage = event.Get("error.message").String()
	}
}

// Message renders the assembled Anthropic message.
func (a *MessageAssembler) Message() []byte {
	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)

	id := a.id
	if id == "" {
		id = "msg_codex"
	}
	out, _ = sjson.SetBytes(out, "id", id)
	out, _ = sjson.SetBytes(out, "model", a.model)

	rendered := make([][]byte, 0, len(a.blocks))
	for _, block := range a.blocks {
		if encoded, ok := block.render(); ok {
			rendered = append(rendered, encoded)
		}
	}
	if len(rendered) > 0 {
		out, _ = sjson.SetRawBytes(out, "content", joinRawArray(rendered))
	}

	if a.stopReason != "" {
		out, _ = sjson.SetBytes(out, "stop_reason", a.stopReason)
	}
	if a.stopSequence != "" {
		out, _ = sjson.SetBytes(out, "stop_sequence", a.stopSequence)
	}
	if len(a.usage) > 0 {
		out, _ = sjson.SetRawBytes(out, "usage", a.usage)
	}
	return out
}

func (block *assembledBlock) render() ([]byte, bool) {
	switch block.kind {
	case "text":
		if block.text.Len() == 0 {
			return nil, false
		}
		out := []byte(`{"type":"text","text":""}`)
		out, _ = sjson.SetBytes(out, "text", block.text.String())
		return out, true

	case "thinking":
		if block.thinking.Len() == 0 && block.signature == "" {
			return nil, false
		}
		out := []byte(`{"type":"thinking","thinking":"","signature":""}`)
		out, _ = sjson.SetBytes(out, "thinking", block.thinking.String())
		out, _ = sjson.SetBytes(out, "signature", block.signature)
		return out, true

	case "tool_use":
		if block.toolName == "" {
			return nil, false
		}
		out := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
		out, _ = sjson.SetBytes(out, "id", block.toolID)
		out, _ = sjson.SetBytes(out, "name", block.toolName)
		// Partial arguments would make the block unusable; an empty object
		// is at least well formed.
		if arguments := block.arguments.String(); arguments != "" && gjson.Valid(arguments) {
			if parsed := gjson.Parse(arguments); parsed.IsObject() {
				out, _ = sjson.SetRawBytes(out, "input", []byte(parsed.Raw))
			}
		}
		return out, true

	default:
		return nil, false
	}
}

// ssePayloads extracts the data payload of each event in an SSE byte stream.
func ssePayloads(frames []byte) [][]byte {
	var payloads [][]byte
	for _, block := range bytes.Split(frames, []byte("\n\n")) {
		var payload []byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			value := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if len(payload) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, value...)
		}
		if len(payload) > 0 {
			payloads = append(payloads, payload)
		}
	}
	return payloads
}
