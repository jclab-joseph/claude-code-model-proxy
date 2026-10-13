package translate

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// StripProxySignatures removes assistant thinking blocks whose signature this
// proxy issued for a turn answered by one of its backends.
//
// Claude Code replays prior assistant turns verbatim, so a session that
// switched from an added model back to a Claude model would otherwise send
// that backend's reasoning to Anthropic, which rejects the request. The check
// is a byte scan first, so requests that never touched an added model are
// returned untouched and pass through unmodified.
func StripProxySignatures(raw []byte) ([]byte, bool) {
	if !containsAnyMarker(raw) {
		return raw, false
	}
	messages := gjson.GetBytes(raw, "messages")
	if !messages.IsArray() {
		return raw, false
	}

	out := raw
	changed := false
	// Rebuild each affected message's content without the foreign blocks,
	// walking messages in order and rewriting only those that need it.
	messages.ForEach(func(messageIndex, message gjson.Result) bool {
		content := message.Get("content")
		if !content.IsArray() {
			return true
		}
		kept := make([][]byte, 0, len(content.Array()))
		dropped := false
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "thinking" {
				if issuedByProxy(block.Get("signature").String()) {
					dropped = true
					return true
				}
			}
			kept = append(kept, []byte(block.Raw))
			return true
		})
		if !dropped {
			return true
		}
		changed = true
		out, _ = sjson.SetRawBytes(out, "messages."+messageIndex.String()+".content", joinRawArray(kept))
		return true
	})

	return out, changed
}

func containsAnyMarker(raw []byte) bool {
	for _, dialect := range dialects {
		if bytes.Contains(raw, []byte(dialect.Marker)) {
			return true
		}
	}
	return false
}
