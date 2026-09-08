package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/tidwall/gjson"
)

// ErrNotAuthenticated is returned when no Codex credentials can be found.
var ErrNotAuthenticated = errors.New("codex: not authenticated, run `ccmproxy login codex` or sign in with the Codex CLI")

// refreshLead refreshes an access token this far before it expires.
const refreshLead = 5 * time.Minute

// nativeFileName is this proxy's own credential file.
const nativeFileName = "codex.json"

// StoreOptions configures where credentials are looked for.
type StoreOptions struct {
	// Dir is where this proxy keeps its own credential file.
	Dir string
	// ExplicitPath, when set, is the only file consulted, in whichever
	// layout it turns out to be.
	ExplicitPath string
	// UseCodexCLI enables reading (and keeping in sync) the Codex CLI's
	// ~/.codex/auth.json when this proxy has no credentials of its own.
	UseCodexCLI bool
}

// source is one candidate credential file.
type source struct {
	path   string
	format format
}

// Store finds, caches and refreshes Codex credentials.
//
// Credentials are looked for in order: an explicitly configured file, this
// proxy's own file, then the Codex CLI's ~/.codex/auth.json. A refreshed token
// is written back to whichever file it came from, so signing in with either
// tool is enough and the two stay in sync.
type Store struct {
	client  *Client
	options StoreOptions

	mu     sync.Mutex
	token  *Token
	active source
	// stamp is the size and modification time of the active file when it was
	// read, so an external update (the Codex CLI refreshing its own token)
	// is noticed instead of being overwritten from a stale cache.
	stamp fileStamp
}

type fileStamp struct {
	size    int64
	modTime time.Time
}

// NewStore builds a store from the given options.
func NewStore(client *Client, options StoreOptions) *Store {
	if client == nil {
		client = NewClient(nil)
	}
	return &Store{client: client, options: options}
}

// NativePath reports this proxy's own credential file.
func (s *Store) NativePath() string {
	if path := strings.TrimSpace(s.options.ExplicitPath); path != "" {
		return path
	}
	return filepath.Join(s.options.Dir, nativeFileName)
}

// Path reports the file credentials were loaded from, falling back to the file
// a fresh login would write.
func (s *Store) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active.path != "" {
		return s.active.path
	}
	return s.NativePath()
}

// Sources lists the candidate files in the order they are consulted.
func (s *Store) Sources() []string {
	sources := s.candidates()
	paths := make([]string, 0, len(sources))
	for _, candidate := range sources {
		paths = append(paths, candidate.path)
	}
	return paths
}

func (s *Store) candidates() []source {
	if path := strings.TrimSpace(s.options.ExplicitPath); path != "" {
		// The layout of an explicitly named file is detected from its
		// contents rather than assumed.
		return []source{{path: path, format: formatUnknown}}
	}

	candidates := []source{{path: filepath.Join(s.options.Dir, nativeFileName), format: formatNative}}
	if s.options.UseCodexCLI {
		if path := CodexCLIAuthPath(); path != "" {
			candidates = append(candidates, source{path: path, format: formatCodexCLI})
		}
	}
	return candidates
}

