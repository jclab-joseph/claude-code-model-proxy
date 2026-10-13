package translate

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func mustConvert(t *testing.T, opts Options, raw string) gjson.Result {
	t.Helper()
	if opts.Model == "" {
		opts.Model = "gpt-5.5"
	}
	request, err := ClaudeToCodex(opts, []byte(raw))
	if err != nil {
		t.Fatalf("ClaudeToCodex: %v", err)
	}
	if !gjson.ValidBytes(request.Body) {
		t.Fatalf("translated body is not valid JSON: %s", request.Body)
	}
	return gjson.ParseBytes(request.Body)
}

func TestSystemPromptBecomesDeveloperMessage(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"system": [{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be brief."}],
		"messages": [{"role":"user","content":"hi"}]
	}`)

	first := body.Get("input.0")
	if first.Get("role").String() != "developer" {
		t.Fatalf("first input role = %q, want developer", first.Get("role").String())
	}
	if got := first.Get("content.0.text").String(); got != "You are Claude Code." {
		t.Fatalf("first system text = %q", got)
	}
	if got := first.Get("content.1.text").String(); got != "Be brief." {
		t.Fatalf("second system text = %q", got)
	}
	if got := body.Get("input.1.content.0.type").String(); got != "input_text" {
		t.Fatalf("user text part type = %q, want input_text", got)
	}
}

func TestStringSystemAndStringContent(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"system": "system prompt",
		"messages": [{"role":"user","content":"hello"},{"role":"assistant","content":"hi there"}]
	}`)

	if got := body.Get("input.0.content.0.text").String(); got != "system prompt" {
		t.Fatalf("system text = %q", got)
	}
	// Assistant text is output_text on the Responses API.
	if got := body.Get("input.2.content.0.type").String(); got != "output_text" {
		t.Fatalf("assistant part type = %q, want output_text", got)
	}
}

func TestToolUseAndResultBecomeFunctionCallItems(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"messages": [
			{"role":"assistant","content":[
				{"type":"text","text":"Let me look."},
				{"type":"tool_use","id":"toolu_01","name":"Read","input":{"file_path":"/tmp/x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"file body"}]}
			]}
		],
		"tools": [{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]
	}`)

	if got := body.Get("input.0.content.0.text").String(); got != "Let me look." {
		t.Fatalf("assistant text = %q", got)
	}
	call := body.Get("input.1")
	if call.Get("type").String() != "function_call" {
		t.Fatalf("input.1 type = %q, want function_call", call.Get("type").String())
	}
	if call.Get("call_id").String() != "toolu_01" || call.Get("name").String() != "Read" {
		t.Fatalf("function_call = %s", call.Raw)
	}
	// Arguments travel as a JSON string, not an object.
	if got := call.Get("arguments").String(); got != `{"file_path":"/tmp/x"}` {
		t.Fatalf("arguments = %q", got)
	}

	output := body.Get("input.2")
	if output.Get("type").String() != "function_call_output" {
		t.Fatalf("input.2 type = %q, want function_call_output", output.Get("type").String())
	}
	if got := output.Get("output.0.text").String(); got != "file body" {
		t.Fatalf("tool result text = %q", got)
	}
}

func TestParallelToolResultsFollowCallOrder(t *testing.T) {
	// Claude Code appends results as the tools finish: here b, then c, then a.
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"messages": [
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_a","name":"Read","input":{"file_path":"/a"}},
				{"type":"tool_use","id":"toolu_b","name":"Read","input":{"file_path":"/b"}},
				{"type":"tool_use","id":"toolu_c","name":"Read","input":{"file_path":"/c"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_b","content":"B"},
				{"type":"tool_result","tool_use_id":"toolu_unknown","content":"?"},
				{"type":"tool_result","tool_use_id":"toolu_c","content":"C"},
				{"type":"tool_result","tool_use_id":"toolu_a","content":"A"},
				{"type":"text","text":"next"}
			]}
		]
	}`)

	var got []string
	for _, item := range body.Get("input").Array() {
		if item.Get("type").String() == "function_call_output" {
			got = append(got, item.Get("call_id").String()+"="+item.Get("output").String())
		}
	}
	want := "toolu_a=A toolu_b=B toolu_c=C toolu_unknown=?"
	if strings.Join(got, " ") != want {
		t.Fatalf("outputs = %v, want %s", got, want)
	}
	// The text that followed the results stays after them.
	if last := body.Get("input.@reverse.0.content.0.text").String(); last != "next" {
		t.Fatalf("last input text = %q, want next", last)
	}
}

func TestOnlyProxyIssuedThinkingIsReplayed(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"messages": [{"role":"assistant","content":[
			{"type":"thinking","thinking":"ours","signature":"`+SignatureMarker+`ENCRYPTED"},
			{"type":"thinking","thinking":"anthropic's","signature":"ErUBCkYIBxgCKkA..."},
			{"type":"text","text":"done"}
		]}]
	}`)

	reasoningCount := 0
	body.Get("input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			reasoningCount++
			if got := item.Get("encrypted_content").String(); got != "ENCRYPTED" {
				t.Fatalf("encrypted_content = %q, want the marker stripped", got)
			}
		}
		return true
	})
	if reasoningCount != 1 {
		t.Fatalf("reasoning items = %d, want exactly the one this proxy issued", reasoningCount)
	}
}

func TestLongToolNamesAreShortenedAndMappedBack(t *testing.T) {
	longName := "mcp__some-very-long-server-name-here__do_the_thing_with_extras"
	longer := "mcp__another-extremely-long-server-name-goes-here__do_the_thing_with_extras"

	request, err := ClaudeToCodex(Options{Model: "gpt-5.5"}, []byte(`{
		"model": "gpt-5.5",
		"messages": [{"role":"user","content":"go"}],
		"tools": [
			{"name":"`+longName+`","input_schema":{"type":"object","properties":{}}},
			{"name":"`+longer+`","input_schema":{"type":"object","properties":{}}}
		]
	}`))
	if err != nil {
		t.Fatalf("ClaudeToCodex: %v", err)
	}

	body := gjson.ParseBytes(request.Body)
	seen := map[string]bool{}
	body.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		name := tool.Get("name").String()
		if len(name) > nameLimit {
			t.Fatalf("tool name %q is %d bytes, over the %d limit", name, len(name), nameLimit)
		}
		if seen[name] {
			t.Fatalf("tool name %q was emitted twice; shortened names must stay unique", name)
		}
		seen[name] = true
		if original, ok := request.ToolNames[name]; !ok {
			t.Fatalf("no reverse mapping for %q", name)
		} else if original != longName && original != longer {
			t.Fatalf("reverse mapping for %q = %q", name, original)
		}
		return true
	})
	if len(seen) != 2 {
		t.Fatalf("emitted %d tools, want 2", len(seen))
	}
}

func TestToolSchemaNormalization(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model": "gpt-5.5",
		"messages": [{"role":"user","content":"go"}],
		"tools": [
			{"name":"NoSchema","input_schema":null},
			{"name":"NoProperties","input_schema":{"type":"object"}},
			{"name":"HasDollarSchema","input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"a":{"type":"string"}}}}
		]
	}`)

	tools := body.Get("tools").Array()
	if got := tools[0].Get("parameters").Raw; got != `{"type":"object","properties":{}}` {
		t.Fatalf("null schema became %s", got)
	}
	if !tools[1].Get("parameters.properties").Exists() {
		t.Fatalf("object schema without properties was not filled in: %s", tools[1].Raw)
	}
	if tools[2].Get("parameters.$schema").Exists() {
		t.Fatalf("$schema must be removed: %s", tools[2].Raw)
	}
	for _, tool := range tools {
		if tool.Get("type").String() != "function" {
			t.Fatalf("tool type = %q, want function", tool.Get("type").String())
		}
		if tool.Get("input_schema").Exists() {
			t.Fatalf("input_schema must not be forwarded: %s", tool.Raw)
		}
	}
}

