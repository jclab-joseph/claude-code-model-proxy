//go:build e2e

// Package e2e drives the real Claude Code CLI through a freshly built proxy
// and checks what a user would see: answers, files the agent changed, the
// models that served each turn.
//
// It talks to the real upstreams and costs money, so it only builds with the
// e2e tag:
//
//	go test -tags e2e -v -timeout 60m ./e2e
//
// The CLI must already be signed in. OpenRouter models need
// OPENROUTER_API_KEY, read from the environment or the repository's .env;
// Codex models need a Codex login. A model whose provider is not ready is
// skipped, not failed.
//
// Environment:
//
//	CCM_E2E_MODELS          main models to test, comma separated
//	                        (default z-ai/glm-5.3-flash,gpt-5.5)
//	CCM_E2E_CLAUDE_MODEL    the Claude model sessions switch from and back to
//	                        (default claude-haiku-4-5)
//	CCM_E2E_SUBAGENT_MODEL  the model of the delegated subagent (default opus)
//	CCM_E2E_PARALLEL        Claude Code processes run at once (default 4)
//	CCM_E2E_CLAUDE          the Claude Code binary (default claude)
//	CCM_E2E_KEEP            keep the work directory, transcripts and proxy log
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// env is the proxy every test talks through, set up once by TestMain.
var env struct {
	workDir    string
	proxyURL   string
	statusURL  string
	caCert     string
	proxyLog   string
	claude     string
	slots      chan struct{}
	readyModel map[string]bool
}

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	workDir, err := os.MkdirTemp("", "ccmproxy-e2e-")
	if err != nil {
		return 0, err
	}
	if os.Getenv("CCM_E2E_KEEP") == "" {
		defer func() { _ = os.RemoveAll(workDir) }()
	} else {
		fmt.Fprintf(os.Stderr, "e2e: keeping %s\n", workDir)
	}
	env.workDir = workDir
	env.claude = envOr("CCM_E2E_CLAUDE", "claude")

	parallel, _ := strconv.Atoi(envOr("CCM_E2E_PARALLEL", "4"))
	if parallel < 1 {
		parallel = 1
	}
	env.slots = make(chan struct{}, parallel)

	if os.Getenv("OPENROUTER_API_KEY") == "" {
		if err := loadDotEnv(filepath.Join("..", ".env")); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}

	binary := filepath.Join(workDir, "ccmproxy")
	build := exec.Command("go", "build", "-o", binary, "../cmd/ccmproxy")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return 0, fmt.Errorf("build proxy: %w", err)
	}

	proxyAddr, err := freeAddr()
	if err != nil {
		return 0, err
	}
	httpAddr, err := freeAddr()
	if err != nil {
		return 0, err
	}
	configPath := filepath.Join(workDir, "config.yaml")
	config := fmt.Sprintf(`listen_proxy: %s
listen_http: %s
ca_dir: %s
auth_dir: %s
log_level: debug
openrouter:
  models:
`, proxyAddr, httpAddr, filepath.Join(workDir, "ca"), filepath.Join(workDir, "auth"))
	for _, model := range mainModels() {
		// Codex models are built in; every other id is an OpenRouter one.
		if strings.Contains(model, "/") {
			config += "    - id: " + model + "\n"
		}
	}
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return 0, err
	}

	env.proxyLog = filepath.Join(workDir, "proxy.log")
	logFile, err := os.Create(env.proxyLog)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The argument order run.sh uses: the common flags before the command.
	proxy := exec.CommandContext(ctx, binary, "--config", configPath, "serve", "--experimental-modify-bootstrap-models")
	proxy.Stdout, proxy.Stderr = logFile, logFile
	if err := proxy.Start(); err != nil {
		return 0, fmt.Errorf("start proxy: %w", err)
	}
	defer func() { _ = proxy.Wait() }()
	defer cancel()

	env.proxyURL = "http://" + proxyAddr
	env.statusURL = "http://" + httpAddr + "/__ccmproxy/status"
	env.caCert = filepath.Join(workDir, "ca", "ca.crt")
	if err := waitForStatus(30 * time.Second); err != nil {
		return 0, fmt.Errorf("proxy did not come up (log: %s): %w", env.proxyLog, err)
	}
	return m.Run(), nil
}

