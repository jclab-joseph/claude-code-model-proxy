package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// jwt builds an unsigned JWT carrying the given claims.
func jwt(claims string) string {
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return encode(`{"alg":"none"}`) + "." + encode(claims) + ".sig"
}

// accessToken builds an access token shaped like the real one: it carries its
// own expiry plus the account and plan.
func accessToken(expiresIn time.Duration, account, plan string) string {
	return jwt(fmt.Sprintf(
		`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_plan_type":%q}}`,
		time.Now().Add(expiresIn).Unix(), account, plan))
}

// writeCodexCLIAuth writes a ~/.codex/auth.json in the Codex CLI's layout and
// returns its path.
func writeCodexCLIAuth(t *testing.T, dir string, access, refresh string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	body := fmt.Sprintf(`{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": %q,
    "access_token": %q,
    "refresh_token": %q,
    "account_id": "acct_cli"
  },
  "last_refresh": "2026-08-29T01:17:11.660495951Z"
}`, jwt(`{"email":"cli@example.com"}`), access, refresh)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

func TestCodexCLIHomeHonoursEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/codex-home-test")
	if got := CodexCLIHome(); got != "/tmp/codex-home-test" {
		t.Fatalf("CodexCLIHome() = %q", got)
	}
	if got := CodexCLIAuthPath(); got != "/tmp/codex-home-test/auth.json" {
		t.Fatalf("CodexCLIAuthPath() = %q", got)
	}
}

func TestDecodeCodexCLIAuth(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexCLIAuth(t, dir, accessToken(48*time.Hour, "acct_from_token", "pro"), "rt-cli")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	token, err := decodeCredentials(data, formatCodexCLI)
	if err != nil {
		t.Fatalf("decodeCredentials: %v", err)
	}

	if token.RefreshToken != "rt-cli" {
		t.Fatalf("refresh token = %q", token.RefreshToken)
	}
	// account_id is recorded in the file itself and wins over the claim.
	if token.AccountID != "acct_cli" {
		t.Fatalf("account id = %q, want the value from the file", token.AccountID)
	}
	if token.Email != "cli@example.com" {
		t.Fatalf("email = %q, want it read from the ID token", token.Email)
	}
	if token.PlanType != "pro" {
		t.Fatalf("plan = %q, want it read from the access token", token.PlanType)
	}
	// The file records no expiry, so it comes from the access token's exp.
	if token.ExpiresAt.IsZero() {
		t.Fatal("expiry was not derived from the access token")
	}
	if remaining := time.Until(token.ExpiresAt); remaining < 47*time.Hour {
		t.Fatalf("expiry is %v away, want about 48h", remaining)
	}
	if token.Expired(refreshLead) {
		t.Fatal("a token valid for two days must not be treated as expired")
	}
}

func TestDecodeCodexCLIRejectsApiKeyMode(t *testing.T) {
	body := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test","tokens":{"access_token":"at","refresh_token":"rt"}}`
	if _, err := decodeCredentials([]byte(body), formatCodexCLI); err == nil {
		t.Fatal("an API-key login cannot drive the subscription endpoint and must be rejected")
	}
}

func TestDecodeCodexCLIRejectsMalformedFile(t *testing.T) {
	if _, err := decodeCredentials([]byte(`{oops`), formatCodexCLI); err == nil {
		t.Fatal("invalid JSON must be rejected")
	}
	if _, err := decodeCredentials([]byte(`{"auth_mode":"chatgpt"}`), formatCodexCLI); err == nil {
		t.Fatal("a file with no tokens object must be rejected")
	}
}

func TestDetectFormat(t *testing.T) {
	if got := detectFormat([]byte(`{"tokens":{"access_token":"at"}}`)); got != formatCodexCLI {
		t.Fatalf("detectFormat on a CLI file = %v", got)
	}
	if got := detectFormat([]byte(`{"access_token":"at"}`)); got != formatNative {
		t.Fatalf("detectFormat on a native file = %v", got)
	}
}

func TestStoreFallsBackToCodexCLILogin(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_x", "plus"), "rt-cli")
	t.Setenv("CODEX_HOME", codexHome)

	// This proxy has no credentials of its own.
	store := NewStore(nil, StoreOptions{Dir: t.TempDir(), UseCodexCLI: true})

	if !store.Authenticated() {
		t.Fatal("an existing Codex CLI login must be usable as is")
	}
	access, account, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access == "" {
		t.Fatal("no access token was returned")
	}
	if account != "acct_cli" {
		t.Fatalf("account id = %q", account)
	}
	if store.Path() != filepath.Join(codexHome, "auth.json") {
		t.Fatalf("active file = %q, want the Codex CLI file", store.Path())
	}
}

func TestStoreIgnoresCodexCLIWhenDisabled(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_x", "plus"), "rt-cli")
	t.Setenv("CODEX_HOME", codexHome)

	store := NewStore(nil, StoreOptions{Dir: t.TempDir(), UseCodexCLI: false})
	if store.Authenticated() {
		t.Fatal("the Codex CLI file must not be read when the fallback is disabled")
	}
}

