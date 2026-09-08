package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// loginTimeout bounds how long the callback server waits for the browser.
const loginTimeout = 5 * time.Minute

// callback carries the parameters the OAuth provider redirects back with.
type callback struct {
	code  string
	state string
	err   string
}

// LoginResult reports the outcome of an interactive login.
type LoginResult struct {
	Token *Token
	// AuthorizeURL is exposed so callers can print it when no browser opened.
	AuthorizeURL string
}

// Login runs the authorization code + PKCE flow end to end: it starts a
// loopback callback server, opens the browser, waits for the redirect and
// exchanges the code for tokens. openBrowser can be disabled for headless
// machines, where the URL is printed by the caller instead.
func (c *Client) Login(ctx context.Context, callbackPort int, openBrowser bool, announce func(string)) (*LoginResult, error) {
	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := RandomState()
	if err != nil {
		return nil, err
	}
	authorizeURL, err := AuthorizeURL(state, pkce, callbackPort)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("codex: cannot listen on the OAuth callback port %d (is the Codex CLI mid-login?): %w", callbackPort, err)
	}

	results := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		result := callback{
			code:  query.Get("code"),
			state: query.Get("state"),
			err:   query.Get("error"),
		}
		if desc := query.Get("error_description"); result.err != "" && desc != "" {
			result.err = result.err + ": " + desc
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if result.err != "" || result.code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(failurePage(result.err)))
		} else {
			_, _ = w.Write([]byte(successPage))
		}

		select {
		case results <- result:
		default:
		}
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case results <- callback{err: err.Error()}:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if announce != nil {
		announce(authorizeURL)
	}
	if openBrowser {
		if err := openURL(authorizeURL); err != nil {
			return nil, fmt.Errorf("codex: open browser: %w", err)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var result callback
	select {
	case result = <-results:
	case <-waitCtx.Done():
		return nil, fmt.Errorf("codex: timed out waiting for the OAuth callback")
	}

	if result.err != "" {
		return nil, fmt.Errorf("codex: authorization failed: %s", result.err)
	}
	if result.state != state {
		return nil, fmt.Errorf("codex: OAuth state mismatch, aborting")
	}
	if result.code == "" {
		return nil, fmt.Errorf("codex: authorization callback carried no code")
	}

	token, err := c.Exchange(ctx, result.code, pkce, callbackPort)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Token: token, AuthorizeURL: authorizeURL}, nil
}

func openURL(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

const successPage = `<!doctype html><meta charset="utf-8"><title>Signed in</title>
<style>body{font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;color:#111}
main{text-align:center}h1{font-size:20px}p{color:#555}</style>
<main><h1>Codex account connected</h1><p>You can close this tab and return to the terminal.</p></main>`

func failurePage(reason string) string {
	if strings.TrimSpace(reason) == "" {
		reason = "the authorization server did not return a code"
	}
	return `<!doctype html><meta charset="utf-8"><title>Sign-in failed</title>
<style>body{font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;color:#111}
main{text-align:center}h1{font-size:20px}p{color:#a00}</style>
<main><h1>Sign-in failed</h1><p>` + htmlEscape(reason) + `</p></main>`
}

func htmlEscape(in string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(in)
}
