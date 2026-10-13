package openrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/tidwall/gjson"
)

func TestDataCollectionIsAlwaysDeny(t *testing.T) {
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		sent = string(data)
	}))
	defer server.Close()

	// Even a preference that tries to allow collection is overridden.
	client := NewClient(server.URL, "key", map[string]any{"data_collection": "allow", "order": []string{"z-ai"}}, nil)
	resp, err := client.Responses(context.Background(), []byte(`{"model":"m","input":[],"reasoning":{"effort":"high"}}`), "")
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	_ = resp.Body.Close()

	if got := gjson.Get(sent, "provider.data_collection").String(); got != "deny" {
		t.Fatalf("data_collection = %q, want deny: %s", got, sent)
	}
	if got := gjson.Get(sent, "provider.order.0").String(); got != "z-ai" {
		t.Fatalf("the other preferences must be kept: %s", sent)
	}
	if got := gjson.Get(sent, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning was changed: %s", sent)
	}
}

func TestEmptyEffortDropsReasoning(t *testing.T) {
	client := NewClient("http://unused", "key", nil, nil)
	body, err := client.prepare([]byte(`{"model":"m","reasoning":{"effort":"","summary":"auto"}}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if gjson.GetBytes(body, "reasoning").Exists() {
		t.Fatalf("an empty effort must drop reasoning: %s", body)
	}
}

func TestMissingKeyAndUpstreamErrors(t *testing.T) {
	if _, err := NewClient("http://unused", "", nil, nil).Responses(context.Background(), []byte(`{}`), ""); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":402,"message":"Insufficient credits"}}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "key", nil, nil).Responses(context.Background(), []byte(`{}`), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusPaymentRequired || !strings.Contains(string(apiErr.ResponseBody()), "credits") {
		t.Fatalf("err = %v", err)
	}
}

func TestModelsFromTheModelList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"z-ai/glm-5.3-flash","name":"Z.ai: GLM 5.3 Flash","context_length":1048576,
			 "architecture":{"input_modalities":["text","image"]},"top_provider":{"max_completion_tokens":131072},
			 "supported_parameters":["reasoning","tools","tool_choice"]},
			{"id":"plain/no-tools","name":"Plain","context_length":8192,"supported_parameters":["max_tokens"]}
		]}`))
	}))
	defer server.Close()

	info, err := FetchModels(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	models, warnings := Models([]config.OpenRouterModel{
		{ID: "z-ai/glm-5.3-flash", BehavesAs: "claude-opus-5"},
		{ID: "plain/no-tools"},
		{ID: "gone/model"},
	}, info)

	glm := models[0]
	if glm.DisplayName != "Z.ai: GLM 5.3 Flash (OpenRouter)" || glm.ContextLength != 1048576 || glm.MaxOutput != 131072 || !glm.SupportsImages {
		t.Fatalf("glm = %+v", glm)
	}
	if glm.BehavesAs != "claude-opus-5" || glm.DefaultEffort != "medium" || len(glm.ReasoningLevel) == 0 {
		t.Fatalf("glm = %+v", glm)
	}
	plain := models[1]
	if plain.ReasoningLevel != nil || plain.DefaultEffort != "" || plain.BehavesAs != DefaultBehavesAs {
		t.Fatalf("a model without reasoning support must ask for no effort: %+v", plain)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "tool calls") || !strings.Contains(warnings[1], "gone/model") {
		t.Fatalf("warnings = %v", warnings)
	}

	// Without the model list the configured models are still offered.
	offline, _ := Models([]config.OpenRouterModel{{ID: "z-ai/glm-5.3-flash"}}, nil)
	if offline[0].ContextLength == 0 || offline[0].DefaultEffort != "medium" {
		t.Fatalf("offline model = %+v", offline[0])
	}
}
