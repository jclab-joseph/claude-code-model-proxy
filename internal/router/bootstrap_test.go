package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// realBootstrapBody is the shape the production endpoint returns, trimmed to
// the fields that matter here. The account's own extra model is kept so the
// merge can be checked against a non-empty list.
const realBootstrapBody = `{
  "client_data": {"cedar_lagoon": {"claude-fable": true}},
  "additional_model_options": [
    {"model":"claude-fable-5-1[1m]","name":"Fable","description":"Fable 5.1 - most capable","disabled_reason":null}
  ],
  "additional_model_costs": null,
  "oauth_account": {"account_uuid":"uuid","organization_name":"Org"},
  "model_access": null,
  "cwk_cfg_key": "pewter"
}`

func bootstrapHarness(t *testing.T, body string, status int, header map[string]string) *harness {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		for name, value := range header {
			w.Header().Set(name, value)
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}, nil, true)
	h.router.cfg.ExperimentalModifyBootstrapModels = true
	return h
}

func TestBootstrapIsPassedThroughWhenTheExperimentIsOff(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realBootstrapBody))
	}, nil, true)
	// The experiment defaults to off.
	if h.router.cfg.ExperimentalModifyBootstrapModels {
		t.Fatal("rewriting the startup response must be opt-in")
	}

	rec := h.do(t, http.MethodGet, bootstrapPath+"?entrypoint=cli&model=claude-sonnet-5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != realBootstrapBody {
		t.Fatal("the startup response was altered with the experiment off")
	}
}

func TestBootstrapAddsModelsWhenEnabled(t *testing.T) {
	h := bootstrapHarness(t, realBootstrapBody, http.StatusOK, nil)

	rec := h.do(t, http.MethodGet, bootstrapPath+"?entrypoint=cli&model=claude-sonnet-5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.anthropic.path != bootstrapPath {
		t.Fatalf("upstream path = %q", h.anthropic.path)
	}
	if h.anthropic.query != "entrypoint=cli&model=claude-sonnet-5" {
		t.Fatalf("upstream query = %q, want the client's own", h.anthropic.query)
	}

	body := gjson.Parse(rec.Body.String())
	options := body.Get("additional_model_options").Array()
	if len(options) < 2 {
		t.Fatalf("additional_model_options = %s", body.Get("additional_model_options").Raw)
	}

	// The account's own entry must come first and be untouched.
	if got := options[0].Get("model").String(); got != "claude-fable-5-1[1m]" {
		t.Fatalf("first option = %q, want the account's own entry preserved", got)
	}

	ids := map[string]gjson.Result{}
	for _, option := range options[1:] {
		ids[option.Get("model").String()] = option
	}
	added, ok := ids["gpt-5.5"]
	if !ok {
		t.Fatalf("gpt-5.5 was not added: %s", body.Get("additional_model_options").Raw)
	}
	// Every field of the client's schema must be present: a response that
	// fails its validation would break more than the picker.
	for _, field := range []string{"model", "name", "description", "disabled_reason"} {
		if !added.Get(field).Exists() {
			t.Fatalf("added option is missing %q: %s", field, added.Raw)
		}
	}
	if added.Get("name").String() == "" || added.Get("description").String() == "" {
		t.Fatalf("added option has an empty required field: %s", added.Raw)
	}
	if added.Get("disabled_reason").Type != gjson.Null {
		t.Fatalf("disabled_reason = %s, want null", added.Get("disabled_reason").Raw)
	}

	// Everything else in the response has to survive.
	if got := body.Get("cwk_cfg_key").String(); got != "pewter" {
		t.Fatalf("cwk_cfg_key = %q, want it preserved", got)
	}
	if got := body.Get("oauth_account.organization_name").String(); got != "Org" {
		t.Fatalf("oauth_account was altered: %s", body.Get("oauth_account").Raw)
	}
	if !body.Get("model_access").Exists() {
		t.Fatal("model_access was dropped")
	}
}

