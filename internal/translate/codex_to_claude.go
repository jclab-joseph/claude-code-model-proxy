package translate

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// summarySeparator joins the reasoning summary parts that make up a single
// Codex reasoning item into one Claude thinking block.
const summarySeparator = "\n\n"

// callState tracks one streamed function call.
type callState struct {
	callID     string
	name       string
	blockIndex int
	arguments  string
	emitted    int
	sawDelta   bool
	done       bool
	closed     bool
	started    bool
}

// StreamTranslator turns the Codex Responses event stream into the Anthropic
// Messages SSE stream Claude Code consumes. One instance handles one response
// and is not safe for concurrent use.
type StreamTranslator struct {
	// dialect decides how the reasoning of this backend is signed.
	dialect Dialect
	// clientModel is echoed back so the client sees the name it requested
	// rather than the upstream slug.
	clientModel string
	// toolNames maps shortened function names back to the client's names.
	toolNames map[string]string

	blockIndex   int
	textOpen     bool
	thinkingOpen bool
	// thinkingSignature holds the reasoning item's encrypted state until the
	// block closes, since only output_item.done carries the final value.
	thinkingSignature string
	thinkingSeen      bool

	messageStarted  bool
	messageStopped  bool
	hasTextDelta    bool
	emittedThinking bool
	emittedToolUse  bool

	calls  map[string]*callState
	queue  []*callState
	active *callState
}

// NewStreamTranslator returns a translator for one Codex response. clientModel
// is the model name the client asked for; toolNames maps upstream (shortened)
// tool names to the client's original names.
func NewStreamTranslator(clientModel string, toolNames map[string]string) *StreamTranslator {
	return Codex.NewStreamTranslator(clientModel, toolNames)
}

// NewStreamTranslator returns a translator for one response from this
// dialect's backend, as the package-level NewStreamTranslator does for Codex.
func (d Dialect) NewStreamTranslator(clientModel string, toolNames map[string]string) *StreamTranslator {
	return &StreamTranslator{
		dialect:     d.orDefault(),
		clientModel: clientModel,
		toolNames:   toolNames,
		calls:       map[string]*callState{},
	}
}

// Started reports whether a message_start has been emitted, which tells the
// caller whether an error can still be reported as an HTTP status.
func (t *StreamTranslator) Started() bool { return t.messageStarted }

