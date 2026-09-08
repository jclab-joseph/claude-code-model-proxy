package codex

import (
	"strings"
	"testing"
)

func TestResolveBareModel(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	resolution, ok := registry.Resolve("gpt-5.5")
	if !ok {
		t.Fatal("gpt-5.5 was not recognised as a Codex model")
	}
	if resolution.Model.ID != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", resolution.Model.ID)
	}
	if resolution.Effort != "" {
		t.Fatalf("effort = %q, want empty for a bare model name", resolution.Effort)
	}
	if resolution.Requested != "gpt-5.5" {
		t.Fatalf("requested = %q, want the name as sent", resolution.Requested)
	}
}

func TestResolveEffortSpellings(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	for _, name := range []string{"gpt-5.5-high", "gpt-5.5:high", "codex/gpt-5.5-high", "GPT-5.5-High"} {
		resolution, ok := registry.Resolve(name)
		if !ok {
			t.Fatalf("%q was not recognised", name)
		}
		if resolution.Model.ID != "gpt-5.5" || resolution.Effort != "high" {
			t.Fatalf("%q resolved to %q effort %q, want gpt-5.5/high", name, resolution.Model.ID, resolution.Effort)
		}
	}
}

func TestResolvePrefersLongerEffortSuffix(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	resolution, ok := registry.Resolve("gpt-5.5-xhigh")
	if !ok {
		t.Fatal("gpt-5.5-xhigh was not recognised")
	}
	if resolution.Effort != "xhigh" {
		t.Fatalf("effort = %q, want xhigh (not a partial match on high)", resolution.Effort)
	}
}

func TestResolveClampsUnsupportedEffort(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	// gpt-5.5 stops at xhigh, so "max" must clamp rather than be forwarded.
	resolution, ok := registry.Resolve("gpt-5.5-max")
	if !ok {
		t.Fatal("gpt-5.5-max was not recognised")
	}
	if resolution.Effort != "xhigh" {
		t.Fatalf("effort = %q, want it clamped to xhigh", resolution.Effort)
	}

	// gpt-6-astra supports max, so it passes through.
	resolution, ok = registry.Resolve("gpt-6-astra-max")
	if !ok {
		t.Fatal("gpt-6-astra-max was not recognised")
	}
	if resolution.Effort != "max" {
		t.Fatalf("effort = %q, want max", resolution.Effort)
	}
}

func TestResolveIgnoresClaudeModels(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	for _, name := range []string{"claude-opus-5", "claude-sonnet-5", "", "gpt-4o"} {
		if _, ok := registry.Resolve(name); ok {
			t.Fatalf("%q must not be treated as a Codex model", name)
		}
	}
}

func TestRegistryFilterAndPrefix(t *testing.T) {
	registry := NewRegistry([]string{"gpt-5.5"}, "codex", false)

	if _, ok := registry.Resolve("gpt-6-astra"); ok {
		t.Fatal("a filtered-out model must not resolve")
	}

	resolution, ok := registry.Resolve("codex/gpt-5.5")
	if !ok {
		t.Fatal("codex/gpt-5.5 was not recognised with a configured prefix")
	}
	if resolution.Model.ID != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", resolution.Model.ID)
	}

	ids := registry.AdvertisedIDs()
	if len(ids) != 1 || ids[0] != "codex/gpt-5.5" {
		t.Fatalf("advertised ids = %v, want exactly [codex/gpt-5.5] with variants disabled", ids)
	}
}

func TestAdvertisedIDsIncludeEffortVariants(t *testing.T) {
	registry := NewRegistry([]string{"gpt-5.5"}, "", true)

	ids := registry.AdvertisedIDs()
	want := []string{"gpt-5.5", "gpt-5.5-low", "gpt-5.5-high", "gpt-5.5-xhigh"}
	if len(ids) != len(want) {
		t.Fatalf("advertised ids = %v, want %v", ids, want)
	}
	for _, id := range want {
		found := false
		for _, got := range ids {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("advertised ids %v is missing %q", ids, id)
		}
	}
	// The default effort is already covered by the bare id.
	for _, id := range ids {
		if id == "gpt-5.5-medium" {
			t.Fatal("the default effort must not be advertised as a separate variant")
		}
	}
}

