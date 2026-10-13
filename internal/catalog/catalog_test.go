package catalog

import "testing"

func testModels() []Model {
	return []Model{
		{ID: "z-ai/glm-5.3-flash", DisplayName: "GLM", ReasoningLevel: []string{"low", "medium", "high"}, DefaultEffort: "medium", BehavesAs: "claude-sonnet-5"},
		{ID: "z-ai/glm-5.3-flash:batch", DisplayName: "GLM batch", ReasoningLevel: []string{"low", "medium", "high"}, DefaultEffort: "medium", BehavesAs: "claude-sonnet-5"},
	}
}

func TestResolveIdsThatContainAColon(t *testing.T) {
	registry := New(testModels(), nil, "", "openrouter/", false)

	resolution, ok := registry.Resolve("z-ai/glm-5.3-flash:batch")
	if !ok || resolution.Model.ID != "z-ai/glm-5.3-flash:batch" || resolution.Effort != "" {
		t.Fatalf("the colon is part of the id: %+v, %v", resolution, ok)
	}

	// The effort separator still works, and is taken from the last colon.
	resolution, ok = registry.Resolve("z-ai/glm-5.3-flash:batch:high")
	if !ok || resolution.Model.ID != "z-ai/glm-5.3-flash:batch" || resolution.Effort != "high" {
		t.Fatalf("effort after an id with a colon: %+v, %v", resolution, ok)
	}
	resolution, ok = registry.Resolve("z-ai/glm-5.3-flash:low")
	if !ok || resolution.Model.ID != "z-ai/glm-5.3-flash" || resolution.Effort != "low" {
		t.Fatalf("effort separator: %+v, %v", resolution, ok)
	}
}

func TestResolveNamespace(t *testing.T) {
	registry := New(testModels(), nil, "", "openrouter/", false)
	if resolution, ok := registry.Resolve("openrouter/Z-AI/GLM-5.3-Flash"); !ok || resolution.Model.ID != "z-ai/glm-5.3-flash" {
		t.Fatalf("the namespace must be accepted when no prefix is set: %+v, %v", resolution, ok)
	}
	if resolution, ok := registry.Resolve("openrouter/z-ai/glm-5.3-flash"); !ok || resolution.Requested != "openrouter/z-ai/glm-5.3-flash" {
		t.Fatalf("Requested must keep the name as sent: %+v", resolution)
	}

	prefixed := New(testModels(), nil, "or", "openrouter/", false)
	if _, ok := prefixed.Resolve("or/z-ai/glm-5.3-flash"); !ok {
		t.Fatal("the configured prefix must be accepted")
	}
	if ids := prefixed.AdvertisedIDs(); len(ids) != 2 || ids[0] != "or/z-ai/glm-5.3-flash" {
		t.Fatalf("advertised ids = %v", ids)
	}
	if _, ok := prefixed.Resolve("claude-opus-5"); ok {
		t.Fatal("a Claude model must not resolve")
	}
}
