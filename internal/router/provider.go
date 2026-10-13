package router

import (
	"context"
	"net/http"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/translate"
)

// Upstream sends a translated Responses API request and returns the event
// stream. The caller owns the response body.
type Upstream interface {
	Responses(ctx context.Context, body []byte, sessionID string) (*http.Response, error)
}

// Provider is one backend that answers the requests naming its own models.
// Every provider speaks the Responses API; what differs is its catalog, its
// credentials and the Dialect its reasoning is replayed in.
type Provider struct {
	// Name labels the provider in logs and on the status endpoint.
	Name string
	// Registry holds the models the provider answers for.
	Registry *catalog.Registry
	// Dialect is passed to the translator in both directions.
	Dialect translate.Dialect
	// Upstream is nil when the provider is not configured.
	Upstream Upstream
	// Ready reports whether a request can be served now: signed in, or an
	// API key present. Nil means always ready.
	Ready func() bool
}

// ready reports whether the provider can answer a request right now.
func (p *Provider) ready() bool {
	return p.Upstream != nil && (p.Ready == nil || p.Ready())
}

// resolve finds the provider a client model name belongs to. Providers are
// asked in order and the first match wins; false means the request is
// Anthropic's.
func (r *Router) resolve(model string) (*Provider, catalog.Resolution, bool) {
	for _, provider := range r.providers {
		if resolution, ok := provider.Registry.Resolve(model); ok {
			return provider, resolution, true
		}
	}
	return nil, catalog.Resolution{}, false
}

// anyReady reports whether at least one provider can serve requests.
func (r *Router) anyReady() bool {
	for _, provider := range r.providers {
		if provider.ready() {
			return true
		}
	}
	return false
}
