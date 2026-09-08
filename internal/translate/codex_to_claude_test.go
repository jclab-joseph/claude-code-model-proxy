package translate

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// frame is one decoded Anthropic SSE event.
type frame struct {
	event string
	data  string
}

// runStream feeds Codex event payloads through the translator and decodes the
// Anthropic frames it produces.
func runStream(t *testing.T, translator *StreamTranslator, payloads ...string) []frame {
	t.Helper()
	var raw strings.Builder
	for _, payload := range payloads {
		raw.Write(translator.Event([]byte(payload)))
	}
	raw.Write(translator.Close())
	return parseFrames(t, raw.String())
}

func parseFrames(t *testing.T, stream string) []frame {
	t.Helper()
	var frames []frame
	for _, block := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var current frame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				current.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if current.event == "" {
			t.Fatalf("frame without an event line: %q", block)
		}
		if !gjson.Valid(current.data) {
			t.Fatalf("frame data is not valid JSON: %q", current.data)
		}
		frames = append(frames, current)
	}
	return frames
}

func eventNames(frames []frame) []string {
	names := make([]string, 0, len(frames))
	for _, f := range frames {
		names = append(names, f.event)
	}
	return names
}

func assertEvents(t *testing.T, frames []frame, want ...string) {
	t.Helper()
	got := eventNames(frames)
	if len(got) != len(want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", got, want)
		}
	}
}

