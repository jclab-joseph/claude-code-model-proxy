// Package tokenize provides the token estimate the proxy answers
// /v1/messages/count_tokens with for models that have no upstream counting
// endpoint.
package tokenize

import (
	"github.com/tidwall/gjson"
)

// charsPerToken is the ratio used to approximate tokens from text length. It
// is deliberately conservative: Claude Code uses the count to decide when to
// compact a conversation, so overestimating degrades gracefully while
// underestimating risks an upstream context overflow.
const charsPerToken = 3.5

// perBlockOverhead accounts for the role and block framing each content block
// costs once serialized.
const perBlockOverhead = 4

// EstimateClaudeRequest approximates the prompt token count of an Anthropic
// Messages request. It is an estimate, not a measurement: the Codex backend
// does not expose a counting endpoint, and its tokenizer is not public.
func EstimateClaudeRequest(raw []byte) int64 {
	root := gjson.ParseBytes(raw)
	var chars int64
	var blocks int64

	countText := func(text string) {
		chars += int64(len([]rune(text)))
	}

	system := root.Get("system")
	if system.Type == gjson.String {
		countText(system.String())
		blocks++
	} else if system.IsArray() {
		system.ForEach(func(_, block gjson.Result) bool {
			countText(block.Get("text").String())
			blocks++
			return true
		})
	}

	root.Get("messages").ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if content.Type == gjson.String {
			countText(content.String())
			blocks++
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			blocks++
			switch block.Get("type").String() {
			case "text":
				countText(block.Get("text").String())
			case "thinking":
				countText(block.Get("thinking").String())
			case "tool_use":
				countText(block.Get("name").String())
				countText(block.Get("input").Raw)
			case "tool_result":
				result := block.Get("content")
				if result.Type == gjson.String {
					countText(result.String())
					return true
				}
				result.ForEach(func(_, entry gjson.Result) bool {
					countText(entry.Get("text").String())
					// Images are billed by size, not text; this is a flat
					// stand-in for a tile-based count.
					if entry.Get("type").String() == "image" {
						chars += 1000
					}
					return true
				})
			case "image":
				chars += 1000
			}
			return true
		})
		return true
	})

	// Tool declarations are part of the prompt and are frequently the largest
	// fixed cost in a Claude Code request.
	root.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		blocks++
		countText(tool.Get("name").String())
		countText(tool.Get("description").String())
		countText(tool.Get("input_schema").Raw)
		return true
	})

	estimate := int64(float64(chars)/charsPerToken) + blocks*perBlockOverhead
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}
