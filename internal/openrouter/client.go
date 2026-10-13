// Package openrouter reaches OpenRouter's Responses API with an API key, and
// builds the catalog of OpenRouter models this proxy adds to Claude Code.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ErrNoAPIKey is returned when no API key is configured.
var ErrNoAPIKey = errors.New("openrouter: no API key; set the variable named by openrouter.api_key_env (OPENROUTER_API_KEY by default)")

// Client calls OpenRouter's Responses endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	// Provider holds the routing preferences sent with every request. Its
	// data_collection is overridden to deny regardless of what it says.
	Provider map[string]any
	HTTP     *http.Client
}

// NewClient wires a client with a streaming-friendly HTTP client. There is no
// overall request timeout because a response can stream for minutes; the
// caller's context bounds the call instead.
func NewClient(baseURL, apiKey string, provider map[string]any, transport http.RoundTripper) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		BaseURL:  strings.TrimSuffix(baseURL, "/"),
		APIKey:   apiKey,
		Provider: provider,
		HTTP:     &http.Client{Transport: transport},
	}
}

// Ready reports whether a key is configured.
func (c *Client) Ready() bool { return c.APIKey != "" }

// APIError describes a non-2xx response from OpenRouter.
type APIError struct {
	StatusCode int
	Body       []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: returned %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

// HTTPStatus and ResponseBody let callers handle the errors of every
// provider alike.
func (e *APIError) HTTPStatus() int      { return e.StatusCode }
func (e *APIError) ResponseBody() []byte { return e.Body }

// Responses posts a translated Responses API request and returns the
// streaming response. The caller owns the body and must close it.
//
// sessionID is unused: OpenRouter's Responses API is stateless, and the
// conversation travels whole in every request.
func (c *Client) Responses(ctx context.Context, body []byte, _ string) (*http.Response, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	body, err := c.prepare(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openrouter: build request: %w", err)
	}
	// Only these headers are sent. Nothing from the client's own request
	// reaches OpenRouter, so Claude Code's Anthropic credentials cannot leak.
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &APIError{StatusCode: resp.StatusCode, Body: data}
	}
	return resp, nil
}

// prepare adds the routing preferences and drops what OpenRouter must not
// receive.
func (c *Client) prepare(body []byte) ([]byte, error) {
	provider := make(map[string]any, len(c.Provider)+1)
	for key, value := range c.Provider {
		provider[key] = value
	}
	// Set last, so no configuration can weaken it.
	provider["data_collection"] = config.DataCollectionDeny

	encoded, err := json.Marshal(provider)
	if err != nil {
		return nil, fmt.Errorf("openrouter: encode provider preferences: %w", err)
	}
	body, err = sjson.SetRawBytes(body, "provider", encoded)
	if err != nil {
		return nil, fmt.Errorf("openrouter: set provider preferences: %w", err)
	}

	// A model without reasoning support has no effort to ask for; an empty
	// one would be rejected.
	if gjson.GetBytes(body, "reasoning.effort").String() == "" {
		body, _ = sjson.DeleteBytes(body, "reasoning")
	}
	return body, nil
}