func TestToolChoiceMapping(t *testing.T) {
	cases := map[string]string{
		`{"type":"auto"}`:               `"auto"`,
		`{"type":"any"}`:                `"required"`,
		`{"type":"none"}`:               `"none"`,
		`{"type":"tool","name":"Read"}`: `{"type":"function","name":"Read"}`,
	}
	for choice, want := range cases {
		body := mustConvert(t, Options{}, `{
			"model":"gpt-5.5",
			"messages":[{"role":"user","content":"go"}],
			"tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}}],
			"tool_choice":`+choice+`
		}`)
		if got := body.Get("tool_choice").Raw; got != want {
			t.Fatalf("tool_choice %s became %s, want %s", choice, got, want)
		}
	}
}

func TestDisableParallelToolUse(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	if body.Get("parallel_tool_calls").Bool() {
		t.Fatal("parallel_tool_calls must follow disable_parallel_tool_use")
	}
}

func TestEffortSelection(t *testing.T) {
	explicit := mustConvert(t, Options{Effort: "high"}, `{"model":"gpt-5.5","messages":[]}`)
	if got := explicit.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("explicit effort = %q, want high", got)
	}

	fromBudget := mustConvert(t, Options{}, `{"model":"gpt-5.5","messages":[],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	if got := fromBudget.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("budget 31999 mapped to %q, want high", got)
	}

	disabled := mustConvert(t, Options{}, `{"model":"gpt-5.5","messages":[],"thinking":{"type":"disabled"}}`)
	if got := disabled.Get("reasoning.effort").String(); got != "low" {
		t.Fatalf("disabled thinking mapped to %q, want low", got)
	}

	fallback := mustConvert(t, Options{DefaultEffort: "medium"}, `{"model":"gpt-5.5","messages":[]}`)
	if got := fallback.Get("reasoning.effort").String(); got != "medium" {
		t.Fatalf("fallback effort = %q, want medium", got)
	}
}

func TestFixedUpstreamFields(t *testing.T) {
	body := mustConvert(t, Options{}, `{"model":"gpt-5.5","messages":[{"role":"user","content":"go"}]}`)

	if !body.Get("stream").Bool() {
		t.Fatal("stream must be set: the Codex backend only serves streaming responses")
	}
	if body.Get("store").Bool() {
		t.Fatal("store must be false")
	}
	if got := body.Get("include.0").String(); got != "reasoning.encrypted_content" {
		t.Fatalf("include = %s, want reasoning.encrypted_content", body.Get("include").Raw)
	}
	if got := body.Get("reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto so thinking text is streamed", got)
	}
	if !body.Get("instructions").Exists() {
		t.Fatal("instructions must be present, even empty")
	}
}

func TestImageAndDocumentParts(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"BBBB"}},
			{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"CCCC"}}
		]}]
	}`)

	parts := body.Get("input.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("emitted %d parts, want 2 (the non-PDF document is dropped): %s", len(parts), body.Get("input.0.content").Raw)
	}
	if got := parts[0].Get("image_url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("image_url = %q", got)
	}
	if got := parts[1].Get("file_data").String(); !strings.HasPrefix(got, "data:application/pdf;base64,") {
		t.Fatalf("file_data = %q", got)
	}
}

