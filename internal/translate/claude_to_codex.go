// Package translate converts between the Anthropic Messages API that Claude
// Code speaks and the OpenAI Responses API exposed by the Codex backend.
package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SignatureMarker prefixes every thinking signature the proxy hands to Claude
// Code. Codex reasoning state and Anthropic reasoning signatures are not
// interchangeable, so the marker lets both directions recognise their own
// blocks and drop the other's instead of failing a request upstream.
const SignatureMarker = "ccmp1:"

// nameLimit is the maximum function name length the Responses API accepts.
const nameLimit = 64

// Request is the outcome of translating one Claude request.
type Request struct {
	// Body is the JSON payload to POST to the Codex Responses endpoint.
	Body []byte
	// ToolNames maps the shortened names sent upstream back to the names the
	// client used, so tool_use blocks can be reported under the original name.
	ToolNames map[string]string
}

// Options controls the translation.
type Options struct {
	// Model is the Codex slug to request.
	Model string
	// Effort overrides the reasoning effort. When empty the effort is derived
	// from the Claude thinking configuration.
	Effort string
	// DefaultEffort is used when neither Effort nor the request implies one.
	DefaultEffort string
	// Dialect is the backend the request is for; the zero value is Codex.
	Dialect Dialect
}

// ClaudeToCodex translates an Anthropic Messages request into a Codex
// Responses request. The Codex backend only serves streaming responses, so the
// generated body always sets stream.
func ClaudeToCodex(opts Options, raw []byte) (*Request, error) {
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("translate: request body is not valid JSON")
	}
	root := gjson.ParseBytes(raw)

	body := []byte(`{"model":"","instructions":"","input":[]}`)
	body, _ = sjson.SetBytes(body, "model", opts.Model)

	shortNames, originalNames := buildToolNameMaps(root.Get("tools"))

	items := make([][]byte, 0, 16)
	items = appendSystemItem(items, root.Get("system"))
	items = appendMessageItems(items, root.Get("messages"), shortNames, opts.Dialect.orDefault())
	body = setRawArray(body, "input", items)

	if tools := root.Get("tools"); tools.IsArray() {
		converted := convertTools(tools, shortNames)
		body = setRawArray(body, "tools", converted)
		body, _ = sjson.SetRawBytes(body, "tool_choice", convertToolChoice(root.Get("tool_choice"), shortNames))
	}

	parallel := true
	if disable := root.Get("tool_choice.disable_parallel_tool_use"); disable.Exists() {
		parallel = !disable.Bool()
	}
	body, _ = sjson.SetBytes(body, "parallel_tool_calls", parallel)

	effort := opts.Effort
	if effort == "" {
		effort = effortFrom(root, opts.DefaultEffort)
	}
	body, _ = sjson.SetBytes(body, "reasoning.effort", effort)
	// Reasoning summaries are opt-in on the Responses API and are what becomes
	// the thinking blocks Claude Code renders.
	body, _ = sjson.SetBytes(body, "reasoning.summary", "auto")

	body, _ = sjson.SetBytes(body, "store", false)
	body, _ = sjson.SetBytes(body, "stream", true)
	// The encrypted reasoning state must be echoed back on the next turn for
	// the model to keep its chain of thought across tool calls.
	body, _ = sjson.SetBytes(body, "include", []string{"reasoning.encrypted_content"})

	if format := root.Get("output_config.format"); format.IsObject() && format.Get("type").String() == "json_schema" && format.Get("schema").IsObject() {
		body, _ = sjson.SetRawBytes(body, "text.format", convertJSONSchemaFormat(format))
	}

	return &Request{Body: body, ToolNames: originalNames}, nil
}

