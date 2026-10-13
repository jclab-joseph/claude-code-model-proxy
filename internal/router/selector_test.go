package router

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const selectorPathForTest = "/api/organizations/b2c2bb4c-0000-0000-0000-000000000000/model_selector/cc"

// realSelectorBody is the shape the production endpoint returns, trimmed to
// one model row and one state entry.
const realSelectorBody = `{
  "model_selector_config": [
    {"id":"cc","models":[
      {"id":"claude-opus-5-5","name":"Opus 5.5","short_name":"Opus","description":"For complex work","section":"main",
       "thinking":{"type":"effort","effort_options":[{"id":"low","name":"Low"},{"id":"high","name":"High"}]},
       "quick_select":true,"min_claude_code_version":"2.1.280"}
    ],"settings_vocabulary":{}}
  ],
  "model_selector_state": [
    {"id":"cc","model":"claude-opus-4-8","selection_source":"user_setting","thinking":{"type":"effort","effort":"high"}}
  ],
  "organization_uuid": "b2c2bb4c-0000-0000-0000-000000000000"
}`

func TestModelSelectorIsPassedThroughWhenTheExperimentIsOff(t *testing.T) {
	h := bootstrapHarness(t, realSelectorBody, http.StatusOK, nil)
	h.router.cfg.ExperimentalModifyBootstrapModels = false

	rec := h.do(t, http.MethodGet, selectorPathForTest, "")
	if rec.Body.String() != realSelectorBody {
		t.Fatalf("the response must be forwarded untouched:\n%s", rec.Body.String())
	}
}

func TestModelSelectorAddsModelsWhenEnabled(t *testing.T) {
	h := bootstrapHarness(t, realSelectorBody, http.StatusOK, nil)

	rec := h.do(t, http.MethodGet, selectorPathForTest, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := gjson.Parse(rec.Body.String())
	models := body.Get("model_selector_config.0.models").Array()
	if models[0].Get("id").String() != "claude-opus-5-5" {
		t.Fatalf("the account's own rows must stay first: %s", body.Raw)
	}

	glm := body.Get(`model_selector_config.0.models.#(id=="z-ai/glm-5.3-flash")`)
	if !glm.Exists() {
		t.Fatalf("the OpenRouter model is missing: %s", body.Get("model_selector_config.0.models.#.id").Raw)
	}
	// Without a minimum version Claude Code would not offer a model it does
	// not know; with no behaves_as it would not know how to treat it.
	if glm.Get("min_claude_code_version").String() == "" || glm.Get("behaves_as").String() != "claude-sonnet-5" {
		t.Fatalf("row = %s", glm.Raw)
	}
	if glm.Get("section").String() != "main" || glm.Get("name").String() == "" || glm.Get("description").String() == "" {
		t.Fatalf("row = %s", glm.Raw)
	}
	if glm.Get("runtime.effort_levels.#").Int() == 0 || glm.Get("thinking.effort_options.#").Int() == 0 {
		t.Fatalf("a reasoning model must offer its effort levels: %s", glm.Raw)
	}
	if !body.Get(`model_selector_config.0.models.#(id=="gpt-5.5")`).Exists() {
		t.Fatal("the Codex models are missing")
	}
	// Everything else is the account's and stays as it was.
	if body.Get("model_selector_state.0.model").String() != "claude-opus-4-8" || !body.Get("model_selector_config.0.settings_vocabulary").Exists() {
		t.Fatalf("unrelated fields changed: %s", body.Raw)
	}
}

func TestModelSelectorNeverDuplicatesARow(t *testing.T) {
	withGLM := strings.Replace(realSelectorBody, `"models":[`, `"models":[{"id":"z-ai/glm-5.3-flash","name":"Account's own"},`, 1)
	h := bootstrapHarness(t, withGLM, http.StatusOK, nil)

	body := gjson.Parse(h.do(t, http.MethodGet, selectorPathForTest, "").Body.String())
	matches := body.Get(`model_selector_config.0.models.#(id=="z-ai/glm-5.3-flash")#`).Array()
	if len(matches) != 1 || matches[0].Get("name").String() != "Account's own" {
		t.Fatalf("rows = %v", matches)
	}
}

func TestModelSelectorPassesThroughWhatItCannotParse(t *testing.T) {
	cases := map[string]struct {
		body   string
		status int
		header map[string]string
	}{
		"failed":      {realSelectorBody, http.StatusInternalServerError, nil},
		"not JSON":    {"<html>", http.StatusOK, nil},
		"encoded":     {"\x83\x87\x01\x00 not json", http.StatusOK, map[string]string{"Content-Encoding": "br"}},
		"no config":   {`{"model_selector_state":[]}`, http.StatusOK, nil},
		"no surface":  {`{"model_selector_config":[{"id":"claude_ai","models":[]}]}`, http.StatusOK, nil},
		"bad models":  {`{"model_selector_config":[{"id":"cc","models":{"x":1}}]}`, http.StatusOK, nil},
		"bad configs": {`{"model_selector_config":{"id":"cc"}}`, http.StatusOK, nil},
	}
	for name, c := range cases {
		h := bootstrapHarness(t, c.body, c.status, c.header)
		rec := h.do(t, http.MethodGet, selectorPathForTest, "")
		if rec.Code != c.status || rec.Body.String() != c.body {
			t.Fatalf("%s: got %d %q, want the response forwarded as is", name, rec.Code, rec.Body.String())
		}
	}
}

func TestModelSelectorFillsAMissingModelList(t *testing.T) {
	h := bootstrapHarness(t, `{"model_selector_config":[{"id":"cc"}]}`, http.StatusOK, nil)
	body := gjson.Parse(h.do(t, http.MethodGet, selectorPathForTest, "").Body.String())
	if body.Get("model_selector_config.0.models.#").Int() == 0 {
		t.Fatalf("models = %s", body.Raw)
	}
}

func TestOnlyClaudeCodesSurfaceIsRewritten(t *testing.T) {
	for _, path := range []string{
		"/api/organizations/org/model_selector/claude_ai",
		"/api/organizations/org/model_selector/cc/extra",
		"/api/model_selector/cc",
	} {
		if selectorPath.MatchString(path) {
			t.Fatalf("%s must not be rewritten", path)
		}
	}
	if !selectorPath.MatchString("/api/oauth/organizations/org/model_selector/cc") {
		t.Fatal("the OAuth spelling of the path must be rewritten too")
	}
}
