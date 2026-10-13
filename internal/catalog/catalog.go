// Package catalog describes the models this proxy adds to Claude Code and
// resolves the names a client sends to them. Each provider supplies its own
// models; the matching rules and the shapes Claude Code expects are shared.
package catalog

import (
	"regexp"
	"sort"
	"strings"
)

// Model describes one added model as advertised to Claude Code.
type Model struct {
	ID             string
	DisplayName    string
	Description    string
	ContextLength  int
	MaxOutput      int
	ReasoningLevel []string
	DefaultEffort  string
	SupportsImages bool
	// BehavesAs names the Claude model whose client-side handling Claude
	// Code should apply to this one. It is required for a `modelPicker` row:
	// a row naming a model the installed version does not know is not
	// offered without it. The value only affects the client's own prompt
	// profile and effort defaults; the model id sent upstream is unchanged.
	BehavesAs string
}

// contextWindowSuffix matches the [1m]/[2m] context-window hint Claude Code
// accepts on a model name.
var contextWindowSuffix = regexp.MustCompile(`\[[12]m\]$`)

// efforts lists every reasoning level the Responses API accepts, longest first
// so suffix matching prefers "xhigh" over "high".
var efforts = []string{"minimal", "xhigh", "medium", "high", "max", "low", "none"}

// Resolution is the outcome of matching a client-supplied model name against
// a registry.
type Resolution struct {
	// Model is the catalog entry that matched.
	Model Model
	// Effort is the reasoning effort requested through the model name, empty
	// when the name carried none.
	Effort string
	// Requested is the name exactly as the client sent it.
	Requested string
}

// Registry resolves client model names to one provider's models. It is
// immutable after construction and safe for concurrent use.
type Registry struct {
	prefix string
	// namespace is accepted in front of an id when no prefix is configured,
	// so a user who types the conventional form is not silently proxied to
	// Anthropic.
	namespace string
	byID      map[string]Model
	ordered   []Model
	variants  bool
}

