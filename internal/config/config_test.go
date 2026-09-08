package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreUsable(t *testing.T) {
	cfg := Default()

	if cfg.Upstream != DefaultUpstream {
		t.Fatalf("upstream = %q", cfg.Upstream)
	}
	if len(cfg.InterceptHosts) != 1 || cfg.InterceptHosts[0] != DefaultInterceptHost {
		t.Fatalf("intercept hosts = %v", cfg.InterceptHosts)
	}
	if !cfg.Intercepts("api.anthropic.com") {
		t.Fatal("the API host must be intercepted by default")
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstream != DefaultUpstream {
		t.Fatalf("upstream = %q, want the default when no file exists", cfg.Upstream)
	}
}

func TestLoadFillsUnsetFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen_proxy":"127.0.0.1:9999","codex":{"model_prefix":"codex"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenProxy != "127.0.0.1:9999" {
		t.Fatalf("listen_proxy = %q", cfg.ListenProxy)
	}
	// Listeners are the exception: a key absent from the file is off, not
	// defaulted. See TestListenersAreOptInOnceAFileExists.
	if cfg.ListenTLS != "" || cfg.ListenHTTP != "" {
		t.Fatalf("unlisted listeners = %q / %q, want both disabled", cfg.ListenTLS, cfg.ListenHTTP)
	}
	if cfg.Upstream != DefaultUpstream {
		t.Fatalf("upstream = %q, want the default filled in", cfg.Upstream)
	}
	if cfg.Codex.BaseURL == "" || cfg.Codex.UserAgent == "" || cfg.Codex.Originator == "" {
		t.Fatalf("codex defaults were not filled in: %+v", cfg.Codex)
	}
	if cfg.Codex.ModelPrefix != "codex/" {
		t.Fatalf("model_prefix = %q, want the separator normalized", cfg.Codex.ModelPrefix)
	}
	if cfg.Codex.CallbackPort != 1455 {
		t.Fatalf("callback_port = %d", cfg.Codex.CallbackPort)
	}
}

func TestLoadRejectsBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{oops`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a malformed config must be an error, not silently ignored")
	}
}

func TestInterceptsIgnoresPortAndCase(t *testing.T) {
	cfg := Default()

	for _, host := range []string{"api.anthropic.com", "api.anthropic.com:443", "API.Anthropic.com:443"} {
		if !cfg.Intercepts(host) {
			t.Fatalf("Intercepts(%q) = false", host)
		}
	}
	for _, host := range []string{"console.anthropic.com", "chatgpt.com:443", ""} {
		if cfg.Intercepts(host) {
			t.Fatalf("Intercepts(%q) = true, want unrelated hosts tunnelled", host)
		}
	}
}

func TestListenersAreOptInOnceAFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen_http":"127.0.0.1:9100"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenHTTP != "127.0.0.1:9100" {
		t.Fatalf("listen_http = %q", cfg.ListenHTTP)
	}
	if cfg.ListenProxy != "" {
		t.Fatalf("listen_proxy = %q, want disabled because the file does not name it", cfg.ListenProxy)
	}
	if cfg.ListenTLS != "" {
		t.Fatalf("listen_tls = %q, want disabled because the file does not name it", cfg.ListenTLS)
	}

	addrs := cfg.ListenAddrs()
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:9100" {
		t.Fatalf("ListenAddrs() = %v, want only the configured listener", addrs)
	}
}

func TestNoListenersAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// A file that names no listener leaves the proxy with nothing to serve;
	// that is the caller's error to report, not something to paper over.
	if err := os.WriteFile(path, []byte(`{"upstream":"https://api.anthropic.com"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ListenAddrs()) != 0 {
		t.Fatalf("ListenAddrs() = %v, want none", cfg.ListenAddrs())
	}
	for _, listener := range cfg.ListenerSummary() {
		if listener[1] != "disabled" {
			t.Fatalf("%s = %q, want disabled", listener[0], listener[1])
		}
	}
}

func TestAnExplicitEmptyListenerIsDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen_proxy":"127.0.0.1:9200","listen_tls":"","listen_http":"   "}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Present-but-empty and absent must behave the same way.
	addrs := cfg.ListenAddrs()
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:9200" {
		t.Fatalf("ListenAddrs() = %v, want only the proxy listener", addrs)
	}
}

func TestMissingFileKeepsEveryListener(t *testing.T) {
	// With no file there is nothing to opt in with, so the proxy has to be
	// usable out of the box.
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ListenAddrs()) != 3 {
		t.Fatalf("ListenAddrs() = %v, want all three defaults", cfg.ListenAddrs())
	}
}

func TestSavedConfigRoundTripsItsListeners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Disable one listener and persist; reloading must not resurrect it.
	cfg.ListenTLS = ""
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ListenTLS != "" {
		t.Fatalf("listen_tls = %q, want it to stay disabled", reloaded.ListenTLS)
	}
	if reloaded.ListenProxy != cfg.ListenProxy || reloaded.ListenHTTP != cfg.ListenHTTP {
		t.Fatalf("the other listeners did not round-trip: %q / %q", reloaded.ListenProxy, reloaded.ListenHTTP)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.InterceptHosts = []string{"api.anthropic.com", "api.example.test"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config mode = %o, want 600", mode)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.InterceptHosts) != 2 || !reloaded.Intercepts("api.example.test") {
		t.Fatalf("intercept hosts did not round-trip: %v", reloaded.InterceptHosts)
	}
}

func TestRootHonoursEnvironment(t *testing.T) {
	t.Setenv("CCMPROXY_HOME", "/tmp/ccmproxy-test")
	if got := Root(); got != "/tmp/ccmproxy-test" {
		t.Fatalf("Root() = %q", got)
	}

	t.Setenv("CCMPROXY_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got := Root(); got != filepath.Join("/tmp/xdg", "ccmproxy") {
		t.Fatalf("Root() = %q", got)
	}
}

func TestDNSDefaults(t *testing.T) {
	cfg := Default()

	if !cfg.DNS.Enabled {
		t.Fatal("the internal resolver must be on by default; the system one honours /etc/hosts and can loop")
	}
	if cfg.DNS.TimeoutMS <= 0 || cfg.DNS.MinTTLSeconds <= 0 || cfg.DNS.MaxTTLSeconds <= 0 {
		t.Fatalf("dns defaults are incomplete: %+v", cfg.DNS)
	}
	if len(cfg.DNS.Servers) != 0 {
		t.Fatalf("dns.servers = %v, want empty so they are discovered", cfg.DNS.Servers)
	}
}

func TestDNSSettingsRoundTripAndFillIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"dns":{"enabled":true,"servers":["9.9.9.9"],"static":{"api.anthropic.com":["203.0.113.1"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DNS.Servers) != 1 || cfg.DNS.Servers[0] != "9.9.9.9" {
		t.Fatalf("dns.servers = %v", cfg.DNS.Servers)
	}
	if got := cfg.DNS.Static["api.anthropic.com"]; len(got) != 1 || got[0] != "203.0.113.1" {
		t.Fatalf("dns.static = %v", cfg.DNS.Static)
	}
	// Timeouts absent from the file must be filled in.
	if cfg.DNS.TimeoutMS != Default().DNS.TimeoutMS {
		t.Fatalf("dns.timeout_ms = %d, want the default", cfg.DNS.TimeoutMS)
	}
}

func TestDNSCanBeDisabledExplicitly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"dns":{"enabled":false}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// An explicit false must survive being loaded over a default of true.
	if cfg.DNS.Enabled {
		t.Fatal("dns.enabled = false in the file must be honoured")
	}
}

func TestListenAddrsCollectsEveryListener(t *testing.T) {
	cfg := Default()
	// Default() is the no-file case, so it carries all three.
	addrs := cfg.ListenAddrs()
	if len(addrs) != 3 {
		t.Fatalf("ListenAddrs() = %v, want all three listeners", addrs)
	}

	cfg.ListenTLS = ""
	cfg.ListenHTTP = "  "
	addrs = cfg.ListenAddrs()
	if len(addrs) != 1 || addrs[0] != cfg.ListenProxy {
		t.Fatalf("ListenAddrs() = %v, want only the configured listener", addrs)
	}
}

func TestCodexAuthDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Codex.UseCLIAuth {
		t.Fatal("an existing Codex CLI login should be usable without configuration")
	}
	if cfg.Codex.AuthFile != "" {
		t.Fatalf("codex.auth_file = %q, want empty so discovery applies", cfg.Codex.AuthFile)
	}
}

func TestExpandUser(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	cases := map[string]string{
		"~":                      home,
		"~/":                     home,
		"~/.config/ccmproxy/ca":  filepath.Join(home, ".config", "ccmproxy", "ca"),
		"  ~/.codex/auth.json  ": filepath.Join(home, ".codex", "auth.json"),
		"/absolute/path":         "/absolute/path",
		"relative/path":          "relative/path",
		"":                       "",
		// Another user's home needs the password database; left alone.
		"~someone/else": "~someone/else",
		// A tilde anywhere but the front is an ordinary character.
		"/tmp/~backup": "/tmp/~backup",
	}
	for input, want := range cases {
		if got := ExpandUser(input); got != want {
			t.Fatalf("ExpandUser(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTildePathsInConfigAreExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
		"listen_http": "127.0.0.1:9000",
		"ca_dir": "~/.config/ccmproxy/ca",
		"auth_dir": "~/.config/ccmproxy/auth",
		"codex": { "auth_file": "~/.codex/auth.json" }
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A literal "~" would be created as a directory next to the working
	// directory, which is the bug this guards against.
	for name, got := range map[string]string{
		"ca_dir":    cfg.CADir,
		"auth_dir":  cfg.AuthDir,
		"auth_file": cfg.Codex.AuthFile,
	} {
		if strings.Contains(got, "~") {
			t.Fatalf("%s = %q, want the tilde expanded", name, got)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("%s = %q, want an absolute path", name, got)
		}
		if !strings.HasPrefix(got, home) {
			t.Fatalf("%s = %q, want it under %q", name, got, home)
		}
	}
	if cfg.CADir != filepath.Join(home, ".config", "ccmproxy", "ca") {
		t.Fatalf("ca_dir = %q", cfg.CADir)
	}
}

func TestTildeInTheConfigPathIsExpanded(t *testing.T) {
	// A quoted --config value reaches the program unexpanded.
	cfg, err := Load("~/definitely-not-a-real-ccmproxy-config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(cfg.Path(), "~") {
		t.Fatalf("Path() = %q, want the tilde expanded", cfg.Path())
	}
}

func TestRootExpandsTildeInEnvironment(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	t.Setenv("CCMPROXY_HOME", "~/ccmproxy-env-test")
	if got := Root(); got != filepath.Join(home, "ccmproxy-env-test") {
		t.Fatalf("Root() = %q", got)
	}

	t.Setenv("CCMPROXY_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "~/xdg-env-test")
	if got := Root(); got != filepath.Join(home, "xdg-env-test", "ccmproxy") {
		t.Fatalf("Root() = %q", got)
	}
}

func TestDefaultsAreAlreadyAbsolute(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, got := range map[string]string{"ca_dir": cfg.CADir, "auth_dir": cfg.AuthDir} {
		if !filepath.IsAbs(got) {
			t.Fatalf("%s = %q, want an absolute path even with no config file", name, got)
		}
	}
}

func TestEffortVariantsAreOffByDefault(t *testing.T) {
	// Claude Code picks effort itself and sends it with the request, so a
	// row per effort would only multiply the picker entries.
	if Default().Codex.EffortVariants {
		t.Fatal("codex.effort_variants must default to false")
	}
}

func TestEffortVariantsCanBeEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen_http":"127.0.0.1:1","codex":{"effort_variants":true}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Codex.EffortVariants {
		t.Fatal("an explicit true must be honoured")
	}
}