func TestEveryCatalogEntryCanBeOfferedToClaudeCode(t *testing.T) {
	for _, model := range Catalog {
		// Without behavesAs, Claude Code does not offer a picker row for a
		// model its own catalog does not know, so the field is mandatory.
		if model.BehavesAs == "" {
			t.Errorf("%s has no BehavesAs; its picker row would not be offered", model.ID)
		}
		if !strings.HasPrefix(model.BehavesAs, "claude-") {
			t.Errorf("%s behaves as %q, which is not a Claude model id", model.ID, model.BehavesAs)
		}
		// The bootstrap schema requires a non-empty description.
		if strings.TrimSpace(model.Description) == "" {
			t.Errorf("%s has no Description", model.ID)
		}
		if strings.TrimSpace(model.DisplayName) == "" {
			t.Errorf("%s has no DisplayName", model.ID)
		}
	}
}

func TestPickerOptionsCoverEveryAdvertisedModel(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	ids := registry.AdvertisedIDs()
	rows := registry.PickerOptions()
	if len(rows) != len(ids) {
		t.Fatalf("%d picker rows for %d advertised models", len(rows), len(ids))
	}

	for i, row := range rows {
		if row.Model != ids[i] {
			t.Fatalf("row %d is %q, want %q: the order must match", i, row.Model, ids[i])
		}
		if row.Label == "" || row.Description == "" || row.BehavesAs == "" {
			t.Fatalf("row %q has an empty field: %+v", row.Model, row)
		}
	}
}

func TestPickerOptionLabelsAreDistinct(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	seen := map[string]string{}
	for _, row := range registry.PickerOptions() {
		// Two rows with the same title are indistinguishable in the picker.
		if other, clash := seen[row.Label]; clash {
			t.Fatalf("%q and %q share the label %q", other, row.Model, row.Label)
		}
		seen[row.Label] = row.Model
	}
}

func TestPickerOptionLabelCarriesTheEffort(t *testing.T) {
	registry := NewRegistry([]string{"gpt-5.5"}, "", true)

	for _, row := range registry.PickerOptions() {
		resolution, ok := registry.Resolve(row.Model)
		if !ok {
			t.Fatalf("%q does not resolve", row.Model)
		}
		if resolution.Effort == "" {
			continue
		}
		// An effort variant is a separate row, so the label has to say which.
		if !strings.Contains(row.Label, resolution.Effort) {
			t.Fatalf("row %q has label %q, want it to mention the %s effort", row.Model, row.Label, resolution.Effort)
		}
	}
}

func TestBootstrapOptionsMatchTheClientSchema(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	options := registry.BootstrapOptions()
	if len(options) != len(registry.AdvertisedIDs()) {
		t.Fatalf("%d bootstrap options for %d models", len(options), len(registry.AdvertisedIDs()))
	}
	for _, option := range options {
		if option.Model == "" || option.Name == "" || option.Description == "" {
			t.Fatalf("option has an empty required field: %+v", option)
		}
		if option.DisabledReason != nil {
			t.Fatalf("%s is marked disabled: %+v", option.Model, option)
		}
	}
}

func TestResolveStripsTheContextWindowSuffix(t *testing.T) {
	registry := NewRegistry(nil, "", true)

	// Claude Code appends [1m]/[2m] to ask for a larger context window; the
	// suffix is a client-side hint and no upstream model id carries it.
	for _, name := range []string{"gpt-5.5[1m]", "gpt-5.5-high[1m]", "gpt-5.6-sol[2m]"} {
		resolution, ok := registry.Resolve(name)
		if !ok {
			t.Fatalf("%q was not recognised", name)
		}
		if strings.Contains(resolution.Model.ID, "[") {
			t.Fatalf("%q resolved to model id %q, want the suffix stripped", name, resolution.Model.ID)
		}
		if resolution.Requested != name {
			t.Fatalf("requested = %q, want the name as sent", resolution.Requested)
		}
	}

	if resolution, _ := registry.Resolve("gpt-5.5-high[1m]"); resolution.Effort != "high" {
		t.Fatalf("effort = %q, want high", resolution.Effort)
	}
	// A bracket that is not the window hint must not be silently accepted.
	if _, ok := registry.Resolve("gpt-5.5[9m]"); ok {
		t.Fatal("gpt-5.5[9m] must not resolve")
	}
}