// appendSystemItem folds the Claude system prompt into a developer message,
// which is where the Responses API expects out-of-band instructions.
func appendSystemItem(items [][]byte, system gjson.Result) [][]byte {
	if !system.Exists() {
		return items
	}

	parts := make([][]byte, 0, 2)
	add := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		part := []byte(`{"type":"input_text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", text)
		parts = append(parts, part)
	}

	switch {
	case system.Type == gjson.String:
		add(system.String())
	case system.IsArray():
		for _, block := range system.Array() {
			if block.Get("type").String() == "text" {
				add(block.Get("text").String())
			}
		}
	}

	if len(parts) == 0 {
		return items
	}
	message := []byte(`{"type":"message","role":"developer"}`)
	message, _ = sjson.SetRawBytes(message, "content", joinRawArray(parts))
	return append(items, message)
}

func appendMessageItems(items [][]byte, messages gjson.Result, shortNames map[string]string, dialect Dialect) [][]byte {
	if !messages.IsArray() {
		return items
	}

	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			// Anthropic requests only carry user and assistant turns; a
			// stray system message is folded in as user text.
			role = "user"
		}
		content := message.Get("content")

		parts := make([][]byte, 0, 4)
		flush := func() {
			if len(parts) == 0 {
				return
			}
			item := []byte(`{"type":"message","role":""}`)
			item, _ = sjson.SetBytes(item, "role", role)
			item, _ = sjson.SetRawBytes(item, "content", joinRawArray(parts))
			items = append(items, item)
			parts = parts[:0]
		}

		if content.Type == gjson.String {
			parts = append(parts, textPart(role, content.String()))
			flush()
			continue
		}
		if !content.IsArray() {
			continue
		}

		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "text":
				parts = append(parts, textPart(role, block.Get("text").String()))
			case "image":
				if part, ok := imagePart(block.Get("source")); ok {
					parts = append(parts, part)
				}
			case "document":
				if part, ok := documentPart(block.Get("source")); ok {
					parts = append(parts, part)
				}
			case "thinking":
				// Only reasoning this backend produced can be replayed to it;
				// anything else is dropped rather than rejected upstream.
				if item, ok := reasoningItem(block, dialect); ok {
					flush()
					items = append(items, item)
				}
			case "tool_use":
				flush()
				items = append(items, functionCallItem(block, shortNames))
			case "tool_result":
				flush()
				items = append(items, functionCallOutputItem(block))
			}
		}
		flush()
	}
	return orderToolOutputs(items)
}

// orderToolOutputs puts each run of consecutive function_call_output items in
// the order of the function_call items they answer.
//
// Claude Code appends parallel tool results in the order the tools finish,
// not the order they were called. The call_ids still pair them correctly, but
// Codex models read the outputs positionally and attribute them to the wrong
// call when the two orders differ. Outputs whose call is unknown keep their
// place after the ones that are.
func orderToolOutputs(items [][]byte) [][]byte {
	callIndex := make(map[string]int)
	for i := 0; i < len(items); i++ {
		switch gjson.GetBytes(items[i], "type").String() {
		case "function_call":
			callIndex[gjson.GetBytes(items[i], "call_id").String()] = i
			continue
		case "function_call_output":
		default:
			continue
		}

		end := i + 1
		for end < len(items) && gjson.GetBytes(items[end], "type").String() == "function_call_output" {
			end++
		}
		run := items[i:end]
		rank := func(item []byte) int {
			if index, ok := callIndex[gjson.GetBytes(item, "call_id").String()]; ok {
				return index
			}
			return len(items)
		}
		sort.SliceStable(run, func(a, b int) bool { return rank(run[a]) < rank(run[b]) })
		i = end - 1
	}
	return items
}

func textPart(role, text string) []byte {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	part := []byte(`{"type":"","text":""}`)
	part, _ = sjson.SetBytes(part, "type", partType)
	part, _ = sjson.SetBytes(part, "text", text)
	return part
}

func imagePart(source gjson.Result) ([]byte, bool) {
	if !source.Exists() {
		return nil, false
	}
	if url := source.Get("url").String(); url != "" {
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", url)
		return part, true
	}
	data := source.Get("data").String()
	if data == "" {
		return nil, false
	}
	mediaType := source.Get("media_type").String()
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	part := []byte(`{"type":"input_image","image_url":""}`)
	part, _ = sjson.SetBytes(part, "image_url", fmt.Sprintf("data:%s;base64,%s", mediaType, data))
	return part, true
}

func documentPart(source gjson.Result) ([]byte, bool) {
	if source.Get("type").String() != "base64" {
		return nil, false
	}
	mediaType := strings.TrimSpace(source.Get("media_type").String())
	if !strings.EqualFold(mediaType, "application/pdf") {
		return nil, false
	}
	data := source.Get("data").String()
	if data == "" {
		return nil, false
	}
	part := []byte(`{"type":"input_file","file_data":"","filename":"document.pdf"}`)
	part, _ = sjson.SetBytes(part, "file_data", fmt.Sprintf("data:%s;base64,%s", mediaType, data))
	return part, true
}

func functionCallItem(block gjson.Result, shortNames map[string]string) []byte {
	item := []byte(`{"type":"function_call"}`)
	item, _ = sjson.SetBytes(item, "call_id", shortenCallID(block.Get("id").String()))
	item, _ = sjson.SetBytes(item, "name", shortenToolName(block.Get("name").String(), shortNames))
	arguments := block.Get("input").Raw
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	item, _ = sjson.SetBytes(item, "arguments", arguments)
	return item
}

func functionCallOutputItem(block gjson.Result) []byte {
	item := []byte(`{"type":"function_call_output"}`)
	item, _ = sjson.SetBytes(item, "call_id", shortenCallID(block.Get("tool_use_id").String()))

	content := block.Get("content")
	if content.IsArray() {
		parts := make([][]byte, 0, len(content.Array()))
		for _, entry := range content.Array() {
			switch entry.Get("type").String() {
			case "text":
				part := []byte(`{"type":"input_text","text":""}`)
				part, _ = sjson.SetBytes(part, "text", entry.Get("text").String())
				parts = append(parts, part)
			case "image":
				if part, ok := imagePart(entry.Get("source")); ok {
					parts = append(parts, part)
				}
			}
		}
		if len(parts) > 0 {
			item, _ = sjson.SetRawBytes(item, "output", joinRawArray(parts))
			return item
		}
	}
	item, _ = sjson.SetBytes(item, "output", content.String())
	return item
}

func convertTools(tools gjson.Result, shortNames map[string]string) [][]byte {
	entries := tools.Array()
	converted := make([][]byte, 0, len(entries))
	for _, tool := range entries {
		if isWebSearchTool(tool.Get("type").String()) {
			converted = append(converted, convertWebSearchTool(tool))
			continue
		}
		name := tool.Get("name").String()
		if name == "" {
			continue
		}

		out := []byte(`{"type":"function","name":"","strict":false}`)
		out, _ = sjson.SetBytes(out, "name", shortenToolName(name, shortNames))
		if description := tool.Get("description"); description.Exists() {
			out, _ = sjson.SetBytes(out, "description", description.String())
		}
		out, _ = sjson.SetRawBytes(out, "parameters", normalizeSchema(tool.Get("input_schema")))
		converted = append(converted, out)
	}
	return converted
}

func isWebSearchTool(toolType string) bool {
	return strings.HasPrefix(toolType, "web_search_")
}

func convertWebSearchTool(tool gjson.Result) []byte {
	out := []byte(`{"type":"web_search"}`)
	if allowed := tool.Get("allowed_domains"); allowed.IsArray() {
		out, _ = sjson.SetRawBytes(out, "filters.allowed_domains", []byte(allowed.Raw))
	}
	if location := tool.Get("user_location"); location.IsObject() {
		out, _ = sjson.SetRawBytes(out, "user_location", []byte(location.Raw))
	}
	return out
}

func convertToolChoice(choice gjson.Result, shortNames map[string]string) []byte {
	kind := choice.Get("type").String()
	if kind == "" && choice.Type == gjson.String {
		kind = choice.String()
	}
	switch kind {
	case "any":
		return []byte(`"required"`)
	case "none":
		return []byte(`"none"`)
	case "tool":
		name := shortenToolName(choice.Get("name").String(), shortNames)
		if name == "" {
			return []byte(`"auto"`)
		}
		out := []byte(`{"type":"function","name":""}`)
		out, _ = sjson.SetBytes(out, "name", name)
		return out
	default:
		return []byte(`"auto"`)
	}
}

// normalizeSchema makes a Claude input_schema acceptable as Responses tool
// parameters: object schemas need a properties map, and the $schema keyword is
// rejected.
func normalizeSchema(schema gjson.Result) []byte {
	raw := strings.TrimSpace(schema.Raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return []byte(`{"type":"object","properties":{}}`)
	}
	out := []byte(raw)
	kind := schema.Get("type").String()
	if kind == "" {
		out, _ = sjson.SetBytes(out, "type", "object")
		kind = "object"
	}
	if kind == "object" && !schema.Get("properties").Exists() {
		out, _ = sjson.SetRawBytes(out, "properties", []byte(`{}`))
	}
	// The Responses API rejects the $schema keyword Claude tools often carry.
	if gjson.GetBytes(out, "$schema").Exists() {
		out, _ = sjson.DeleteBytes(out, "$schema")
	}
	return out
}

func convertJSONSchemaFormat(format gjson.Result) []byte {
	name := format.Get("name").String()
	if name == "" {
		name = "structured_output"
	}
	strict := true
	if value := format.Get("strict"); value.Exists() && value.Type == gjson.False {
		strict = false
	}
	// Strict mode requires every declared property to be listed as required;
	// downgrading beats an upstream 400 on a schema that cannot be satisfied.
	if strict && schemaMissesRequired(format.Get("schema")) {
		strict = false
	}
	out := []byte(`{"type":"json_schema","name":"","strict":true,"schema":{}}`)
	out, _ = sjson.SetBytes(out, "name", name)
	out, _ = sjson.SetBytes(out, "strict", strict)
	out, _ = sjson.SetRawBytes(out, "schema", []byte(format.Get("schema").Raw))
	return out
}

func schemaMissesRequired(schema gjson.Result) bool {
	if schema.IsArray() {
		missing := false
		schema.ForEach(func(_, child gjson.Result) bool {
			if schemaMissesRequired(child) {
				missing = true
				return false
			}
			return true
		})
		return missing
	}
	if !schema.IsObject() {
		return false
	}
	if properties := schema.Get("properties"); properties.IsObject() {
		required := schema.Get("required")
		if !required.IsArray() {
			return len(properties.Map()) > 0
		}
		names := make(map[string]struct{}, len(required.Array()))
		for _, entry := range required.Array() {
			names[entry.String()] = struct{}{}
		}
		for name := range properties.Map() {
			if _, ok := names[name]; !ok {
				return true
			}
		}
	}
	for _, keyword := range []string{"properties", "$defs", "definitions", "patternProperties"} {
		children := schema.Get(keyword)
		if !children.IsObject() {
			continue
		}
		missing := false
		children.ForEach(func(_, child gjson.Result) bool {
			if schemaMissesRequired(child) {
				missing = true
				return false
			}
			return true
		})
		if missing {
			return true
		}
	}
	for _, keyword := range []string{"items", "prefixItems", "anyOf", "oneOf", "allOf", "not", "if", "then", "else", "contains"} {
		if child := schema.Get(keyword); child.Exists() && schemaMissesRequired(child) {
			return true
		}
	}
	return false
}

// EffortFrom reads the client's reasoning preference out of an Anthropic
// Messages request and maps it onto a Responses reasoning effort. fallback is
// returned when the request states no preference.
//
// An explicit output_config.effort wins over everything else: it is what
// Claude Code's /effort and --effort set, and it says exactly what the user
// asked for rather than implying it through a token budget.
func EffortFrom(raw []byte, fallback string) string {
	return effortFrom(gjson.ParseBytes(raw), fallback)
}

func effortFrom(root gjson.Result, fallback string) string {
	if fallback == "" {
		fallback = "medium"
	}
	if effort := root.Get("output_config.effort"); effort.Type == gjson.String {
		if value := strings.ToLower(strings.TrimSpace(effort.String())); value != "" {
			return value
		}
	}

	thinking := root.Get("thinking")
	if !thinking.IsObject() {
		return fallback
	}

	switch thinking.Get("type").String() {
	case "enabled":
		budget := thinking.Get("budget_tokens").Int()
		switch {
		case budget <= 0:
			return "low"
		case budget <= 4096:
			return "low"
		case budget <= 16384:
			return "medium"
		case budget <= 32768:
			return "high"
		default:
			return "xhigh"
		}
	case "adaptive", "auto":
		// Adaptive thinking with no stated effort means "think as needed".
		return "xhigh"
	case "disabled":
		return "low"
	}
	return fallback
}

// buildToolNameMaps returns original->short and short->original maps for the
// request's tools, keeping the shortened names unique.
func buildToolNameMaps(tools gjson.Result) (map[string]string, map[string]string) {
	short := map[string]string{}
	original := map[string]string{}
	if !tools.IsArray() {
		return short, original
	}

	names := make([]string, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		if name := tool.Get("name").String(); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	used := map[string]struct{}{}
	for _, name := range names {
		candidate := truncateToolName(name)
		if _, taken := used[candidate]; taken {
			base := candidate
			for i := 1; ; i++ {
				suffix := "_" + strconv.Itoa(i)
				trimmed := base
				if len(trimmed) > nameLimit-len(suffix) {
					trimmed = trimmed[:nameLimit-len(suffix)]
				}
				candidate = trimmed + suffix
				if _, taken := used[candidate]; !taken {
					break
				}
			}
		}
		used[candidate] = struct{}{}
		short[name] = candidate
		original[candidate] = name
	}
	return short, original
}

func truncateToolName(name string) string {
	if len(name) <= nameLimit {
		return name
	}
	// MCP tools are namespaced as mcp__<server>__<tool>; keeping the leaf name
	// is far more informative than a blind prefix cut.
	if strings.HasPrefix(name, "mcp__") {
		if idx := strings.LastIndex(name, "__"); idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > nameLimit {
				return candidate[:nameLimit]
			}
			return candidate
		}
	}
	return name[:nameLimit]
}

func shortenToolName(name string, shortNames map[string]string) string {
	if name == "" {
		return ""
	}
	if short, ok := shortNames[name]; ok {
		return short
	}
	return truncateToolName(name)
}

// shortenCallID keeps tool ids inside the Responses call_id limit while
// staying stable and collision resistant.
func shortenCallID(id string) string {
	if len(id) <= nameLimit {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	return id[:nameLimit-len(suffix)] + suffix
}

// reasoningItem turns a thinking block back into the reasoning item the
// backend produced it from: its encrypted state when it has one, otherwise
// its text when the dialect can replay text.
func reasoningItem(block gjson.Result, dialect Dialect) ([]byte, bool) {
	encrypted, ok := dialect.unwrap(block.Get("signature").String())
	if !ok {
		return nil, false
	}
	if encrypted != "" {
		item := []byte(`{"type":"reasoning","summary":[],"content":null}`)
		item, _ = sjson.SetBytes(item, "encrypted_content", encrypted)
		return item, true
	}
	text := block.Get("thinking").String()
	if !dialect.ReplayText || strings.TrimSpace(text) == "" {
		return nil, false
	}
	item := []byte(`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":""}]}`)
	item, _ = sjson.SetBytes(item, "content.0.text", text)
	return item, true
}

// MarkSignature tags Codex reasoning state as this proxy's own.
func MarkSignature(encrypted string) string {
	return Codex.sign(encrypted)
}

func joinRawArray(items [][]byte) []byte {
	out := make([]byte, 0, 64)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}

func setRawArray(body []byte, path string, items [][]byte) []byte {
	out, _ := sjson.SetRawBytes(body, path, joinRawArray(items))
	return out
}
