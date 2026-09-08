package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// format identifies the on-disk credential layout.
type format int

const (
	// formatUnknown asks for the layout to be detected from the contents.
	formatUnknown format = iota
	// formatNative is this proxy's own file: a flat Token object.
	formatNative
	// formatCodexCLI is the Codex CLI's ~/.codex/auth.json, whose tokens sit
	// under a "tokens" object alongside fields this proxy does not own.
	formatCodexCLI
)

// codexCLIFileName is the credential file the Codex CLI writes.
const codexCLIFileName = "auth.json"

// CodexCLIHome reports the Codex CLI's configuration directory, honouring the
// CODEX_HOME override the CLI itself uses.
func CodexCLIHome() string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// CodexCLIAuthPath reports the Codex CLI credential file, or "" when the home
// directory cannot be determined.
func CodexCLIAuthPath() string {
	home := CodexCLIHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, codexCLIFileName)
}

// decodeCredentials parses either supported layout.
func decodeCredentials(data []byte, kind format) (*Token, error) {
	switch kind {
	case formatCodexCLI:
		return decodeCodexCLI(data)
	default:
		return decodeNative(data)
	}
}

func decodeNative(data []byte) (*Token, error) {
	var token Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("codex: parse credentials: %w", err)
	}
	enrich(&token)
	return &token, nil
}

// decodeCodexCLI reads the Codex CLI's auth.json. The file records no expiry,
// so it is derived from the access token's own claims.
func decodeCodexCLI(data []byte) (*Token, error) {
	if !gjson.ValidBytes(data) {
		return nil, fmt.Errorf("codex: %s is not valid JSON", codexCLIFileName)
	}
	root := gjson.ParseBytes(data)
	tokens := root.Get("tokens")
	if !tokens.IsObject() {
		return nil, fmt.Errorf("codex: %s has no \"tokens\" object", codexCLIFileName)
	}

	token := &Token{
		IDToken:      tokens.Get("id_token").String(),
		AccessToken:  tokens.Get("access_token").String(),
		RefreshToken: tokens.Get("refresh_token").String(),
		AccountID:    tokens.Get("account_id").String(),
		Type:         "codex",
	}
	if stamp := strings.TrimSpace(root.Get("last_refresh").String()); stamp != "" {
		if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
			token.LastRefresh = parsed
		}
	}
	enrich(token)

	if token.AccessToken == "" && token.RefreshToken == "" {
		return nil, ErrNotAuthenticated
	}
	// An API-key-only file cannot drive the subscription endpoint.
	if mode := root.Get("auth_mode").String(); mode != "" && mode != "chatgpt" {
		return nil, fmt.Errorf("codex: %s uses auth_mode %q; a ChatGPT subscription login is required", codexCLIFileName, mode)
	}
	return token, nil
}

// enrich fills in whatever the file did not record, reading the tokens' own
// claims. The access token is preferred: it is the credential whose lifetime
// actually matters, and it carries the account and plan, whereas the ID token
// expires within the hour and goes stale in a stored file.
func enrich(token *Token) {
	if token == nil {
		return
	}

	if claims, err := ParseIDToken(token.AccessToken); err == nil && claims != nil {
		if token.ExpiresAt.IsZero() && claims.Exp > 0 {
			token.ExpiresAt = time.Unix(claims.Exp, 0)
		}
		if token.AccountID == "" {
			token.AccountID = claims.Auth.ChatGPTAccountID
		}
		if token.PlanType == "" {
			token.PlanType = claims.Auth.ChatGPTPlanType
		}
	}

	// The email only appears in the ID token.
	if token.Email == "" || token.AccountID == "" || token.PlanType == "" {
		if claims, err := ParseIDToken(token.IDToken); err == nil && claims != nil {
			if token.Email == "" {
				token.Email = claims.Email
			}
			if token.AccountID == "" {
				token.AccountID = claims.Auth.ChatGPTAccountID
			}
			if token.PlanType == "" {
				token.PlanType = claims.Auth.ChatGPTPlanType
			}
		}
	}

	if token.Type == "" {
		token.Type = "codex"
	}
}

// encodeCredentials renders token in the given layout. For the Codex CLI
// layout, previous holds the file's current contents so fields this proxy does
// not own (auth_mode, OPENAI_API_KEY, anything added later) survive the write.
func encodeCredentials(token *Token, kind format, previous []byte) ([]byte, error) {
	if token == nil {
		return nil, fmt.Errorf("codex: nothing to encode")
	}
	token.Type = "codex"

	if kind == formatNative {
		data, err := json.MarshalIndent(token, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("codex: encode credentials: %w", err)
		}
		return append(data, '\n'), nil
	}

	out := previous
	if !gjson.ValidBytes(out) {
		out = []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null}`)
	}
	for path, value := range map[string]string{
		"tokens.id_token":      token.IDToken,
		"tokens.access_token":  token.AccessToken,
		"tokens.refresh_token": token.RefreshToken,
		"tokens.account_id":    token.AccountID,
	} {
		var err error
		if out, err = sjson.SetBytes(out, path, value); err != nil {
			return nil, fmt.Errorf("codex: update %s: %w", path, err)
		}
	}
	refreshed := token.LastRefresh
	if refreshed.IsZero() {
		refreshed = time.Now()
	}
	out, err := sjson.SetBytes(out, "last_refresh", refreshed.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("codex: update last_refresh: %w", err)
	}
	return append(out, '\n'), nil
}
