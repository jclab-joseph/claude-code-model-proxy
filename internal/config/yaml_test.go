package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
# Only the TLS listener, as in the hosts-file setup.
listen_tls: 127.1.0.101:443
intercept_hosts: [API.Anthropic.com]
codex:
  model_prefix: codex
  models:
    - gpt-5.5
dns:
  static:
    api.anthropic.com: [160.79.104.10]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenTLS != "127.1.0.101:443" {
		t.Fatalf("listen_tls = %q", cfg.ListenTLS)
	}
	// Listener opt-in works the same as in JSON.
	if cfg.ListenProxy != "" || cfg.ListenHTTP != "" {
		t.Fatalf("unlisted listeners = %q / %q, want both disabled", cfg.ListenProxy, cfg.ListenHTTP)
	}
	if !cfg.Intercepts("api.anthropic.com") {
		t.Fatalf("intercept hosts = %v", cfg.InterceptHosts)
	}
	if cfg.Codex.ModelPrefix != "codex/" || len(cfg.Codex.Models) != 1 {
		t.Fatalf("codex = %+v", cfg.Codex)
	}
	if got := cfg.DNS.Static["api.anthropic.com"]; len(got) != 1 || got[0] != "160.79.104.10" {
		t.Fatalf("dns static = %v", cfg.DNS.Static)
	}
	// Unset fields still come from the defaults.
	if cfg.Upstream != DefaultUpstream || cfg.Codex.BaseURL == "" || !cfg.DNS.Enabled {
		t.Fatalf("defaults were not filled in: %+v", cfg)
	}
}

func TestEmptyYAMLIsAnEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "# nothing yet\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ListenAddrs()) != 0 {
		t.Fatalf("listeners = %v, want none for a file that names none", cfg.ListenAddrs())
	}
	if cfg.Upstream != DefaultUpstream {
		t.Fatalf("upstream = %q", cfg.Upstream)
	}
}

func TestLoadRejectsBrokenYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "listen_tls: [unclosed\n")
	if _, err := Load(path); err == nil {
		t.Fatal("a malformed YAML config must be an error")
	}

	writeFile(t, path, "- a list, not a mapping\n")
	if _, err := Load(path); err == nil {
		t.Fatal("a YAML document that is not a mapping must be an error")
	}
}

func TestSaveYAMLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.ListenHTTP = ""
	cfg.DNS.Servers = []string{"9.9.9.9"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(data)
	if strings.HasPrefix(strings.TrimSpace(text), "{") || !strings.Contains(text, "listen_proxy: ") {
		t.Fatalf("a .yaml file must be written as YAML with the documented keys:\n%s", text)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ListenHTTP != "" || reloaded.ListenProxy != cfg.ListenProxy || reloaded.ListenTLS != cfg.ListenTLS {
		t.Fatalf("listeners did not round-trip: %v", reloaded.ListenerSummary())
	}
	if len(reloaded.DNS.Servers) != 1 || reloaded.DNS.Servers[0] != "9.9.9.9" {
		t.Fatalf("dns servers = %v", reloaded.DNS.Servers)
	}
}

func TestDefaultPathPrefersYAML(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CCMPROXY_HOME", root)

	if got := DefaultPath(); got != filepath.Join(root, "config.yaml") {
		t.Fatalf("DefaultPath() = %q, want config.yaml when nothing exists", got)
	}

	// An existing JSON file is still found, so older installs keep working.
	writeFile(t, filepath.Join(root, "config.json"), `{"listen_tls":"127.1.0.101:443"}`)
	if got := DefaultPath(); got != filepath.Join(root, "config.json") {
		t.Fatalf("DefaultPath() = %q, want the existing config.json", got)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenTLS != "127.1.0.101:443" {
		t.Fatalf("listen_tls = %q, want it read from config.json", cfg.ListenTLS)
	}

	writeFile(t, filepath.Join(root, "config.yml"), "listen_http: 127.0.0.1:9000\n")
	if got := DefaultPath(); got != filepath.Join(root, "config.yml") {
		t.Fatalf("DefaultPath() = %q, want config.yml over config.json", got)
	}

	writeFile(t, filepath.Join(root, "config.yaml"), "listen_proxy: 127.0.0.1:9001\n")
	if got := DefaultPath(); got != filepath.Join(root, "config.yaml") {
		t.Fatalf("DefaultPath() = %q, want config.yaml first", got)
	}
}

func TestOpenRouterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
openrouter:
  provider:
    sort: price
  models:
    - id: z-ai/glm-5.3-flash
      behaves_as: claude-sonnet-5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OpenRouter.Enabled() || cfg.OpenRouter.Models[0].ID != "z-ai/glm-5.3-flash" {
		t.Fatalf("openrouter = %+v", cfg.OpenRouter)
	}
	if cfg.OpenRouter.BaseURL != "https://openrouter.ai/api/v1" || cfg.OpenRouter.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Fatalf("defaults were not filled in: %+v", cfg.OpenRouter)
	}
	t.Setenv("OPENROUTER_API_KEY", " sk-or-env ")
	if got := cfg.OpenRouter.Key(); got != "sk-or-env" {
		t.Fatalf("Key() = %q", got)
	}
	if Default().OpenRouter.Enabled() {
		t.Fatal("OpenRouter must be off until a model is configured")
	}
}

func TestOpenRouterDataCollectionMustBeDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "openrouter:\n  provider:\n    data_collection: allow\n  models:\n    - id: m\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "data_collection") {
		t.Fatalf("err = %v, want a data_collection error", err)
	}

	writeFile(t, path, "openrouter:\n  provider:\n    data_collection: deny\n  models:\n    - id: m\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("deny must be accepted: %v", err)
	}

	writeFile(t, path, "openrouter:\n  models:\n    - display_name: no id\n")
	if _, err := Load(path); err == nil {
		t.Fatal("a model without an id must be rejected")
	}
}