// Event translates a single Codex SSE payload (the JSON after "data:") into
// zero or more Anthropic SSE frames.
func (t *StreamTranslator) Event(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return nil
	}
	root := gjson.ParseBytes(payload)
	out := make([]byte, 0, 256)

	switch root.Get("type").String() {
	case "response.created":
		out = append(out, t.start(root.Get("response"))...)

	case "response.reasoning_summary_part.added":
		out = append(out, t.ensureStarted()...)
		out = append(out, t.closeText()...)
		// Codex splits one reasoning item into several summary parts but only
		// reports the item's encrypted state at the end, so a single thinking
		// block spans the whole item and the parts are separated by a blank line.
		if t.thinkingOpen {
			out = append(out, t.thinkingDelta(summarySeparator)...)
		} else {
			out = append(out, t.openThinking()...)
		}
		t.thinkingSeen = true

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// Codex streams a summary of its reasoning; OpenRouter streams the
		// reasoning text itself. Both become the thinking block.
		out = append(out, t.ensureStarted()...)
		out = append(out, t.closeText()...)
		out = append(out, t.openThinking()...)
		out = append(out, t.thinkingDelta(root.Get("delta").String())...)
		t.thinkingSeen = true

	case "response.content_part.added":
		if root.Get("part.type").String() == "output_text" {
			out = append(out, t.ensureStarted()...)
			out = append(out, t.closeThinking()...)
			out = append(out, t.openText()...)
		}

	case "response.output_text.delta":
		out = append(out, t.ensureStarted()...)
		out = append(out, t.closeThinking()...)
		out = append(out, t.openText()...)
		out = append(out, t.textDelta(root.Get("delta").String())...)
		t.hasTextDelta = true

	case "response.content_part.done":
		if root.Get("part.type").String() == "output_text" {
			out = append(out, t.closeText()...)
		}

	case "response.output_item.added":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "reasoning":
			out = append(out, t.ensureStarted()...)
			out = append(out, t.closeText()...)
			// A reasoning item that never reported done must not leak its
			// open block into the next one.
			out = append(out, t.closeThinking()...)
			t.thinkingSeen = false
			// Kept only as a fallback: this is a pre-content snapshot, the
			// authoritative value arrives with output_item.done.
			t.thinkingSignature = item.Get("encrypted_content").String()
		case "function_call":
			out = append(out, t.ensureStarted()...)
			out = append(out, t.closeThinking()...)
			out = append(out, t.closeText()...)
			call := t.recordCall(root, item)
			t.updateCallIdentity(root, item, call)
			out = append(out, t.pumpCalls()...)
		}

	case "response.output_item.done":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "reasoning":
			out = append(out, t.closeText()...)
			if signature := item.Get("encrypted_content").String(); signature != "" {
				t.thinkingSignature = signature
			}
			if !t.thinkingSeen && t.thinkingSignature != "" {
				// No summary was streamed, but the encrypted state still has
				// to reach the client so the next turn can replay it.
				out = append(out, t.openThinking()...)
			}
			out = append(out, t.closeThinking()...)
			t.thinkingSignature = ""
			t.thinkingSeen = false
		case "function_call":
			out = append(out, t.closeThinking()...)
			out = append(out, t.closeText()...)
			call := t.lookupCall(root, item)
			if call == nil {
				call = t.recordCall(root, item)
			}
			t.updateCallIdentity(root, item, call)
			t.appendArguments(call, item.Get("arguments").String(), false)
			call.done = true
			out = append(out, t.pumpCalls()...)
		case "message":
			// Some responses deliver the whole message at once with no
			// output_text deltas.
			if t.hasTextDelta {
				break
			}
			var text strings.Builder
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					text.WriteString(part.Get("text").String())
				}
				return true
			})
			if text.Len() == 0 {
				break
			}
			out = append(out, t.ensureStarted()...)
			out = append(out, t.closeThinking()...)
			out = append(out, t.openText()...)
			out = append(out, t.textDelta(text.String())...)
			out = append(out, t.closeText()...)
			t.hasTextDelta = true
		}

	case "response.function_call_arguments.delta":
		call := t.lookupCall(root, gjson.Result{})
		if call == nil {
			call = t.recordCall(root, gjson.Result{})
		}
		t.appendArguments(call, root.Get("delta").String(), true)
		out = append(out, t.flushArguments(call)...)

	case "response.function_call_arguments.done":
		call := t.lookupCall(root, gjson.Result{})
		if call == nil {
			call = t.recordCall(root, gjson.Result{})
		}
		t.appendArguments(call, root.Get("arguments").String(), false)
		out = append(out, t.flushArguments(call)...)

	case "response.completed", "response.incomplete":
		out = append(out, t.finish(root.Get("response"))...)

	case "response.failed":
		message := root.Get("response.error.message").String()
		if message == "" {
			message = "the Codex backend reported a failed response"
		}
		out = append(out, t.Error(root.Get("response.error.type").String(), message)...)

	case "error":
		errorResult := root.Get("error")
		kind := errorResult.Get("type").String()
		message := errorResult.Get("message").String()
		if message == "" {
			message = root.Get("message").String()
		}
		out = append(out, t.Error(kind, message)...)
	}

	return out
}

// Error renders an Anthropic error frame.
func (t *StreamTranslator) Error(kind, message string) []byte {
	if kind == "" {
		kind = "api_error"
	}
	if message == "" {
		message = "upstream error"
	}
	frame := []byte(`{"type":"error","error":{"type":"","message":""}}`)
	frame, _ = sjson.SetBytes(frame, "error.type", kind)
	frame, _ = sjson.SetBytes(frame, "error.message", message)
	return sseFrame("error", frame)
}

// Close emits whatever is needed to leave the client with a well-formed
// stream when upstream ends without a terminal event.
func (t *StreamTranslator) Close() []byte {
	if t.messageStopped {
		return nil
	}
	out := t.closeThinking()
	out = append(out, t.closeText()...)
	out = append(out, t.closeActiveCalls()...)
	if !t.messageStarted {
		return out
	}
	out = append(out, t.messageDelta("end_turn", 0, 0, 0, 0)...)
	out = append(out, sseFrame("message_stop", []byte(`{"type":"message_stop"}`))...)
	t.messageStopped = true
	return out
}

func (t *StreamTranslator) start(response gjson.Result) []byte {
	if t.messageStarted {
		return nil
	}
	t.messageStarted = true

	frame := []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
	id := response.Get("id").String()
	if id == "" {
		id = "msg_codex"
	}
	frame, _ = sjson.SetBytes(frame, "message.id", id)
	frame, _ = sjson.SetBytes(frame, "message.model", t.clientModel)
	return sseFrame("message_start", frame)
}