// Save writes token to this proxy's own credential file and installs it as the
// active credential.
func (s *Store) Save(token *Token) error {
	if token == nil {
		return fmt.Errorf("codex: nothing to save")
	}
	path := s.NativePath()
	kind := formatNative
	// Writing to an explicitly configured file keeps that file's layout.
	if strings.TrimSpace(s.options.ExplicitPath) != "" {
		kind = detectFormatOfFile(path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(token, source{path: path, format: kind})
}

// Peek returns the cached or on-disk token without refreshing it.
func (s *Store) Peek() (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Authenticated reports whether credentials are available.
func (s *Store) Authenticated() bool {
	_, err := s.Peek()
	return err == nil
}

func (s *Store) loadLocked() (*Token, error) {
	// A file changed underneath us (the Codex CLI refreshing its own token)
	// invalidates the cache: its refresh token may already be rotated.
	if s.token != nil && !s.staleLocked() {
		return s.token, nil
	}

	var lastErr error
	for _, candidate := range s.candidates() {
		data, err := os.ReadFile(candidate.path)
		if err != nil {
			if !os.IsNotExist(err) {
				lastErr = fmt.Errorf("codex: read %s: %w", candidate.path, err)
			}
			continue
		}

		kind := candidate.format
		if kind == formatUnknown {
			kind = detectFormat(data)
		}
		token, err := decodeCredentials(data, kind)
		if err != nil {
			lastErr = err
			continue
		}
		if token.AccessToken == "" && token.RefreshToken == "" {
			continue
		}

		if s.active.path != candidate.path {
			logx.Infof("codex: using credentials from %s", candidate.path)
		}
		s.token = token
		s.active = source{path: candidate.path, format: kind}
		s.stamp = stampOf(candidate.path)
		return token, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNotAuthenticated
}

// staleLocked reports whether the active file changed since it was read.
func (s *Store) staleLocked() bool {
	if s.active.path == "" {
		return true
	}
	current := stampOf(s.active.path)
	if current.size == 0 && current.modTime.IsZero() {
		// The file went away; fall back to rediscovery.
		return true
	}
	return current != s.stamp
}

// Access returns a usable access token together with the account id,
// refreshing the credential when it is close to expiry.
func (s *Store) Access(ctx context.Context) (accessToken, accountID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := s.loadLocked()
	if err != nil {
		return "", "", err
	}
	if !token.Expired(refreshLead) {
		return token.AccessToken, token.AccountID, nil
	}
	if token.RefreshToken == "" {
		return "", "", ErrNotAuthenticated
	}

	logx.Infof("codex: refreshing access token (expires %s)", expiryLabel(token))
	refreshed, err := s.client.Refresh(context.WithoutCancel(ctx), token.RefreshToken)
	if err != nil {
		// A still-valid token is worth trying even if the refresh failed;
		// only a genuinely expired one is a hard error.
		if !token.Expired(0) {
			logx.Warnf("codex: token refresh failed, using the current token: %v", err)
			return token.AccessToken, token.AccountID, nil
		}
		return "", "", err
	}

	// A refresh response repeats little about the account; carry the rest
	// over from the token being replaced.
	if refreshed.AccountID == "" {
		refreshed.AccountID = token.AccountID
	}
	if refreshed.Email == "" {
		refreshed.Email = token.Email
	}
	if refreshed.PlanType == "" {
		refreshed.PlanType = token.PlanType
	}
	if refreshed.IDToken == "" {
		refreshed.IDToken = token.IDToken
	}

	// The refreshed token goes back where it came from, so a Codex CLI login
	// this proxy is borrowing stays usable by the CLI itself.
	if err := s.writeLocked(refreshed, s.active); err != nil {
		logx.Warnf("codex: could not persist the refreshed token: %v", err)
		s.token = refreshed
	}
	return refreshed.AccessToken, refreshed.AccountID, nil
}

// writeLocked persists token to the given file, preserving any fields of that
// file this proxy does not own.
func (s *Store) writeLocked(token *Token, target source) error {
	if target.path == "" {
		return fmt.Errorf("codex: no credential file to write to")
	}
	if token.LastRefresh.IsZero() {
		token.LastRefresh = time.Now()
	}

	var previous []byte
	if target.format == formatCodexCLI {
		previous, _ = os.ReadFile(target.path)
	}
	data, err := encodeCredentials(token, target.format, previous)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(target.path), 0o700); err != nil {
		return fmt.Errorf("codex: create %s: %w", filepath.Dir(target.path), err)
	}
	if err := os.WriteFile(target.path, data, 0o600); err != nil {
		return fmt.Errorf("codex: write %s: %w", target.path, err)
	}

	s.token = token
	s.active = target
	s.stamp = stampOf(target.path)
	return nil
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: info.Size(), modTime: info.ModTime()}
}

func expiryLabel(token *Token) string {
	if token == nil || token.ExpiresAt.IsZero() {
		return "unknown"
	}
	return token.ExpiresAt.Format(time.RFC3339)
}

// detectFormat picks the layout a credential file is written in.
func detectFormat(data []byte) format {
	if gjson.GetBytes(data, "tokens").IsObject() {
		return formatCodexCLI
	}
	return formatNative
}

func detectFormatOfFile(path string) format {
	data, err := os.ReadFile(path)
	if err != nil {
		return formatNative
	}
	return detectFormat(data)
}
