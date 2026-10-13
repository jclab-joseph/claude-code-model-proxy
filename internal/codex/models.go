package codex

import "github.com/jclab-joseph/claude-code-model-proxy/internal/catalog"

// Catalog is the set of Codex models reachable with a ChatGPT subscription.
// The ids are the slugs the ChatGPT backend expects verbatim in the request
// body; availability still depends on the signed-in account's plan, so a model
// listed here can legitimately be rejected upstream.
var Catalog = []catalog.Model{
	{
		ID:             "gpt-5.6-sol",
		DisplayName:    "GPT-5.6 Sol (Codex)",
		Description:    "Frontier coding and research model. Capable at low effort; turn it up for harder work.",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
		BehavesAs:      "claude-opus-5",
	},
	{
		ID:             "gpt-6-astra",
		DisplayName:    "GPT-6 Astra (Codex)",
		Description:    "OpenAI's most intelligent model: computer use, browsing and software engineering.",
		ContextLength:  272000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
		BehavesAs:      "claude-opus-5",
	},
	{
		ID:             "gpt-5.6-terra",
		DisplayName:    "GPT-5.6 Terra (Codex)",
		Description:    "Frontier model for complex, long-running work.",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
		BehavesAs:      "claude-opus-5",
	},
	{
		ID:             "gpt-5.6-luna",
		DisplayName:    "GPT-5.6 Luna (Codex)",
		Description:    "Frontier model for complex, long-running work.",
		ContextLength:  372000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:  "medium",
		SupportsImages: true,
		BehavesAs:      "claude-opus-5",
	},
	{
		ID:             "gpt-5.5",
		DisplayName:    "GPT-5.5 (Codex)",
		Description:    "Frontier model for complex coding, research and real-world work.",
		ContextLength:  272000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh"},
		DefaultEffort:  "medium",
		SupportsImages: true,
		BehavesAs:      "claude-sonnet-5",
	},
	{
		ID:             "gpt-5.3-codex-spark",
		DisplayName:    "GPT-5.3 Codex Spark",
		Description:    "Ultra-fast coding model for quick edits.",
		ContextLength:  128000,
		MaxOutput:      128000,
		ReasoningLevel: []string{"low", "medium", "high", "xhigh"},
		DefaultEffort:  "medium",
		SupportsImages: false,
		BehavesAs:      "claude-haiku-4-5",
	},
}

// NewRegistry builds a registry over the Codex catalog. allow, when non-empty,
// restricts it to those ids; prefix and variants are as in catalog.New, and
// codex/ is accepted in front of an id when no prefix is configured.
func NewRegistry(allow []string, prefix string, variants bool) *catalog.Registry {
	return catalog.New(Catalog, allow, prefix, "codex/", variants)
}
