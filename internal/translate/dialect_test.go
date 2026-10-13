package translate

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The event sequence OpenRouter streamed for z-ai/glm-5.3-flash: the reasoning
// arrives as text, with no encrypted state.
var openRouterReasoningEvents = []string{
	`{"type":"response.created","response":{"id":"gen-1"}}`,
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
	`{"type":"response.content_part.added","output_index":0,"part":{"type":"reasoning_text","text":""}}`,
	`{"type":"response.reasoning_text.delta","output_index":0,"delta":"17 x 25 "}`,
	`{"type":"response.reasoning_text.delta","output_index":0,"delta":"= 425"}`,
	`{"type":"response.reasoning_text.done","output_index":0,"text":"17 x 25 = 425"}`,
	`{"type":"response.content_part.done","output_index":0,"part":{"type":"reasoning_text","text":"17 x 25 = 425"}}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"17 x 25 = 425"}]}}`,
	`{"type":"response.content_part.added","output_index":1,"part":{"type":"output_text"}}`,
	`{"type":"response.output_text.delta","output_index":1,"delta":"425"}`,
	`{"type":"response.content_part.done","output_index":1,"part":{"type":"output_text"}}`,
	`{"type":"response.completed","response":{"id":"gen-1","usage":{"input_tokens":5,"output_tokens":3}}}`,
}

func TestOpenRouterReasoningTextBecomesASignedThinkingBlock(t *testing.T) {
	translator := OpenRouter.NewStreamTranslator("z-ai/glm-5.3-flash", nil)
	assembler := NewMessageAssembler()
	for _, payload := range openRouterReasoningEvents {
		assembler.Write(translator.Event([]byte(payload)))
	}
	assembler.Write(translator.Close())
	message := gjson.ParseBytes(assembler.Message())

	blocks := message.Get("content").Array()
	if len(blocks) != 2 {
		t.Fatalf("content = %s", message.Get("content").Raw)
	}
	if blocks[0].Get("type").String() != "thinking" || blocks[0].Get("thinking").String() != "17 x 25 = 425" {
		t.Fatalf("thinking block = %s", blocks[0].Raw)
	}
	// With no encrypted state the block still has to be signed, so it is
	// recognisably this proxy's and never reaches Anthropic.
	if got := blocks[0].Get("signature").String(); got != OpenRouter.Marker {
		t.Fatalf("signature = %q, want the bare OpenRouter marker", got)
	}
	if blocks[1].Get("text").String() != "425" {
		t.Fatalf("text block = %s", blocks[1].Raw)
	}
}

func TestEachDialectReplaysOnlyItsOwnReasoning(t *testing.T) {
	request := `{
		"model": "m",
		"messages": [
			{"role":"user","content":"q"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex thought","signature":"` + Codex.Marker + `ENC"},
				{"type":"thinking","thinking":"openrouter thought","signature":"` + OpenRouter.Marker + `"},
				{"type":"thinking","thinking":"anthropic thought","signature":"EqQBCkYI"},
				{"type":"text","text":"a"}
			]},
			{"role":"user","content":"next"}
		]
	}`

	reasoning := func(body gjson.Result) []gjson.Result {
		var items []gjson.Result
		for _, item := range body.Get("input").Array() {
			if item.Get("type").String() == "reasoning" {
				items = append(items, item)
			}
		}
		return items
	}

	codex := reasoning(mustConvert(t, Options{Dialect: Codex}, request))
	if len(codex) != 1 || codex[0].Get("encrypted_content").String() != "ENC" {
		t.Fatalf("Codex must replay only its encrypted state: %v", codex)
	}

	openRouter := reasoning(mustConvert(t, Options{Dialect: OpenRouter}, request))
	if len(openRouter) != 1 {
		t.Fatalf("OpenRouter must replay only its own block: %v", openRouter)
	}
	if got := openRouter[0].Get("content.0.text").String(); got != "openrouter thought" || openRouter[0].Get("content.0.type").String() != "reasoning_text" {
		t.Fatalf("OpenRouter reasoning item = %s", openRouter[0].Raw)
	}
	if openRouter[0].Get("encrypted_content").Exists() {
		t.Fatalf("a text replay must not claim encrypted state: %s", openRouter[0].Raw)
	}
}

func TestOpenRouterEncryptedReasoningIsReplayedEncrypted(t *testing.T) {
	// Some models behind OpenRouter do return encrypted state; it wins.
	body := mustConvert(t, Options{Dialect: OpenRouter}, `{
		"model": "m",
		"messages": [
			{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"`+OpenRouter.Marker+`ENC"}]},
			{"role":"user","content":"next"}
		]
	}`)
	if got := body.Get("input.0.encrypted_content").String(); got != "ENC" {
		t.Fatalf("input = %s", body.Get("input").Raw)
	}
}

func TestStripProxySignaturesRemovesEveryDialect(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"c","signature":"` + Codex.Marker + `X"},` +
		`{"type":"thinking","thinking":"o","signature":"` + OpenRouter.Marker + `"},` +
		`{"type":"thinking","thinking":"a","signature":"EqQBCkYI"},` +
		`{"type":"text","text":"kept"}]}]}`)

	cleaned, changed := StripProxySignatures(raw)
	if !changed {
		t.Fatal("proxy-issued blocks must be removed")
	}
	content := gjson.GetBytes(cleaned, "messages.0.content")
	if len(content.Array()) != 2 || strings.Contains(content.Raw, Codex.Marker) || strings.Contains(content.Raw, OpenRouter.Marker) {
		t.Fatalf("content = %s, want only Anthropic's thinking and the text", content.Raw)
	}
}