// waitForStatus polls the status endpoint until the proxy answers, and records
// which models have a provider ready to serve them.
func waitForStatus(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(env.statusURL)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			env.readyModel = map[string]bool{}
			for _, provider := range gjson.GetBytes(body, "providers").Array() {
				for _, model := range provider.Get("models").Array() {
					env.readyModel[strings.ToLower(model.String())] = provider.Get("ready").Bool()
				}
			}
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func mainModels() []string {
	var models []string
	for _, model := range strings.Split(envOr("CCM_E2E_MODELS", "z-ai/glm-5.3-flash,gpt-5.5"), ",") {
		if model = strings.TrimSpace(model); model != "" {
			models = append(models, model)
		}
	}
	return models
}

func claudeModel() string   { return envOr("CCM_E2E_CLAUDE_MODEL", "claude-haiku-4-5") }
func subagentModel() string { return envOr("CCM_E2E_SUBAGENT_MODEL", "opus") }

// requireReady skips the test when no provider can serve model.
func requireReady(t *testing.T, model string) {
	t.Helper()
	if !env.readyModel[strings.ToLower(model)] {
		t.Skipf("%s: its provider is not ready (not signed in, or no API key)", model)
	}
}

// session is one Claude Code conversation in its own directory.
type session struct {
	t     *testing.T
	name  string
	dir   string
	id    string
	turns int
}

func newSession(t *testing.T) *session {
	t.Helper()
	name := strings.NewReplacer("/", "_", ":", "_").Replace(t.Name())
	dir := filepath.Join(env.workDir, "sessions", name)
	for _, path := range []string{dir, filepath.Join(env.workDir, "transcripts")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &session{t: t, name: name, dir: dir}
}

// write creates a file in the session directory.
func (s *session) write(name, content string) {
	s.t.Helper()
	path := filepath.Join(s.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// read returns a file from the session directory.
func (s *session) read(name string) string {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(data)
}

// turn is one `claude -p` invocation and the events it printed.
type turn struct {
	model  string
	events []gjson.Result
	result gjson.Result
	stderr string
	path   string
}

// ask runs one turn on model, resuming the session after the first.
func (s *session) ask(model, prompt string, extra ...string) *turn {
	s.t.Helper()
	env.slots <- struct{}{}
	defer func() { <-env.slots }()

	args := []string{"-p", prompt, "--model", model,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json", "--verbose",
		// The user's MCP servers would slow every turn down and are not
		// what is under test.
		"--strict-mcp-config"}
	if s.id != "" {
		args = append(args, "--resume", s.id)
	}
	args = append(args, extra...)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, env.claude, args...)
	cmd.Dir = s.dir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"HTTPS_PROXY="+env.proxyURL,
		"HTTP_PROXY="+env.proxyURL,
		"NODE_EXTRA_CA_CERTS="+env.caCert,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// Kept outside the session directory, where the agent would see them.
	s.turns++
	path := filepath.Join(env.workDir, "transcripts", fmt.Sprintf("%s-%d.jsonl", s.name, s.turns))
	tr := &turn{model: model, stderr: stderr.String(), path: path}
	_ = os.WriteFile(tr.path, stdout.Bytes(), 0o600)
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		event := gjson.ParseBytes(scanner.Bytes())
		tr.events = append(tr.events, event)
		if event.Get("type").String() == "result" {
			tr.result = event
		}
	}

	if !tr.result.Exists() {
		s.t.Fatalf("%s turn printed no result (%v); stderr:\n%s\nevents: %s\nproxy log: %s", model, runErr, tr.stderr, tr.path, env.proxyLog)
	}
	if tr.result.Get("is_error").Bool() || tr.result.Get("subtype").String() != "success" {
		s.t.Fatalf("%s turn failed: %s\nevents: %s\nproxy log: %s", model, tr.result.Get("result").String(), tr.path, env.proxyLog)
	}
	if id := tr.result.Get("session_id").String(); id != "" {
		s.id = id
	}
	return tr
}

// answer is the final text of the turn.
func (tr *turn) answer() string { return tr.result.Get("result").String() }

// assistant lists the assistant messages; subagent reports whether to list
// the subagents' messages instead of the main agent's.
func (tr *turn) assistant(subagent bool) []gjson.Result {
	var out []gjson.Result
	for _, event := range tr.events {
		if event.Get("type").String() != "assistant" {
			continue
		}
		parent := event.Get("parent_tool_use_id")
		if (parent.Exists() && parent.Type != gjson.Null) == subagent {
			out = append(out, event)
		}
	}
	return out
}

// toolUses lists the main agent's calls to the named tool.
func (tr *turn) toolUses(name string) []gjson.Result {
	var out []gjson.Result
	for _, message := range tr.assistant(false) {
		for _, block := range message.Get("message.content").Array() {
			if block.Get("type").String() == "tool_use" && block.Get("name").String() == name {
				out = append(out, block)
			}
		}
	}
	return out
}

// mostParallel reports the largest number of calls to the named tool that the
// main agent made in a single message. Claude Code streams one event per
// content block, so the blocks are grouped by message id.
func (tr *turn) mostParallel(name string) int {
	perMessage := map[string]int{}
	most := 0
	for _, message := range tr.assistant(false) {
		id := message.Get("message.id").String()
		for _, block := range message.Get("message.content").Array() {
			if block.Get("type").String() == "tool_use" && block.Get("name").String() == name {
				perMessage[id]++
				most = max(most, perMessage[id])
			}
		}
	}
	return most
}

// mustContain fails unless text contains every want, ignoring case.
func mustContain(t *testing.T, tr *turn, text string, want ...string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, w := range want {
		if !strings.Contains(lower, strings.ToLower(w)) {
			t.Fatalf("missing %q in:\n%s\nevents: %s", w, text, tr.path)
		}
	}
}

func freeAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String(), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// loadDotEnv sets the KEY=VALUE pairs of path that are not already set.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}