func TestBootstrapHandlesANullOptionList(t *testing.T) {
	h := bootstrapHarness(t, `{"additional_model_options":null,"cwk_cfg_key":"x"}`, http.StatusOK, nil)

	rec := h.do(t, http.MethodGet, bootstrapPath, "")
	options := gjson.Get(rec.Body.String(), "additional_model_options")
	if !options.IsArray() || len(options.Array()) == 0 {
		t.Fatalf("additional_model_options = %s, want the added models", options.Raw)
	}
}

func TestBootstrapHandlesAMissingOptionList(t *testing.T) {
	h := bootstrapHarness(t, `{"cwk_cfg_key":"x"}`, http.StatusOK, nil)

	rec := h.do(t, http.MethodGet, bootstrapPath, "")
	options := gjson.Get(rec.Body.String(), "additional_model_options")
	if !options.IsArray() || len(options.Array()) == 0 {
		t.Fatalf("additional_model_options = %s, want the added models", options.Raw)
	}
}

func TestBootstrapNeverDuplicatesAnExistingModel(t *testing.T) {
	// The account already offers gpt-5.5; the proxy must not add it twice.
	body := `{"additional_model_options":[{"model":"gpt-5.5","name":"Theirs","description":"d","disabled_reason":null}]}`
	h := bootstrapHarness(t, body, http.StatusOK, nil)

	rec := h.do(t, http.MethodGet, bootstrapPath, "")
	count := 0
	names := []string{}
	gjson.Get(rec.Body.String(), "additional_model_options").ForEach(func(_, option gjson.Result) bool {
		if option.Get("model").String() == "gpt-5.5" {
			count++
			names = append(names, option.Get("name").String())
		}
		return true
	})
	if count != 1 {
		t.Fatalf("gpt-5.5 appears %d times (%v), want once", count, names)
	}
	if names[0] != "Theirs" {
		t.Fatalf("name = %q, want the account's own entry to win", names[0])
	}
}

func TestBootstrapPassesThroughWhatItCannotParse(t *testing.T) {
	cases := map[string]struct {
		body   string
		status int
		header map[string]string
	}{
		"still compressed": {
			body:   "\x83\x87\x01\x00 not json",
			status: http.StatusOK,
			header: map[string]string{"Content-Encoding": "br"},
		},
		"not JSON": {
			body:   "<html>maintenance</html>",
			status: http.StatusOK,
			header: map[string]string{"Content-Type": "text/html"},
		},
		"unexpected option shape": {
			body:   `{"additional_model_options":{"model":"x"}}`,
			status: http.StatusOK,
		},
		"upstream error": {
			body:   `{"type":"error","error":{"message":"nope"}}`,
			status: http.StatusUnauthorized,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := bootstrapHarness(t, tc.body, tc.status, tc.header)

			rec := h.do(t, http.MethodGet, bootstrapPath, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want the upstream %d", rec.Code, tc.status)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("body was altered:\n got %q\nwant %q", rec.Body.String(), tc.body)
			}
		})
	}
}

func TestBootstrapDropsTheStaleContentEncoding(t *testing.T) {
	// The upstream body arrived decoded because the proxy asked for it that
	// way, so a Content-Encoding copied from the response would be a lie.
	h := bootstrapHarness(t, realBootstrapBody, http.StatusOK, map[string]string{"Content-Encoding": "identity"})

	rec := h.do(t, http.MethodGet, bootstrapPath, "")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want it removed after rewriting", got)
	}
	if !gjson.Valid(rec.Body.String()) {
		t.Fatalf("body is not valid JSON: %s", rec.Body.String())
	}
}

func TestBootstrapStripsTheClientAcceptEncoding(t *testing.T) {
	h := bootstrapHarness(t, realBootstrapBody, http.StatusOK, nil)

	req := httptest.NewRequest(http.MethodGet, bootstrapPath, nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Asking upstream for a compressed body would make it unparseable.
	if got := h.anthropic.header.Get("Accept-Encoding"); strings.Contains(got, "br") {
		t.Fatalf("upstream Accept-Encoding = %q, want br not requested", got)
	}
}