func TestStreamTextResponse(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5-high", nil),
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.content_part.added","part":{"type":"output_text"}}`,
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.output_text.delta","delta":" world"}`,
		`{"type":"response.content_part.done","part":{"type":"output_text"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":120,"output_tokens":8}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)

	start := gjson.Parse(frames[0].data)
	// The client must see the name it asked for, not the upstream slug.
	if got := start.Get("message.model").String(); got != "gpt-5.5-high" {
		t.Fatalf("message.model = %q, want the requested name", got)
	}
	if got := start.Get("message.id").String(); got != "resp_1" {
		t.Fatalf("message.id = %q", got)
	}

	if got := gjson.Parse(frames[2].data).Get("delta.text").String(); got != "Hello" {
		t.Fatalf("first text delta = %q", got)
	}

	delta := gjson.Parse(frames[5].data)
	if got := delta.Get("delta.stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
	if got := delta.Get("usage.input_tokens").Int(); got != 120 {
		t.Fatalf("input_tokens = %d, want 120", got)
	}
	if got := delta.Get("usage.output_tokens").Int(); got != 8 {
		t.Fatalf("output_tokens = %d, want 8", got)
	}
}

func TestStreamThinkingCarriesMarkedSignature(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_2"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"PARTIAL"}}`,
		`{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Thinking about it"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"FINAL"}}`,
		`{"type":"response.content_part.added","part":{"type":"output_text"}}`,
		`{"type":"response.output_text.delta","delta":"Answer"}`,
		`{"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta
		"content_block_delta", // signature_delta
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)

	if got := gjson.Parse(frames[1].data).Get("content_block.type").String(); got != "thinking" {
		t.Fatalf("first block type = %q, want thinking", got)
	}
	if got := gjson.Parse(frames[2].data).Get("delta.thinking").String(); got != "Thinking about it" {
		t.Fatalf("thinking delta = %q", got)
	}

	signature := gjson.Parse(frames[3].data).Get("delta.signature").String()
	// The final value from output_item.done wins over the earlier snapshot,
	// and it is marked as this proxy's own.
	if signature != SignatureMarker+"FINAL" {
		t.Fatalf("signature = %q, want %q", signature, SignatureMarker+"FINAL")
	}

	// The thinking block must occupy index 0 and the text block index 1.
	if got := gjson.Parse(frames[1].data).Get("index").Int(); got != 0 {
		t.Fatalf("thinking block index = %d, want 0", got)
	}
	if got := gjson.Parse(frames[5].data).Get("index").Int(); got != 1 {
		t.Fatalf("text block index = %d, want 1", got)
	}
}

func TestStreamReasoningWithoutSummaryStillDeliversSignature(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_3"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"ONLYSIG"}}`,
		`{"type":"response.completed","response":{"id":"resp_3","usage":{}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
	if got := gjson.Parse(frames[2].data).Get("delta.signature").String(); got != SignatureMarker+"ONLYSIG" {
		t.Fatalf("signature = %q", got)
	}
}

func TestStreamToolCall(t *testing.T) {
	toolNames := map[string]string{"mcp__srv__do_thing": "mcp__a-very-long-server__do_thing"}
	frames := runStream(t, NewStreamTranslator("gpt-5.5", toolNames),
		`{"type":"response.created","response":{"id":"resp_4"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"mcp__srv__do_thing"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"a\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"mcp__srv__do_thing","arguments":"{\"a\":1}"}}`,
		`{"type":"response.completed","response":{"id":"resp_4","output":[{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"mcp__srv__do_thing","arguments":"{\"a\":1}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)

	start := gjson.Parse(frames[1].data)
	if got := start.Get("content_block.type").String(); got != "tool_use" {
		t.Fatalf("block type = %q, want tool_use", got)
	}
	if got := start.Get("content_block.id").String(); got != "call_abc" {
		t.Fatalf("tool_use id = %q", got)
	}
	// The client sees its own tool name, not the shortened upstream one.
	if got := start.Get("content_block.name").String(); got != "mcp__a-very-long-server__do_thing" {
		t.Fatalf("tool name = %q, want the original client name", got)
	}

	var arguments strings.Builder
	for _, f := range frames[2:4] {
		arguments.WriteString(gjson.Parse(f.data).Get("delta.partial_json").String())
	}
	if got := arguments.String(); got != `{"a":1}` {
		t.Fatalf("streamed arguments = %q", got)
	}

	if got := gjson.Parse(frames[5].data).Get("delta.stop_reason").String(); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

func TestStreamParallelToolCallsDoNotInterleave(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_5"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"Write"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_2","delta":"{\"b\":2}"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"a\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{\"a\":1}"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"Write","arguments":"{\"b\":2}"}}`,
		`{"type":"response.completed","response":{"id":"resp_5","output":[],"usage":{}}}`,
	)

	// Every block must open, receive its own arguments and close before the
	// next one opens.
	type block struct {
		index int
		id    string
		args  string
		open  bool
	}
	var blocks []*block
	var current *block
	for _, f := range frames {
		data := gjson.Parse(f.data)
		switch f.event {
		case "content_block_start":
			if current != nil {
				t.Fatalf("block %d opened while block %d was still open", data.Get("index").Int(), current.index)
			}
			current = &block{index: int(data.Get("index").Int()), id: data.Get("content_block.id").String(), open: true}
			blocks = append(blocks, current)
		case "content_block_delta":
			if current == nil {
				t.Fatal("delta arrived with no open block")
			}
			current.args += data.Get("delta.partial_json").String()
		case "content_block_stop":
			if current == nil || int(data.Get("index").Int()) != current.index {
				t.Fatalf("stop for index %d did not match the open block", data.Get("index").Int())
			}
			current.open = false
			current = nil
		}
	}

	if len(blocks) != 2 {
		t.Fatalf("emitted %d tool_use blocks, want 2", len(blocks))
	}
	if blocks[0].index != 0 || blocks[1].index != 1 {
		t.Fatalf("block indices = %d, %d, want 0, 1", blocks[0].index, blocks[1].index)
	}
	if blocks[0].id != "call_1" || blocks[0].args != `{"a":1}` {
		t.Fatalf("first block = %+v", *blocks[0])
	}
	if blocks[1].id != "call_2" || blocks[1].args != `{"b":2}` {
		t.Fatalf("second block = %+v", *blocks[1])
	}
}

func TestStreamAdoptsToolCallsOnlyPresentInTerminalEvent(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_6"}}`,
		`{"type":"response.completed","response":{"id":"resp_6","output":[{"type":"function_call","call_id":"call_9","name":"Bash","arguments":"{\"command\":\"ls\"}"}],"usage":{}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
	if got := gjson.Parse(frames[1].data).Get("content_block.name").String(); got != "Bash" {
		t.Fatalf("tool name = %q", got)
	}
	if got := gjson.Parse(frames[2].data).Get("delta.partial_json").String(); got != `{"command":"ls"}` {
		t.Fatalf("arguments = %q", got)
	}
}

func TestStreamMessageItemWithoutDeltas(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_7"}}`,
		`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"whole answer"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_7","usage":{}}}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
	if got := gjson.Parse(frames[2].data).Get("delta.text").String(); got != "whole answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamTruncatedUpstreamIsClosedCleanly(t *testing.T) {
	translator := NewStreamTranslator("gpt-5.5", nil)
	// No terminal event: Close must still finish every open block.
	frames := runStream(t, translator,
		`{"type":"response.created","response":{"id":"resp_8"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
	)

	assertEvents(t, frames,
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
}

func TestStreamCachedTokensSplitInput(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_9"}}`,
		`{"type":"response.completed","response":{"id":"resp_9","usage":{"input_tokens":1000,"output_tokens":10,"input_tokens_details":{"cached_tokens":900}}}}`,
	)

	usage := gjson.Parse(frames[len(frames)-2].data).Get("usage")
	if got := usage.Get("input_tokens").Int(); got != 100 {
		t.Fatalf("input_tokens = %d, want 100 (cached tokens reported separately)", got)
	}
	if got := usage.Get("cache_read_input_tokens").Int(); got != 900 {
		t.Fatalf("cache_read_input_tokens = %d, want 900", got)
	}
}

func TestStreamMaxTokensStopReason(t *testing.T) {
	frames := runStream(t, NewStreamTranslator("gpt-5.5", nil),
		`{"type":"response.created","response":{"id":"resp_10"}}`,
		`{"type":"response.incomplete","response":{"id":"resp_10","incomplete_details":{"reason":"max_output_tokens"},"usage":{}}}`,
	)
	if got := gjson.Parse(frames[len(frames)-2].data).Get("delta.stop_reason").String(); got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens", got)
	}
}

func TestStreamErrorEvent(t *testing.T) {
	translator := NewStreamTranslator("gpt-5.5", nil)
	frames := parseFrames(t, string(translator.Event([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))))

	if len(frames) != 1 || frames[0].event != "error" {
		t.Fatalf("frames = %v, want a single error frame", eventNames(frames))
	}
	data := gjson.Parse(frames[0].data)
	if data.Get("error.type").String() != "rate_limit_error" || data.Get("error.message").String() != "slow down" {
		t.Fatalf("error frame = %s", frames[0].data)
	}
}

func TestSanitizeToolIDKeepsAcceptedCharacters(t *testing.T) {
	if got := sanitizeToolID("call_abc-123"); got != "call_abc-123" {
		t.Fatalf("sanitizeToolID mangled a valid id: %q", got)
	}
	if got := sanitizeToolID("call/abc.123"); got != "call_abc_123" {
		t.Fatalf("sanitizeToolID = %q", got)
	}
	if got := sanitizeToolID(""); got == "" {
		t.Fatal("an empty id must be replaced with a placeholder")
	}
}

// assemble runs Codex events through the translator and folds the frames into
// a single message, which is exactly what the non-streaming path does.
func assemble(t *testing.T, clientModel string, toolNames map[string]string, payloads ...string) gjson.Result {
	t.Helper()
	translator := NewStreamTranslator(clientModel, toolNames)
	assembler := NewMessageAssembler()
	for _, payload := range payloads {
		assembler.Write(translator.Event([]byte(payload)))
	}
	assembler.Write(translator.Close())
	out := assembler.Message()
	if !gjson.ValidBytes(out) {
		t.Fatalf("assembled message is not valid JSON: %s", out)
	}
	return gjson.ParseBytes(out)
}

func TestAssembleTextThinkingAndToolUse(t *testing.T) {
	message := assemble(t, "gpt-5.5-high", map[string]string{"short_name": "original_name"},
		`{"type":"response.created","response":{"id":"resp_a"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"pondering"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"SIG"}}`,
		`{"type":"response.content_part.added","part":{"type":"output_text"}}`,
		`{"type":"response.output_text.delta","delta":"the "}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.content_part.done","part":{"type":"output_text"}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"short_name"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"q\":1}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"short_name","arguments":"{\"q\":1}"}}`,
		`{"type":"response.completed","response":{"id":"resp_a","usage":{"input_tokens":42,"output_tokens":7}}}`,
	)

	if got := message.Get("model").String(); got != "gpt-5.5-high" {
		t.Fatalf("model = %q", got)
	}
	if got := message.Get("id").String(); got != "resp_a" {
		t.Fatalf("id = %q", got)
	}
	if got := message.Get("stop_reason").String(); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}

	blocks := message.Get("content").Array()
	if len(blocks) != 3 {
		t.Fatalf("content has %d blocks, want 3: %s", len(blocks), message.Get("content").Raw)
	}
	if blocks[0].Get("type").String() != "thinking" || blocks[0].Get("signature").String() != SignatureMarker+"SIG" {
		t.Fatalf("thinking block = %s", blocks[0].Raw)
	}
	if blocks[0].Get("thinking").String() != "pondering" {
		t.Fatalf("thinking text = %q", blocks[0].Get("thinking").String())
	}
	// Text deltas must be concatenated, not reported one block each.
	if blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != "the answer" {
		t.Fatalf("text block = %s", blocks[1].Raw)
	}
	if blocks[2].Get("name").String() != "original_name" {
		t.Fatalf("tool name = %q, want the client's own name", blocks[2].Get("name").String())
	}
	if blocks[2].Get("id").String() != "call_x" || blocks[2].Get("input.q").Int() != 1 {
		t.Fatalf("tool_use block = %s", blocks[2].Raw)
	}
	if got := message.Get("usage.input_tokens").Int(); got != 42 {
		t.Fatalf("input_tokens = %d", got)
	}
}

func TestAssembleWhenTerminalEventOmitsOutput(t *testing.T) {
	// The real Codex backend does not repeat the output items in
	// response.completed. A non-streaming answer must still carry them.
	message := assemble(t, "gpt-5.5-low", nil,
		`{"type":"response.created","response":{"id":"resp_b"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_9","call_id":"call_9","name":"read_file"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_9","delta":"{\"path\":\"/etc/hostname\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_9","call_id":"call_9","name":"read_file","arguments":"{\"path\":\"/etc/hostname\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_b","usage":{"input_tokens":64,"output_tokens":32}}}`,
	)

	blocks := message.Get("content").Array()
	if len(blocks) != 1 {
		t.Fatalf("content = %s, want the streamed tool call", message.Get("content").Raw)
	}
	if blocks[0].Get("type").String() != "tool_use" || blocks[0].Get("name").String() != "read_file" {
		t.Fatalf("block = %s", blocks[0].Raw)
	}
	if got := blocks[0].Get("input.path").String(); got != "/etc/hostname" {
		t.Fatalf("input.path = %q", got)
	}
	if got := message.Get("stop_reason").String(); got != "tool_use" {
		t.Fatalf("stop_reason = %q", got)
	}
}

func TestAssembleReportsAStreamError(t *testing.T) {
	translator := NewStreamTranslator("gpt-5.5", nil)
	assembler := NewMessageAssembler()
	assembler.Write(translator.Event([]byte(`{"type":"response.created","response":{"id":"resp_c"}}`)))
	assembler.Write(translator.Event([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)))

	kind, message, failed := assembler.Err()
	if !failed {
		t.Fatal("an error frame must be reported")
	}
	if kind != "rate_limit_error" || message != "slow down" {
		t.Fatalf("error = %q / %q", kind, message)
	}
}

func TestAssembleEmptyResponse(t *testing.T) {
	message := assemble(t, "gpt-5.5", nil,
		`{"type":"response.created","response":{"id":"resp_d"}}`,
		`{"type":"response.completed","response":{"id":"resp_d","usage":{}}}`,
	)
	if !message.Get("content").IsArray() || len(message.Get("content").Array()) != 0 {
		t.Fatalf("content = %s, want an empty array", message.Get("content").Raw)
	}
	if got := message.Get("stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q", got)
	}
}

func TestAssembleDiscardsPartialToolArguments(t *testing.T) {
	// A stream cut off mid-arguments must not yield invalid tool input.
	message := assemble(t, "gpt-5.5", nil,
		`{"type":"response.created","response":{"id":"resp_e"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"Bash"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc","delta":"{\"command\":\"l"}`,
	)

	blocks := message.Get("content").Array()
	if len(blocks) != 1 {
		t.Fatalf("content = %s", message.Get("content").Raw)
	}
	if got := blocks[0].Get("input").Raw; got != "{}" {
		t.Fatalf("input = %s, want an empty object rather than broken JSON", got)
	}
}