// New builds a registry over models. allow, when non-empty, restricts it to
// those ids. prefix is an optional namespace clients may use in front of an id
// (for example "codex/"); namespace is the one accepted when prefix is empty.
// variants controls whether per-effort model ids are advertised.
func New(models []Model, allow []string, prefix, namespace string, variants bool) *Registry {
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

	namespace = strings.ToLower(strings.TrimSpace(namespace))
	if namespace != "" {
		namespace = strings.TrimSuffix(namespace, "/") + "/"
	}

	registry := &Registry{
		prefix:    prefix,
		namespace: namespace,
		byID:      map[string]Model{},
		variants:  variants,
	}
	for _, model := range models {
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

// Resolve matches name against the registry. The second return value is false
// when the name is not one of its models, in which case another provider or
// Anthropic handles the request.
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
	// Claude Code lets a user append [1m] or [2m] to ask for a larger
	// context window. The suffix is a client-side hint and is not part of
	// any upstream model id, so it is stripped before matching.
	candidate = contextWindowSuffix.ReplaceAllString(candidate, "")

	if r.prefix != "" {
		candidate = strings.TrimPrefix(candidate, r.prefix)
	} else if r.namespace != "" {
		candidate = strings.TrimPrefix(candidate, r.namespace)
	}

	// An exact id wins, since some providers put a colon in their ids
	// (OpenRouter's z-ai/glm-5.3-flash:batch).
	if model, found := r.byID[candidate]; found {
		return Resolution{Model: model, Requested: requested}, true
	}

	// An explicit separator wins over suffix guessing.
	if colon := strings.LastIndex(candidate, ":"); colon >= 0 {
		base, effort := candidate[:colon], candidate[colon+1:]
		if model, found := r.byID[base]; found {
			return Resolution{Model: model, Effort: normalizeEffort(model, effort), Requested: requested}, true
		}
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

// ClampEffort keeps a requested reasoning level only when this model supports
// it, otherwise returning the closest level it does. An empty effort stays
// empty, meaning "no preference".
//
// Claude Code offers effort levels based on the model its picker row says to
// behave as, which can be a level the real model does not have.
func (m Model) ClampEffort(effort string) string {
	return normalizeEffort(m, effort)
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

// PickerRow is one entry of the `modelPicker.options` array in Claude Code's
// settings.json. The field names and semantics are Claude Code's, not this
// proxy's.
type PickerRow struct {
	// Model is the id to send, taken verbatim; the same value --model takes.
	Model string `json:"model"`
	// Label is the row title, Description its subtitle.
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	// BehavesAs names a model the installed Claude Code knows, whose
	// client-side handling applies. Without it a row for an unknown model
	// is not offered at all.
	BehavesAs string `json:"behavesAs,omitempty"`
}

// PickerOptions renders one settings.json picker row per advertised model.
func (r *Registry) PickerOptions() []PickerRow {
	ids := r.AdvertisedIDs()
	rows := make([]PickerRow, 0, len(ids))
	for _, id := range ids {
		resolution, ok := r.Resolve(id)
		if !ok {
			continue
		}
		label := resolution.Model.DisplayName
		description := resolution.Model.Description
		if resolution.Effort != "" {
			// The effort is part of the id, so it belongs in the label the
			// user picks from.
			label = label + " · " + resolution.Effort
			description = strings.TrimSuffix(description, ".") + ". Reasoning effort: " + resolution.Effort + "."
		}
		rows = append(rows, PickerRow{
			Model:       id,
			Label:       label,
			Description: description,
			BehavesAs:   resolution.Model.BehavesAs,
		})
	}
	return rows
}

// BootstrapOption is one entry of the `additional_model_options` array in the
// Claude Code bootstrap response. The field names are Claude Code's.
type BootstrapOption struct {
	Model          string  `json:"model"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	DisabledReason *string `json:"disabled_reason"`
}

// BootstrapOptions renders one bootstrap model option per advertised model.
func (r *Registry) BootstrapOptions() []BootstrapOption {
	rows := r.PickerOptions()
	options := make([]BootstrapOption, 0, len(rows))
	for _, row := range rows {
		name := row.Label
		if name == "" {
			name = row.Model
		}
		description := row.Description
		if description == "" {
			// The field is required by the client's schema, and a response
			// that fails validation would break more than it fixes.
			description = "Added by claude-code-model-proxy."
		}
		options = append(options, BootstrapOption{
			Model:       row.Model,
			Name:        name,
			Description: description,
		})
	}
	return options
}

// Lookup returns the registry entry for an exact id.
func (r *Registry) Lookup(id string) (Model, bool) {
	model, ok := r.byID[strings.ToLower(strings.TrimSpace(id))]
	return model, ok
}

// SelectorRow is one entry of a served model catalog's
// model_selector_config[].models array, the list newer Claude Code versions
// build their /model picker from. The field names are Claude Code's.
type SelectorRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Section places the row in the picker: "main" rows are listed first.
	Section string `json:"section"`
	// MinClaudeCodeVersion makes the row selectable. Without it Claude Code
	// offers a row only for a model it already knows, which an added model
	// never is.
	MinClaudeCodeVersion string `json:"min_claude_code_version"`
	// BehavesAs names the Claude model whose client-side handling applies.
	BehavesAs     string            `json:"behaves_as,omitempty"`
	ContextWindow int               `json:"context_window,omitempty"`
	Runtime       SelectorRuntime   `json:"runtime"`
	Thinking      *SelectorThinking `json:"thinking,omitempty"`
}

// SelectorRuntime is a catalog row's runtime block.
type SelectorRuntime struct {
	MaxInputTokens  int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	EffortLevels    []string `json:"effort_levels,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
}

// SelectorThinking is a catalog row's effort control.
type SelectorThinking struct {
	Type          string           `json:"type"`
	EffortOptions []SelectorOption `json:"effort_options"`
}

// SelectorOption is one choice of a catalog row's effort control.
type SelectorOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// selectorMinVersion is old enough for every Claude Code that serves a
// catalog, and only there to make the row selectable.
const selectorMinVersion = "2.1.0"

// effortNames are the labels Claude Code's own catalog gives effort levels.
var effortNames = map[string]string{
	"none": "None", "minimal": "Minimal", "low": "Low", "medium": "Medium",
	"high": "High", "xhigh": "Extra", "max": "Max",
}

// SelectorRows renders one served-catalog row per advertised model.
func (r *Registry) SelectorRows() []SelectorRow {
	rows := make([]SelectorRow, 0, len(r.ordered))
	for _, picker := range r.PickerOptions() {
		resolution, ok := r.Resolve(picker.Model)
		if !ok {
			continue
		}
		model := resolution.Model
		row := SelectorRow{
			ID:                   picker.Model,
			Name:                 picker.Label,
			Description:          picker.Description,
			Section:              "main",
			MinClaudeCodeVersion: selectorMinVersion,
			BehavesAs:            model.BehavesAs,
			ContextWindow:        model.ContextLength,
			Runtime: SelectorRuntime{
				MaxInputTokens:  model.ContextLength,
				MaxOutputTokens: model.MaxOutput,
			},
		}
		// An effort variant pins its effort, so it offers no choice.
		if resolution.Effort == "" && len(model.ReasoningLevel) > 0 {
			row.Runtime.EffortLevels = append([]string(nil), model.ReasoningLevel...)
			row.Runtime.DefaultEffort = model.DefaultEffort
			thinking := &SelectorThinking{Type: "effort"}
			for _, level := range model.ReasoningLevel {
				name := effortNames[level]
				if name == "" {
					name = level
				}
				thinking.EffortOptions = append(thinking.EffortOptions, SelectorOption{ID: level, Name: name})
			}
			row.Thinking = thinking
		}
		rows = append(rows, row)
	}
	return rows
}
