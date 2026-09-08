package codex

import "testing"

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