// ensureStarted covers streams that begin emitting content without a
// response.created event.
func (t *StreamTranslator) ensureStarted() []byte {
	if t.messageStarted {
		return nil
	}
	return t.start(gjson.Result{})
}

func (t *StreamTranslator) finish(response gjson.Result) []byte {
	out := t.ensureStarted()
	out = append(out, t.closeThinking()...)
	out = append(out, t.closeText()...)
	// A terminal event may repeat items that were never streamed.
	out = append(out, t.adoptTerminalContent(response)...)
	out = append(out, t.adoptTerminalCalls(response)...)
	out = append(out, t.closeThinking()...)
	out = append(out, t.closeText()...)

	input, output, cacheRead, cacheWrite := usage(response.Get("usage"))
	stopReason := mapStopReason(stopReasonOf(response), t.emittedToolUse)
	out = append(out, t.messageDelta(stopReason, input, output, cacheRead, cacheWrite)...)
	out = append(out, sseFrame("message_stop", []byte(`{"type":"message_stop"}`))...)
	t.messageStopped = true
	return out
}

func (t *StreamTranslator) messageDelta(stopReason string, input, output, cacheRead, cacheWrite int64) []byte {
	frame := []byte(`{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
	frame, _ = sjson.SetBytes(frame, "delta.stop_reason", stopReason)
	frame, _ = sjson.SetBytes(frame, "usage.input_tokens", input)
	frame, _ = sjson.SetBytes(frame, "usage.output_tokens", output)
	if cacheRead > 0 {
		frame, _ = sjson.SetBytes(frame, "usage.cache_read_input_tokens", cacheRead)
	}
	if cacheWrite > 0 {
		frame, _ = sjson.SetBytes(frame, "usage.cache_creation_input_tokens", cacheWrite)
	}
	return sseFrame("message_delta", frame)
}

func (t *StreamTranslator) openText() []byte {
	if t.textOpen {
		return nil
	}
	t.textOpen = true
	frame := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	frame, _ = sjson.SetBytes(frame, "index", t.blockIndex)
	return sseFrame("content_block_start", frame)
}

func (t *StreamTranslator) textDelta(text string) []byte {
	if text == "" {
		return nil
	}
	frame := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
	frame, _ = sjson.SetBytes(frame, "index", t.blockIndex)
	frame, _ = sjson.SetBytes(frame, "delta.text", text)
	return sseFrame("content_block_delta", frame)
}

func (t *StreamTranslator) closeText() []byte {
	if !t.textOpen {
		return nil
	}
	t.textOpen = false
	out := t.blockStop(t.blockIndex)
	t.blockIndex++
	return out
}

func (t *StreamTranslator) openThinking() []byte {
	if t.thinkingOpen {
		return nil
	}
	t.thinkingOpen = true
	t.emittedThinking = true
	frame := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	frame, _ = sjson.SetBytes(frame, "index", t.blockIndex)
	return sseFrame("content_block_start", frame)
}

func (t *StreamTranslator) thinkingDelta(text string) []byte {
	if text == "" {
		return nil
	}
	frame := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
	frame, _ = sjson.SetBytes(frame, "index", t.blockIndex)
	frame, _ = sjson.SetBytes(frame, "delta.thinking", text)
	return sseFrame("content_block_delta", frame)
}

func (t *StreamTranslator) closeThinking() []byte {
	if !t.thinkingOpen {
		return nil
	}
	out := make([]byte, 0, 256)
	if signature := t.dialect.sign(t.thinkingSignature); signature != "" {
		frame := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`)
		frame, _ = sjson.SetBytes(frame, "index", t.blockIndex)
		frame, _ = sjson.SetBytes(frame, "delta.signature", signature)
		out = append(out, sseFrame("content_block_delta", frame)...)
	}
	out = append(out, t.blockStop(t.blockIndex)...)
	t.thinkingOpen = false
	t.blockIndex++
	return out
}

func (t *StreamTranslator) blockStop(index int) []byte {
	frame := []byte(`{"type":"content_block_stop","index":0}`)
	frame, _ = sjson.SetBytes(frame, "index", index)
	return sseFrame("content_block_stop", frame)
}