func TestStorePrefersItsOwnCredentials(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_cli", "plus"), "rt-cli")
	t.Setenv("CODEX_HOME", codexHome)

	dir := t.TempDir()
	own := NewStore(nil, StoreOptions{Dir: dir, UseCodexCLI: true})
	if err := own.Save(&Token{
		AccessToken: "own-access",
		AccountID:   "acct_own",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	store := NewStore(nil, StoreOptions{Dir: dir, UseCodexCLI: true})
	access, account, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != "own-access" || account != "acct_own" {
		t.Fatalf("used %q/%q, want this proxy's own credentials to win", access, account)
	}
}

func TestRefreshWritesBackToTheCodexCLIFile(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	path := writeCodexCLIAuth(t, codexHome, accessToken(-time.Hour, "acct_cli", "plus"), "rt-old")
	t.Setenv("CODEX_HOME", codexHome)

	newAccess := accessToken(48*time.Hour, "acct_cli", "plus")
	client, _ := tokenEndpoint(t, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-new","expires_in":3600}`, newAccess), 200)

	store := NewStore(client, StoreOptions{Dir: t.TempDir(), UseCodexCLI: true})
	access, _, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != newAccess {
		t.Fatal("the expired token was not refreshed")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	updated := gjson.ParseBytes(data)

	// The refreshed credentials must land in the CLI's own layout, so the
	// CLI keeps working.
	if got := updated.Get("tokens.access_token").String(); got != newAccess {
		t.Fatalf("tokens.access_token was not updated: %s", data)
	}
	if got := updated.Get("tokens.refresh_token").String(); got != "rt-new" {
		t.Fatalf("tokens.refresh_token = %q", got)
	}
	// Fields this proxy does not own must survive untouched.
	if got := updated.Get("auth_mode").String(); got != "chatgpt" {
		t.Fatalf("auth_mode = %q, want it preserved", got)
	}
	if !updated.Get("OPENAI_API_KEY").Exists() {
		t.Fatalf("OPENAI_API_KEY was dropped: %s", data)
	}
	if got := updated.Get("last_refresh").String(); got == "2026-08-29T01:17:11.660495951Z" {
		t.Fatal("last_refresh was not advanced")
	}
	// The rewritten file must still be parseable as a CLI credential file.
	if _, err := decodeCredentials(data, formatCodexCLI); err != nil {
		t.Fatalf("the rewritten file no longer decodes: %v", err)
	}
}

func TestStoreNoticesAnExternalUpdate(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_cli", "plus"), "rt-1")
	t.Setenv("CODEX_HOME", codexHome)

	store := NewStore(nil, StoreOptions{Dir: t.TempDir(), UseCodexCLI: true})
	first, _, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}

	// The Codex CLI refreshes its own token behind our back; the cached copy
	// must not be used, because its refresh token may already be rotated.
	time.Sleep(10 * time.Millisecond)
	replacement := accessToken(72*time.Hour, "acct_cli", "plus")
	writeCodexCLIAuth(t, codexHome, replacement, "rt-2")

	second, _, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access after external update: %v", err)
	}
	if second == first {
		t.Fatal("the store kept using a stale cached token after the file changed")
	}
	if second != replacement {
		t.Fatalf("access token = %q, want the one now on disk", second)
	}
}

func TestExplicitAuthFileDetectsEitherLayout(t *testing.T) {
	codexHome := t.TempDir()
	path := writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_cli", "plus"), "rt")

	store := NewStore(nil, StoreOptions{ExplicitPath: path})
	if sources := store.Sources(); len(sources) != 1 || sources[0] != path {
		t.Fatalf("sources = %v, want only the configured file", sources)
	}
	token, err := store.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if token.AccountID != "acct_cli" {
		t.Fatalf("account id = %q", token.AccountID)
	}

	// A native file at an explicit path must work too.
	nativePath := filepath.Join(t.TempDir(), "creds.json")
	data, _ := json.Marshal(Token{AccessToken: "native", ExpiresAt: time.Now().Add(time.Hour)})
	if err := os.WriteFile(nativePath, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	nativeStore := NewStore(nil, StoreOptions{ExplicitPath: nativePath})
	if token, err := nativeStore.Peek(); err != nil {
		t.Fatalf("Peek: %v", err)
	} else if token.AccessToken != "native" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
}

func TestSaveKeepsAnExplicitCodexCLIFileInItsOwnLayout(t *testing.T) {
	codexHome := t.TempDir()
	path := writeCodexCLIAuth(t, codexHome, accessToken(48*time.Hour, "acct_cli", "plus"), "rt")

	store := NewStore(nil, StoreOptions{ExplicitPath: path})
	if err := store.Save(&Token{AccessToken: "brand-new", RefreshToken: "rt2", AccountID: "acct_new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := gjson.GetBytes(data, "tokens.access_token").String(); got != "brand-new" {
		t.Fatalf("the file was not written in the Codex CLI layout: %s", data)
	}
	if got := gjson.GetBytes(data, "auth_mode").String(); got != "chatgpt" {
		t.Fatalf("auth_mode = %q, want it preserved", got)
	}
}
