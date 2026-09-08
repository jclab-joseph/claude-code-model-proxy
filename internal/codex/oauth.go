// Package codex authenticates against the OpenAI Codex (ChatGPT subscription)
// backend and speaks its Responses API. The OAuth client id, endpoints and
// redirect URI mirror the Codex CLI, which is what the subscription endpoint
// accepts; the flow itself is a standard OAuth 2.0 authorization code grant
// with PKCE (RFC 7636).
package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// AuthURL and TokenURL are the OpenAI OAuth endpoints.
	AuthURL  = "https://auth.openai.com/oauth/authorize"
	TokenURL = "https://auth.openai.com/oauth/token"
	// ClientID is the public client id of the Codex CLI.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// Scopes requests an ID token plus a refresh token.
	Scopes = "openid email profile offline_access"

	exchangeTimeout = 30 * time.Second
)

// RedirectURI builds the loopback redirect URI for the given callback port.
// The registered client only accepts the Codex CLI's port, so callers should
// stay on the default unless they know better.
func RedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/auth/callback", port)
}

// PKCE holds a verifier and its derived S256 challenge.
type PKCE struct {
	Verifier  string `json:"code_verifier"`
	Challenge string `json:"code_challenge"`
}

// NewPKCE generates a fresh verifier/challenge pair.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("codex: generate code verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// RandomState returns an opaque value for the OAuth state parameter.
func RandomState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("codex: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// AuthorizeURL builds the URL the user opens to grant access.
func AuthorizeURL(state string, pkce *PKCE, callbackPort int) (string, error) {
	if pkce == nil {
		return "", fmt.Errorf("codex: PKCE codes are required")
	}
	params := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI(callbackPort)},
		"scope":                      {Scopes},
		"state":                      {state},
		"code_challenge":             {pkce.Challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return AuthURL + "?" + params.Encode(), nil
}

// Token is the credential set persisted for a Codex account.
type Token struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	Email        string    `json:"email"`
	PlanType     string    `json:"plan_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	LastRefresh  time.Time `json:"last_refresh"`
	Type         string    `json:"type"`
}

// Expired reports whether the access token is past its lifetime, treating the
// final lead window as already expired so refreshes happen ahead of failures.
func (t *Token) Expired(lead time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(lead).After(t.ExpiresAt)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// Client performs the OAuth token calls.
type Client struct {
	HTTP *http.Client
}

// NewClient returns a Client with a sane timeout.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: exchangeTimeout}
	}
	return &Client{HTTP: httpClient}
}

// Exchange trades an authorization code for tokens.
func (c *Client) Exchange(ctx context.Context, code string, pkce *PKCE, callbackPort int) (*Token, error) {
	if pkce == nil {
		return nil, fmt.Errorf("codex: PKCE codes are required for token exchange")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {RedirectURI(callbackPort)},
		"code_verifier": {pkce.Verifier},
	}
	return c.post(ctx, form)
}

// Refresh exchanges a refresh token for a new access token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("codex: refresh token is required")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	token, err := c.post(ctx, form)
	if err != nil {
		return nil, err
	}
	// A refresh response may omit the refresh token, in which case the
	// existing one stays valid.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, nil
}

func (c *Client) post(ctx context.Context, form url.Values) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codex: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex: token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("codex: parse token response: %w", err)
	}

	token := &Token{
		IDToken:      parsed.IDToken,
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		LastRefresh:  time.Now(),
		Type:         "codex",
	}
	if parsed.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	if claims, err := ParseIDToken(parsed.IDToken); err == nil && claims != nil {
		token.AccountID = claims.Auth.ChatGPTAccountID
		token.Email = claims.Email
		token.PlanType = claims.Auth.ChatGPTPlanType
	}
	return token, nil
}

// Claims is the subset of the ID token payload the proxy needs.
type Claims struct {
	Email string   `json:"email"`
	Exp   int64    `json:"exp"`
	Auth  AuthInfo `json:"https://api.openai.com/auth"`
}

// AuthInfo carries the ChatGPT account details embedded in the ID token.
type AuthInfo struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTPlanType  string `json:"chatgpt_plan_type"`
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	UserID           string `json:"user_id"`
}

// ParseIDToken decodes the claims of a JWT without verifying its signature.
// The token arrives over TLS straight from the issuer's token endpoint, so it
// is only being read for the account id and plan, never trusted as proof.
func ParseIDToken(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("codex: malformed JWT: expected 3 parts, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("codex: decode JWT payload: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("codex: parse JWT payload: %w", err)
	}
	return &claims, nil
}