// callKeys lists the identifiers an event can be correlated by. Codex labels
// function-call events inconsistently across event types, so every available
// identifier becomes an alias for the same call.
func callKeys(root, item gjson.Result) []string {
	keys := make([]string, 0, 5)
	add := func(key string) {
		if key == "" {
			return
		}
		for _, existing := range keys {
			if existing == key {
				return
			}
		}
		keys = append(keys, key)
	}
	if index := root.Get("output_index"); index.Exists() {
		add("output:" + index.Raw)
	}
	add("call:" + item.Get("call_id").String())
	add("call:" + root.Get("call_id").String())
	add("item:" + item.Get("id").String())
	add("item:" + root.Get("item_id").String())

	filtered := keys[:0]
	for _, key := range keys {
		if !strings.HasSuffix(key, ":") {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

func (t *StreamTranslator) lookupCall(root, item gjson.Result) *callState {
	for _, key := range callKeys(root, item) {
		if call, ok := t.calls[key]; ok {
			return call
		}
	}
	if t.active != nil {
		return t.active
	}
	return nil
}

func (t *StreamTranslator) recordCall(root, item gjson.Result) *callState {
	keys := callKeys(root, item)
	for _, key := range keys {
		if call, ok := t.calls[key]; ok {
			t.alias(call, keys)
			return call
		}
	}
	call := &callState{blockIndex: -1}
	t.queue = append(t.queue, call)
	t.alias(call, keys)
	return call
}

func (t *StreamTranslator) alias(call *callState, keys []string) {
	for _, key := range keys {
		t.calls[key] = call
	}
}

func (t *StreamTranslator) updateCallIdentity(root, item gjson.Result, call *callState) {
	if call == nil {
		return
	}
	if callID := item.Get("call_id").String(); callID != "" {
		call.callID = callID
	}
	if name := item.Get("name").String(); name != "" {
		call.name = name
	}
	t.alias(call, callKeys(root, item))
}

func (t *StreamTranslator) appendArguments(call *callState, arguments string, delta bool) {
	if call == nil || arguments == "" {
		return
	}
	if delta {
		call.arguments += arguments
		call.sawDelta = true
		return
	}
	if !call.sawDelta {
		call.arguments = arguments
		return
	}
	// A final value that extends what was streamed replaces it; a shorter or
	// divergent one is ignored so partial JSON is never truncated.
	if strings.HasPrefix(arguments, call.arguments) {
		call.arguments = arguments
	}
}

// flushArguments emits the not-yet-sent tail of the active call's arguments.
func (t *StreamTranslator) flushArguments(call *callState) []byte {
	if call == nil || t.active != call || !call.started || call.closed {
		return nil
	}
	if call.emitted >= len(call.arguments) {
		return nil
	}
	frame := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	frame, _ = sjson.SetBytes(frame, "index", call.blockIndex)
	frame, _ = sjson.SetBytes(frame, "delta.partial_json", call.arguments[call.emitted:])
	call.emitted = len(call.arguments)
	return sseFrame("content_block_delta", frame)
}

// pumpCalls opens, feeds and closes queued tool_use blocks one at a time,
// because Anthropic content blocks cannot interleave.
func (t *StreamTranslator) pumpCalls() []byte {
	out := make([]byte, 0, 256)
	for {
		if active := t.active; active != nil {
			out = append(out, t.flushArguments(active)...)
			if !active.done {
				return out
			}
			out = append(out, t.blockStop(active.blockIndex)...)
			if t.blockIndex <= active.blockIndex {
				t.blockIndex = active.blockIndex + 1
			}
			active.closed = true
			t.active = nil
			t.dequeue(active)
		}

		for len(t.queue) > 0 && t.queue[0].closed {
			t.queue = t.queue[1:]
		}
		if len(t.queue) == 0 {
			return out
		}

		call := t.queue[0]
		// Without a name there is nothing to open yet; wait for the event
		// that carries it.
		if call.name == "" {
			return out
		}

		call.blockIndex = t.blockIndex
		call.started = true
		t.active = call
		t.emittedToolUse = true

		frame := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
		frame, _ = sjson.SetBytes(frame, "index", call.blockIndex)
		frame, _ = sjson.SetBytes(frame, "content_block.id", sanitizeToolID(call.callID))
		frame, _ = sjson.SetBytes(frame, "content_block.name", t.originalToolName(call.name))
		out = append(out, sseFrame("content_block_start", frame)...)
		out = append(out, t.flushArguments(call)...)
	}
}

func (t *StreamTranslator) dequeue(call *callState) {
	for i, queued := range t.queue {
		if queued == call {
			t.queue = append(t.queue[:i], t.queue[i+1:]...)
			return
		}
	}
}

func (t *StreamTranslator) closeActiveCalls() []byte {
	out := make([]byte, 0, 64)
	if t.active != nil {
		t.active.done = true
		out = append(out, t.pumpCalls()...)
	}
	return out
}

// adoptTerminalContent emits reasoning and message items that appear only in
// the terminal response payload.
//
// Which items a terminal event repeats varies: the production backend streams
// everything and repeats nothing, while other deployments deliver a complete
// output array. Emitting only what was not already streamed covers both
// without duplicating content.
func (t *StreamTranslator) adoptTerminalContent(response gjson.Result) []byte {
	output := response.Get("output")
	if !output.IsArray() {
		return nil
	}

	out := make([]byte, 0, 128)
	output.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "reasoning":
			if t.emittedThinking {
				return true
			}
			var thinking strings.Builder
			appendParts := func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					if thinking.Len() > 0 {
						thinking.WriteString(summarySeparator)
					}
					thinking.WriteString(text)
				}
				return true
			}
			item.Get("summary").ForEach(appendParts)
			// OpenRouter reports the reasoning itself as content parts.
			item.Get("content").ForEach(appendParts)
			signature := item.Get("encrypted_content").String()
			if thinking.Len() == 0 && signature == "" {
				return true
			}
			t.thinkingSignature = signature
			out = append(out, t.openThinking()...)
			out = append(out, t.thinkingDelta(thinking.String())...)
			out = append(out, t.closeThinking()...)
			t.thinkingSignature = ""

		case "message":
			if t.hasTextDelta {
				return true
			}
			var text strings.Builder
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					text.WriteString(part.Get("text").String())
				}
				return true
			})
			if text.Len() == 0 {
				return true
			}
			out = append(out, t.openText()...)
			out = append(out, t.textDelta(text.String())...)
			out = append(out, t.closeText()...)
			t.hasTextDelta = true
		}
		return true
	})
	return out
}

