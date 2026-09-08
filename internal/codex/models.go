package codex

import (
	"sort"
	"strings"
)

// Model describes one Codex model as advertised to Claude Code.
type Model struct {
	ID             string
	DisplayName    string
	ContextLength  int
	MaxOutput      int
	ReasoningLevel []string
	DefaultEffort  string
	SupportsImages bool
}

// Catalog is the set of Codex models reachable with a ChatGPT subscription.
// The ids are the slugs the ChatGPT backend expects verbatim in the request
// body; availability still depends on the signed-in account's plan, so a model
// listed here can legitimately be rejected upstream.
var Catalog = []Model{
	{
		ID:             "gpt-5.6-sol",
		DisplayName:    "GPT-5.6 Sol (Codex)",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
	},
	{
		ID:             "gpt-6-astra",
		DisplayName:    "GPT-6 Astra (Codex)",
		ContextLength:  272000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
	},
	{
		ID:             "gpt-5.6-terra",
		DisplayName:    "GPT-5.6 Terra (Codex)",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
	},
	{
		ID:             "gpt-5.6-luna",
		DisplayName:    "GPT-5.6 Luna (Codex)",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
	},
	{
		ID:             "gpt-5.5",
		DisplayName:    "GPT-5.5 (Codex)",
		ContextLength:  272000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh"},
		DefaultEffort:  "medium",
		SupportsImages: true,
	},
	{
		ID:             "gpt-5.3-codex-spark",
		DisplayName:    "GPT-5.3 Codex Spark",
		ContextLength:  128000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh"},
		DefaultEffort:  "medium",
		SupportsImages: false,
	},
}

// efforts lists every reasoning level the Responses API accepts, longest first
// so suffix matching prefers "xhigh" over "high".
var efforts = []string{"minimal", "xhigh", "medium", "high", "max", "low", "none"}

// Resolution is the outcome of matching a client-supplied model name against
// the Codex catalog.
type Resolution struct {
	// Model is the catalog entry that matched.
	Model Model
	// Effort is the reasoning effort requested through the model name, empty
	// when the name carried none.
	Effort string
	// Requested is the name exactly as the client sent it.
	Requested string
}

// Registry resolves client model names to Codex models. It is immutable after
// construction and safe for concurrent use.
type Registry struct {
	prefix   string
	byID     map[string]Model
	ordered  []Model
	variants bool
}

// NewRegistry builds a registry over the catalog. allow, when non-empty,
// restricts the catalog to those ids. prefix is an optional namespace clients
// may use in front of an id (for example "codex/"). variants controls whether
// per-effort model ids are advertised.
func NewRegistry(allow []string, prefix string, variants bool) *Registry {
	filter := map[string]bool{}
	for _, id := range allow {
		id = strings.ToLower(strings.TrimSpace(id))
		if id != "" {
			filter[id] = true
		}
	}

	// A prefix is always stored with its separator so callers may pass
	// either "codex" or "codex/".
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/") + "/"
	}

	registry := &Registry{
		prefix:   prefix,
		byID:     map[string]Model{},
		variants: variants,
	}
	for _, model := range Catalog {
		if len(filter) > 0 && !filter[strings.ToLower(model.ID)] {
			continue
		}
		registry.byID[strings.ToLower(model.ID)] = model
		registry.ordered = append(registry.ordered, model)
	}
	return registry
}

// Prefix reports the configured model namespace, if any.
func (r *Registry) Prefix() string { return r.prefix }

// Models returns the catalog entries in advertised order.
func (r *Registry) Models() []Model {
	out := make([]Model, len(r.ordered))
	copy(out, r.ordered)
	return out
}

// AdvertisedIDs lists every model id the proxy adds to the Anthropic model
// list, including per-effort variants when enabled.
func (r *Registry) AdvertisedIDs() []string {
	ids := make([]string, 0, len(r.ordered)*3)
	for _, model := range r.ordered {
		ids = append(ids, r.prefix+model.ID)
		if !r.variants {
			continue
		}
		levels := append([]string(nil), model.ReasoningLevel...)
		sort.SliceStable(levels, func(i, j int) bool {
			return effortRank(levels[i]) < effortRank(levels[j])
		})
		for _, level := range levels {
			if level == model.DefaultEffort {
				continue
			}
			ids = append(ids, r.prefix+model.ID+"-"+level)
		}
	}
	return ids
}

func effortRank(level string) int {
	for i, candidate := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if candidate == level {
			return i
		}
	}
	return 99
}

// Resolve matches name against the catalog. The second return value is false
// when the name is not a Codex model, in which case the request must be passed
// through to Anthropic untouched.
//
// Accepted spellings, all case-insensitive:
//
//	gpt-5.5              the model at its default effort
//	gpt-5.5-high         an effort variant
//	gpt-5.5:high         the same, colon separated
//	codex/gpt-5.5-high   with the configured prefix
func (r *Registry) Resolve(name string) (Resolution, bool) {
	requested := strings.TrimSpace(name)
	if requested == "" {
		return Resolution{}, false
	}
	candidate := strings.ToLower(requested)

	if r.prefix != "" {
		candidate = strings.TrimPrefix(candidate, r.prefix)
	} else {
		// Tolerate the conventional namespace even when unconfigured, so a
		// user who types codex/gpt-5.5 is not silently proxied upstream.
		candidate = strings.TrimPrefix(candidate, "codex/")
	}

	// An explicit separator wins over suffix guessing.
	if base, effort, ok := strings.Cut(candidate, ":"); ok {
		if model, found := r.byID[base]; found {
			return Resolution{Model: model, Effort: normalizeEffort(model, effort), Requested: requested}, true
		}
	}

	if model, found := r.byID[candidate]; found {
		return Resolution{Model: model, Requested: requested}, true
	}

	for _, effort := range efforts {
		suffix := "-" + effort
		if !strings.HasSuffix(candidate, suffix) {
			continue
		}
		base := strings.TrimSuffix(candidate, suffix)
		if model, found := r.byID[base]; found {
			return Resolution{Model: model, Effort: normalizeEffort(model, effort), Requested: requested}, true
		}
	}

	return Resolution{}, false
}

// normalizeEffort keeps a requested level only when the model supports it,
// falling back to the closest supported level.
func normalizeEffort(model Model, effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	for _, level := range model.ReasoningLevel {
		if level == effort {
			return effort
		}
	}
	// Clamp to the highest supported level when the model tops out lower
	// than requested (for instance "max" on a model that stops at xhigh).
	requested := effortRank(effort)
	best := ""
	bestRank := -1
	for _, level := range model.ReasoningLevel {
		rank := effortRank(level)
		if rank <= requested && rank > bestRank {
			best, bestRank = level, rank
		}
	}
	if best != "" {
		return best
	}
	return model.DefaultEffort
}

// Lookup returns the catalog entry for an exact id.
func (r *Registry) Lookup(id string) (Model, bool) {
	model, ok := r.byID[strings.ToLower(strings.TrimSpace(id))]
	return model, ok
}
