package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPKCEChallengeIsS256OfVerifier(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Fatalf("verifier is %d characters, RFC 7636 requires 43 to 128", len(pkce.Verifier))
	}
	sum := sha256.Sum256([]byte(pkce.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); pkce.Challenge != want {
		t.Fatalf("challenge = %q, want the S256 of the verifier", pkce.Challenge)
	}
	if strings.ContainsAny(pkce.Verifier+pkce.Challenge, "+/=") {
		t.Fatal("PKCE codes must be base64url without padding")
	}
}

func TestAuthorizeURLParameters(t *testing.T) {
	pkce := &PKCE{Verifier: "v", Challenge: "c"}
	raw, err := AuthorizeURL("state-123", pkce, 1455)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != AuthURL {
		t.Fatalf("endpoint = %q, want %q", got, AuthURL)
	}

	query := parsed.Query()
	want := map[string]string{
		"client_id":             ClientID,
		"response_type":         "code",
		"redirect_uri":          "http://localhost:1455/auth/callback",
		"state":                 "state-123",
		"code_challenge":        "c",
		"code_challenge_method": "S256",
		"scope":                 Scopes,
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
}

// idToken builds an unsigned JWT with the given claims payload.
func idToken(payload string) string {
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return encode(`{"alg":"none"}`) + "." + encode(payload) + ".signature"
}

func TestParseIDTokenExtractsAccount(t *testing.T) {
	token := idToken(`{"email":"user@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct_1","chatgpt_plan_type":"pro"}}`)

	claims, err := ParseIDToken(token)
	if err != nil {
		t.Fatalf("ParseIDToken: %v", err)
	}
	if claims.Email != "user@example.com" {
		t.Fatalf("email = %q", claims.Email)
	}
	if claims.Auth.ChatGPTAccountID != "acct_1" {
		t.Fatalf("account id = %q", claims.Auth.ChatGPTAccountID)
	}
	if claims.Auth.ChatGPTPlanType != "pro" {
		t.Fatalf("plan = %q", claims.Auth.ChatGPTPlanType)
	}

	if _, err := ParseIDToken("not-a-jwt"); err == nil {
		t.Fatal("a malformed token must be rejected")
	}
}

// tokenEndpoint serves a fake OAuth token endpoint and records the form it
// received.
func tokenEndpoint(t *testing.T, response string, status int) (*Client, *url.Values) {
	t.Helper()
	received := &url.Values{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		*received = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.Client())
	// Point the client at the fake endpoint by overriding the transport's
	// destination rather than the package constant.
	client.HTTP = &http.Client{Transport: rewriteTo(server.URL)}
	return client, received
}

// rewriteTo sends every request to the given base URL.
type rewriteTransport struct {
	base *url.URL
}

func rewriteTo(base string) http.RoundTripper {
	parsed, _ := url.Parse(base)
	return &rewriteTransport{base: parsed}
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.base.Scheme
	clone.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestExchangeSendsPKCEAndParsesToken(t *testing.T) {
	response := `{"access_token":"at","refresh_token":"rt","id_token":"` +
		idToken(`{"email":"a@b.c","https://api.openai.com/auth":{"chatgpt_account_id":"acct_9","chatgpt_plan_type":"plus"}}`) +
		`","expires_in":3600,"token_type":"Bearer"}`

	client, form := tokenEndpoint(t, response, http.StatusOK)

	token, err := client.Exchange(context.Background(), "the-code", &PKCE{Verifier: "the-verifier", Challenge: "c"}, 1455)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if got := form.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := form.Get("code_verifier"); got != "the-verifier" {
		t.Fatalf("code_verifier = %q", got)
	}
	if got := form.Get("code"); got != "the-code" {
		t.Fatalf("code = %q", got)
	}
	if got := form.Get("redirect_uri"); got != "http://localhost:1455/auth/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}

	if token.AccessToken != "at" || token.RefreshToken != "rt" {
		t.Fatalf("token = %+v", token)
	}
	if token.AccountID != "acct_9" || token.Email != "a@b.c" || token.PlanType != "plus" {
		t.Fatalf("account details were not taken from the ID token: %+v", token)
	}
	if remaining := time.Until(token.ExpiresAt); remaining < 55*time.Minute || remaining > time.Hour {
		t.Fatalf("expires_at is %v away, want about an hour", remaining)
	}
}

func TestRefreshKeepsExistingRefreshTokenWhenOmitted(t *testing.T) {
	client, form := tokenEndpoint(t, `{"access_token":"at2","expires_in":3600}`, http.StatusOK)

	token, err := client.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
	if token.AccessToken != "at2" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
	if token.RefreshToken != "old-refresh" {
		t.Fatalf("refresh token = %q, want the existing one carried forward", token.RefreshToken)
	}
}

func TestTokenEndpointErrorIsReported(t *testing.T) {
	client, _ := tokenEndpoint(t, `{"error":"invalid_grant"}`, http.StatusBadRequest)

	if _, err := client.Refresh(context.Background(), "rt"); err == nil {
		t.Fatal("a non-200 token response must be an error")
	} else if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error = %v, want it to carry the upstream message", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	fresh := &Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}
	if fresh.Expired(5 * time.Minute) {
		t.Fatal("a token valid for an hour must not be considered expired")
	}
	if !fresh.Expired(2 * time.Hour) {
		t.Fatal("a lead longer than the remaining lifetime must report expiry")
	}

	var missing *Token
	if !missing.Expired(0) {
		t.Fatal("a nil token must report expiry")
	}
	if !(&Token{}).Expired(0) {
		t.Fatal("a token without an access token must report expiry")
	}
	// A token with no recorded expiry is taken at face value.
	if (&Token{AccessToken: "at"}).Expired(time.Hour) {
		t.Fatal("a token with no expiry must not be refreshed preemptively")
	}
}

func TestStoreRefreshesExpiredTokenAndPersists(t *testing.T) {
	response := `{"access_token":"refreshed","refresh_token":"rt2","expires_in":3600}`
	client, _ := tokenEndpoint(t, response, http.StatusOK)

	dir := t.TempDir()
	store := NewStore(client, StoreOptions{Dir: dir})
	if err := store.Save(&Token{
		AccessToken:  "stale",
		RefreshToken: "rt1",
		AccountID:    "acct_1",
		Email:        "a@b.c",
		PlanType:     "pro",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	access, account, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != "refreshed" {
		t.Fatalf("access token = %q, want the refreshed one", access)
	}
	// Account details are not repeated in a refresh response, so they must be
	// carried over from the stored token.
	if account != "acct_1" {
		t.Fatalf("account id = %q, want it preserved across refresh", account)
	}

	reloaded := NewStore(client, StoreOptions{Dir: dir})
	token, err := reloaded.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if token.AccessToken != "refreshed" || token.RefreshToken != "rt2" {
		t.Fatalf("persisted token = %+v", token)
	}
	if token.Email != "a@b.c" || token.PlanType != "pro" {
		t.Fatalf("account details were lost on refresh: %+v", token)
	}
}

func TestStoreWithoutCredentials(t *testing.T) {
	store := NewStore(nil, StoreOptions{Dir: t.TempDir()})

	if store.Authenticated() {
		t.Fatal("an empty directory must not count as authenticated")
	}
	if _, _, err := store.Access(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("Access error = %v, want ErrNotAuthenticated", err)
	}
}

func TestStoreKeepsValidTokenWhenRefreshFails(t *testing.T) {
	client, _ := tokenEndpoint(t, `{"error":"server_error"}`, http.StatusInternalServerError)

	store := NewStore(client, StoreOptions{Dir: t.TempDir()})
	// Inside the refresh lead but still valid: a failed refresh must not
	// break a request that could still succeed.
	if err := store.Save(&Token{
		AccessToken:  "still-valid",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	access, _, err := store.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != "still-valid" {
		t.Fatalf("access token = %q, want the current one reused", access)
	}
}

func TestStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(nil, StoreOptions{Dir: dir})
	if err := store.Save(&Token{AccessToken: "at"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", mode)
	}

	// The stored file must be readable back as JSON.
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var token Token
	if err := json.Unmarshal(data, &token); err != nil {
		t.Fatalf("stored credentials are not valid JSON: %v", err)
	}
	if token.Type != "codex" {
		t.Fatalf("type = %q, want codex", token.Type)
	}
}

func TestNewSessionIDFormat(t *testing.T) {
	id := NewSessionID()
	if len(id) != 36 {
		t.Fatalf("session id = %q, want a 36-character UUID", id)
	}
	if id[14] != '4' {
		t.Fatalf("session id = %q, want version 4", id)
	}
	if NewSessionID() == id {
		t.Fatal("session ids must not repeat")
	}
}