// adoptTerminalCalls picks up function calls that only appear in the terminal
// response payload and drains everything still queued.
func (t *StreamTranslator) adoptTerminalCalls(response gjson.Result) []byte {
	response.Get("output").ForEach(func(index, item gjson.Result) bool {
		if item.Get("type").String() != "function_call" {
			return true
		}
		root := gjson.Parse(`{"output_index":` + index.String() + `}`)
		call := t.lookupCallStrict(root, item)
		if call == nil {
			call = t.recordCall(root, item)
		}
		t.updateCallIdentity(root, item, call)
		t.appendArguments(call, item.Get("arguments").String(), false)
		call.done = true
		return true
	})

	remaining := t.queue[:0]
	for _, call := range t.queue {
		if call.closed {
			continue
		}
		if call.name == "" {
			call.closed = true
			continue
		}
		call.done = true
		remaining = append(remaining, call)
	}
	t.queue = remaining
	return t.pumpCalls()
}

// lookupCallStrict resolves by alias only, without falling back to the active
// call, so a terminal item is never merged into an unrelated one.
func (t *StreamTranslator) lookupCallStrict(root, item gjson.Result) *callState {
	for _, key := range callKeys(root, item) {
		if call, ok := t.calls[key]; ok {
			return call
		}
	}
	return nil
}

func (t *StreamTranslator) originalToolName(name string) string {
	if original, ok := t.toolNames[name]; ok {
		return original
	}
	return name
}

// sanitizeToolID keeps tool ids within the character set Anthropic accepts.
func sanitizeToolID(id string) string {
	if id == "" {
		return "toolu_codex"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func stopReasonOf(response gjson.Result) string {
	if reason := response.Get("stop_reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if reason := response.Get("incomplete_details.reason"); reason.Exists() {
		return reason.String()
	}
	return ""
}

func mapStopReason(reason string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	switch reason {
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "refusal", "content_filter":
		return "refusal"
	case "pause_turn":
		return "pause_turn"
	default:
		return "end_turn"
	}
}

// usage extracts token counts, converting the Responses cached-token
// accounting into Anthropic's split between fresh and cached input.
func usage(u gjson.Result) (input, output, cacheRead, cacheWrite int64) {
	if !u.Exists() {
		return 0, 0, 0, 0
	}
	input = u.Get("input_tokens").Int()
	output = u.Get("output_tokens").Int()
	cacheRead = u.Get("input_tokens_details.cached_tokens").Int()
	cacheWrite = u.Get("input_tokens_details.cache_write_tokens").Int()
	if cacheRead > 0 {
		if input >= cacheRead {
			input -= cacheRead
		} else {
			input = 0
		}
	}
	return input, output, cacheRead, cacheWrite
}

// sseFrame renders one Server-Sent Event.
func sseFrame(event string, data []byte) []byte {
	out := make([]byte, 0, len(data)+len(event)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, '\n', '\n')
	return out
}
