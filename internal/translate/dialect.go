package translate

import "strings"

// Dialect is what differs between the Responses API backends a Claude request
// can be translated for. The request and event shapes are shared; what a
// backend can replay of its own reasoning is not.
type Dialect struct {
	// Name labels the backend in error messages.
	Name string
	// Marker prefixes the signature of every thinking block this backend
	// produced. Each backend replays only blocks carrying its own marker, and
	// StripProxySignatures removes every marker's blocks before a request
	// reaches Anthropic, which would reject them.
	Marker string
	// ReplayText replays a thinking block that has no encrypted state as
	// plain reasoning text. Codex reasoning is only usable through its
	// encrypted state; OpenRouter streams the reasoning itself and usually
	// has no encrypted state to give.
	ReplayText bool
}

// Codex is the ChatGPT subscription backend. Its marker predates dialects and
// stays SignatureMarker so sessions recorded by older builds still replay.
var Codex = Dialect{Name: "Codex", Marker: SignatureMarker}

// OpenRouter is OpenRouter's Responses API.
var OpenRouter = Dialect{Name: "OpenRouter", Marker: "ccmpor1:", ReplayText: true}

// dialects lists every marker the proxy has issued.
var dialects = []Dialect{Codex, OpenRouter}

// orDefault makes the zero Dialect mean Codex, the only backend that existed
// before dialects did.
func (d Dialect) orDefault() Dialect {
	if d.Marker == "" {
		return Codex
	}
	return d
}

// sign renders the signature for a thinking block this backend produced. It
// is empty only when there is nothing the backend could replay.
func (d Dialect) sign(encrypted string) string {
	if encrypted == "" && !d.ReplayText {
		return ""
	}
	return d.Marker + encrypted
}

// unwrap removes this backend's marker, reporting whether it was present.
func (d Dialect) unwrap(signature string) (string, bool) {
	if !strings.HasPrefix(signature, d.Marker) {
		return "", false
	}
	return strings.TrimPrefix(signature, d.Marker), true
}

// issuedByProxy reports whether any backend of this proxy produced signature.
func issuedByProxy(signature string) bool {
	for _, dialect := range dialects {
		if _, ok := dialect.unwrap(signature); ok {
			return true
		}
	}
	return false
}
