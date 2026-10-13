package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
)

// Namespace is accepted in front of an OpenRouter model id when no prefix is
// configured.
const Namespace = "openrouter/"

// DefaultBehavesAs is the Claude model whose client-side handling applies to
// an OpenRouter model whose configuration names none.
const DefaultBehavesAs = "claude-sonnet-5"

// reasoningLevels are the efforts offered for a model that supports
// reasoning. OpenRouter maps each onto the model's own controls.
var reasoningLevels = []string{"low", "medium", "high", "xhigh"}

// Info is what OpenRouter's model list reports about one model.
type Info struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters"`
}

func (i Info) supports(parameter string) bool {
	for _, candidate := range i.SupportedParameters {
		if candidate == parameter {
			return true
		}
	}
	return false
}

// FetchModels reads OpenRouter's public model list, keyed by model id.
func FetchModels(ctx context.Context, client *http.Client, baseURL string) (map[string]Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter: list models returned %d", resp.StatusCode)
	}

	var list struct {
		Data []Info `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("openrouter: decode model list: %w", err)
	}
	byID := make(map[string]Info, len(list.Data))
	for _, info := range list.Data {
		byID[info.ID] = info
	}
	return byID, nil
}

// Models builds the catalog entries for the configured models. info may be
// nil when the model list could not be fetched; the entries are then built
// from the configuration alone. The returned warnings name configured models
// that will not work well with Claude Code.
func Models(configured []config.OpenRouterModel, info map[string]Info) ([]catalog.Model, []string) {
	models := make([]catalog.Model, 0, len(configured))
	var warnings []string
	for _, spec := range configured {
		model := catalog.Model{
			ID:            spec.ID,
			DisplayName:   spec.DisplayName,
			Description:   spec.Description,
			BehavesAs:     spec.BehavesAs,
			DefaultEffort: spec.DefaultEffort,
			// Without the model list, assume the common case: a reasoning
			// model with a generous window.
			ContextLength:  200000,
			ReasoningLevel: reasoningLevels,
		}

		if details, ok := info[spec.ID]; ok {
			if model.DisplayName == "" {
				model.DisplayName = details.Name
			}
			if details.ContextLength > 0 {
				model.ContextLength = details.ContextLength
			}
			model.MaxOutput = details.TopProvider.MaxCompletionTokens
			for _, modality := range details.Architecture.InputModalities {
				if modality == "image" {
					model.SupportsImages = true
				}
			}
			if !details.supports("reasoning") {
				model.ReasoningLevel = nil
				model.DefaultEffort = ""
			}
			if !details.supports("tools") {
				warnings = append(warnings, fmt.Sprintf("%s does not support tool calls, which Claude Code depends on", spec.ID))
			}
		} else if info != nil {
			warnings = append(warnings, fmt.Sprintf("%s is not in OpenRouter's model list", spec.ID))
		}

		if model.DisplayName == "" {
			model.DisplayName = spec.ID
		}
		model.DisplayName += " (OpenRouter)"
		if model.Description == "" {
			model.Description = "OpenRouter: " + spec.ID + "."
		}
		if model.BehavesAs == "" {
			model.BehavesAs = DefaultBehavesAs
		}
		if model.ReasoningLevel != nil && model.DefaultEffort == "" {
			model.DefaultEffort = "medium"
		}
		models = append(models, model)
	}
	return models, warnings
}

// NewRegistry builds the registry over the configured models.
func NewRegistry(models []catalog.Model, prefix string) *catalog.Registry {
	return catalog.New(models, nil, prefix, Namespace, false)
}