func TestInvalidJSONIsRejected(t *testing.T) {
	if _, err := ClaudeToCodex(Options{Model: "gpt-5.5"}, []byte(`{not json`)); err == nil {
		t.Fatal("invalid JSON must be rejected")
	}
}

func TestStripProxySignaturesLeavesForeignRequestsUntouched(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"anthropic-signature"}]}]}`)

	out, changed := StripProxySignatures(raw)
	if changed {
		t.Fatal("a request without proxy-issued signatures must not be rewritten")
	}
	if string(out) != string(raw) {
		t.Fatal("the body must be returned byte for byte")
	}
}

func TestStripProxySignaturesRemovesOnlyOurBlocks(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"ours","signature":"` + SignatureMarker + `SIG"},
			{"type":"thinking","thinking":"theirs","signature":"anthropic-signature"},
			{"type":"text","text":"answer"}
		]}
	]}`)

	out, changed := StripProxySignatures(raw)
	if !changed {
		t.Fatal("a request carrying proxy-issued signatures must be rewritten")
	}

	blocks := gjson.GetBytes(out, "messages.1.content").Array()
	if len(blocks) != 2 {
		t.Fatalf("kept %d blocks, want 2: %s", len(blocks), gjson.GetBytes(out, "messages.1.content").Raw)
	}
	if blocks[0].Get("thinking").String() != "theirs" {
		t.Fatalf("wrong block was dropped: %s", blocks[0].Raw)
	}
	if blocks[1].Get("text").String() != "answer" {
		t.Fatalf("text block was lost: %s", blocks[1].Raw)
	}
	// Untouched messages must survive intact.
	if gjson.GetBytes(out, "messages.0.content.0.text").String() != "hi" {
		t.Fatal("an unrelated message was altered")
	}
}

func TestOutputConfigEffortWinsOverThinkingBudget(t *testing.T) {
	// A client that states an effort means it, whatever the token budget
	// alongside it implies.
	body := mustConvert(t, Options{}, `{
		"model":"gpt-5.5",
		"messages":[],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"output_config":{"effort":"xhigh"}
	}`)
	if got := body.Get("reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh", got)
	}
}

func TestAdaptiveThinkingWithoutAnEffort(t *testing.T) {
	body := mustConvert(t, Options{}, `{
		"model":"gpt-5.5",
		"messages":[],
		"thinking":{"type":"adaptive"}
	}`)
	if got := body.Get("reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh for adaptive thinking", got)
	}
}

func TestEffortFrom(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		fallback string
		want     string
	}{
		{"no preference", `{}`, "medium", "medium"},
		{"empty fallback", `{}`, "", "medium"},
		{"explicit effort", `{"output_config":{"effort":"high"}}`, "medium", "high"},
		{"effort is trimmed and lowercased", `{"output_config":{"effort":"  XHigh "}}`, "medium", "xhigh"},
		{"effort beats budget", `{"thinking":{"type":"enabled","budget_tokens":512},"output_config":{"effort":"high"}}`, "medium", "high"},
		{"small budget", `{"thinking":{"type":"enabled","budget_tokens":2048}}`, "medium", "low"},
		{"large budget", `{"thinking":{"type":"enabled","budget_tokens":60000}}`, "medium", "xhigh"},
		{"disabled thinking", `{"thinking":{"type":"disabled"}}`, "medium", "low"},
		{"adaptive thinking", `{"thinking":{"type":"adaptive"}}`, "medium", "xhigh"},
		{"invalid JSON", `nope`, "high", "high"},
	}
	for _, c := range cases {
		if got := EffortFrom([]byte(c.body), c.fallback); got != c.want {
			t.Fatalf("%s: EffortFrom = %q, want %q", c.name, got, c.want)
		}
	}
}
