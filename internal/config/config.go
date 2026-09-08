// Package config holds the on-disk configuration for the proxy along with the
// defaults that make it usable with no configuration at all.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultUpstream is the Anthropic API endpoint every non-intercepted request
// is forwarded to verbatim.
const DefaultUpstream = "https://api.anthropic.com"

// DefaultInterceptHost is the hostname the proxy terminates TLS for.
const DefaultInterceptHost = "api.anthropic.com"

// Codex groups the settings of the OpenAI Codex (ChatGPT subscription) backend.
type Codex struct {
	// BaseURL is the ChatGPT backend that serves the Responses API.
	BaseURL string `json:"base_url"`
	// UserAgent and Originator identify the request as coming from the Codex
	// CLI, which is the client the subscription endpoint expects.
	UserAgent  string `json:"user_agent"`
	Originator string `json:"originator"`
	// Models restricts the exposed catalog to these ids when non-empty.
	Models []string `json:"models,omitempty"`
	// EffortVariants adds one model entry per supported reasoning level
	// (for example gpt-5.5-high) to the advertised model list.
	EffortVariants bool `json:"effort_variants"`
	// ModelPrefix, when set, is accepted (and advertised) in front of every
	// Codex model id, e.g. "codex/gpt-5.5".
	ModelPrefix string `json:"model_prefix,omitempty"`
	// CallbackPort is the loopback port the OAuth redirect URI points at. The
	// value is fixed by the registered OAuth client and should not change.
	CallbackPort int `json:"callback_port"`
	// AuthFile, when set, is the only credential file consulted, in either
	// this proxy's layout or the Codex CLI's.
	AuthFile string `json:"auth_file,omitempty"`
	// UseCLIAuth reads (and keeps in sync) the Codex CLI's
	// ~/.codex/auth.json when this proxy has no credentials of its own.
	UseCLIAuth bool `json:"use_codex_cli_auth"`
}

// DNS configures the proxy's own resolver, which is used for every outbound
// connection.
//
// The proxy resolves upstream names itself so it is not subject to the
// redirection it installs: in the hosts-file deployment api.anthropic.com
// points at this proxy, and the system resolver would send it back to itself.
type DNS struct {
	// Enabled turns the internal resolver on. With it off, outbound
	// connections go through the operating system resolver, which honours
	// /etc/hosts and can therefore loop.
	Enabled bool `json:"enabled"`
	// Servers are the nameservers to query, as host or host:port. When empty
	// they come from /etc/resolv.conf, skipping loopback stub resolvers
	// (systemd-resolved, dnsmasq) because those consult /etc/hosts as well.
	Servers []string `json:"servers,omitempty"`
	// TimeoutMS bounds a single DNS query.
	TimeoutMS int `json:"timeout_ms"`
	// Static pins hostnames to IP addresses, answered without any query.
	Static map[string][]string `json:"static,omitempty"`
	// MinTTLSeconds and MaxTTLSeconds clamp how long an answer is cached.
	MinTTLSeconds int `json:"min_ttl_seconds"`
	MaxTTLSeconds int `json:"max_ttl_seconds"`
}

// listenerKeys are the configuration keys that open a listener. A key absent
// from an existing configuration file disables its listener; see Load.
var listenerKeys = []string{"listen_proxy", "listen_tls", "listen_http"}

// Config is the complete proxy configuration.
//
// The three listen fields are opt-in once a configuration file exists: an
// empty value means that listener is not started.
type Config struct {
	// ListenProxy serves the HTTP CONNECT proxy Claude Code is pointed at.
	// Empty disables it.
	ListenProxy string `json:"listen_proxy"`
	// ListenTLS serves the same router directly over TLS using a certificate
	// for the intercepted host, for hosts-file style redirection. Empty
	// disables it.
	ListenTLS string `json:"listen_tls"`
	// ListenHTTP serves the router over plain HTTP, which is handy for local
	// testing and for ANTHROPIC_BASE_URL setups. Empty disables it.
	ListenHTTP string `json:"listen_http"`

	// Upstream is where pass-through traffic goes.
	Upstream string `json:"upstream"`
	// InterceptHosts are the CONNECT targets whose TLS is terminated locally.
	// Everything else is tunnelled untouched.
	InterceptHosts []string `json:"intercept_hosts"`

	// ProxyUsername and ProxyPassword, when both set, require Basic
	// credentials on the CONNECT proxy.
	ProxyUsername string `json:"proxy_username,omitempty"`
	ProxyPassword string `json:"proxy_password,omitempty"`

	// CADir stores the local CA, AuthDir the provider credentials.
	CADir   string `json:"ca_dir"`
	AuthDir string `json:"auth_dir"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"log_level"`

	// ExperimentalModifyBootstrapModels makes the proxy add its models to
	// Claude Code's startup response, so they appear in the /model picker
	// with no client-side configuration.
	//
	// It is off by default and enabled only by the
	// --experimental-modify-bootstrap-models flag: it rewrites an
	// undocumented Claude Code response rather than a published Anthropic
	// API one, and the shape can change with any Claude Code release.
	ExperimentalModifyBootstrapModels bool `json:"-"`

	Codex Codex `json:"codex"`
	DNS   DNS   `json:"dns"`

	// path records where the config was loaded from, for Save.
	path string `json:"-"`
}

// ExpandUser resolves a leading "~" to the user's home directory.
//
// Tilde expansion is a shell feature, not a filesystem one: a shell rewrites
// "~/x" before the program is ever started. A path that arrives from a
// configuration file passes through no shell, so without this it would be used
// verbatim as a relative path and create a directory literally named "~" next
// to the working directory.
//
// Only the current user's home is expanded. "~other/x" is left alone, since
// resolving another user's home would need the password database and is not
// something this proxy should be guessing at.
func ExpandUser(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path[0] != '~' {
		return path
	}
	if len(path) > 1 && path[1] != '/' && path[1] != filepath.Separator {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		// Better to leave the path as written than to silently relocate it.
		return path
	}
	if len(path) == 1 {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Root reports the configuration directory, honouring XDG_CONFIG_HOME.
func Root() string {
	if dir := strings.TrimSpace(os.Getenv("CCMPROXY_HOME")); dir != "" {
		return ExpandUser(dir)
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(ExpandUser(dir), "ccmproxy")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ccmproxy"
	}
	return filepath.Join(home, ".config", "ccmproxy")
}

// DefaultPath reports the default config file location.
func DefaultPath() string { return filepath.Join(Root(), "config.json") }

// Default returns a fully populated configuration.
func Default() *Config {
	root := Root()
	return &Config{
		ListenProxy:    "127.0.0.1:8080",
		ListenTLS:      "127.0.0.1:8443",
		ListenHTTP:     "127.0.0.1:8081",
		Upstream:       DefaultUpstream,
		InterceptHosts: []string{DefaultInterceptHost},
		CADir:          filepath.Join(root, "ca"),
		AuthDir:        filepath.Join(root, "auth"),
		LogLevel:       "info",
		Codex: Codex{
			BaseURL:        "https://chatgpt.com/backend-api/codex",
			UserAgent:      "codex-tui/0.153.3 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.153.3)",
			Originator:     "codex-tui",
			EffortVariants: true,
			CallbackPort:   1455,
			UseCLIAuth:     true,
		},
		DNS: DNS{
			Enabled:       true,
			TimeoutMS:     5000,
			MinTTLSeconds: 30,
			MaxTTLSeconds: 3600,
		},
	}
}

// Load reads path, filling unset fields from Default. A missing file is not an
// error: the defaults are returned so the proxy runs out of the box.
//
// Listeners are the one exception to filling in defaults. Once a configuration
// file exists, only the listeners it names are started: a key that is absent
// disables its listener rather than inheriting the default address. Without a
// file, all three defaults apply.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath()
	}
	path = ExpandUser(path)

	cfg := Default()
	cfg.path = path

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.normalize()
			return cfg, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Which top-level keys the file actually sets cannot be recovered from
	// the decoded struct, so the raw object is inspected for them.
	var present map[string]json.RawMessage
	if err := json.Unmarshal(data, &present); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.disableUnlistedListeners(present)

	cfg.normalize()
	return cfg, nil
}

// disableUnlistedListeners clears every listener the file did not mention.
func (c *Config) disableUnlistedListeners(present map[string]json.RawMessage) {
	for _, key := range listenerKeys {
		if _, ok := present[key]; ok {
			continue
		}
		switch key {
		case "listen_proxy":
			c.ListenProxy = ""
		case "listen_tls":
			c.ListenTLS = ""
		case "listen_http":
			c.ListenHTTP = ""
		}
	}
}

func (c *Config) normalize() {
	def := Default()
	if strings.TrimSpace(c.Upstream) == "" {
		c.Upstream = def.Upstream
	}
	c.Upstream = strings.TrimSuffix(strings.TrimSpace(c.Upstream), "/")
	if len(c.InterceptHosts) == 0 {
		c.InterceptHosts = def.InterceptHosts
	}
	for i, host := range c.InterceptHosts {
		c.InterceptHosts[i] = strings.ToLower(strings.TrimSpace(host))
	}
	if strings.TrimSpace(c.CADir) == "" {
		c.CADir = def.CADir
	}
	if strings.TrimSpace(c.AuthDir) == "" {
		c.AuthDir = def.AuthDir
	}
	// Path-valued settings come from a file, so "~" has not been expanded
	// by a shell and has to be resolved here.
	c.CADir = ExpandUser(c.CADir)
	c.AuthDir = ExpandUser(c.AuthDir)
	c.Codex.AuthFile = ExpandUser(c.Codex.AuthFile)
	if strings.TrimSpace(c.LogLevel) == "" {
		c.LogLevel = def.LogLevel
	}
	if strings.TrimSpace(c.Codex.BaseURL) == "" {
		c.Codex.BaseURL = def.Codex.BaseURL
	}
	c.Codex.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.Codex.BaseURL), "/")
	if strings.TrimSpace(c.Codex.UserAgent) == "" {
		c.Codex.UserAgent = def.Codex.UserAgent
	}
	if strings.TrimSpace(c.Codex.Originator) == "" {
		c.Codex.Originator = def.Codex.Originator
	}
	if c.Codex.CallbackPort <= 0 {
		c.Codex.CallbackPort = def.Codex.CallbackPort
	}
	if prefix := strings.TrimSpace(c.Codex.ModelPrefix); prefix != "" {
		c.Codex.ModelPrefix = strings.TrimSuffix(prefix, "/") + "/"
	}
	if c.DNS.TimeoutMS <= 0 {
		c.DNS.TimeoutMS = def.DNS.TimeoutMS
	}
	if c.DNS.MinTTLSeconds <= 0 {
		c.DNS.MinTTLSeconds = def.DNS.MinTTLSeconds
	}
	if c.DNS.MaxTTLSeconds <= 0 {
		c.DNS.MaxTTLSeconds = def.DNS.MaxTTLSeconds
	}
}

// ListenAddrs reports every address the proxy listens on. The resolver refuses
// to dial these, so a hosts-file redirect cannot make the proxy call itself.
func (c *Config) ListenAddrs() []string {
	addrs := make([]string, 0, 3)
	for _, addr := range []string{c.ListenProxy, c.ListenTLS, c.ListenHTTP} {
		if strings.TrimSpace(addr) != "" {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// listenerLabel renders a listen address for display.
func listenerLabel(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return "disabled"
	}
	return addr
}

// ListenerSummary describes each listener for reporting.
func (c *Config) ListenerSummary() [][2]string {
	return [][2]string{
		{"proxy listener", listenerLabel(c.ListenProxy)},
		{"tls listener", listenerLabel(c.ListenTLS)},
		{"http listener", listenerLabel(c.ListenHTTP)},
	}
}

// Path reports where the config was loaded from.
func (c *Config) Path() string {
	if c.path == "" {
		return DefaultPath()
	}
	return c.path
}

// Save writes the configuration back to disk, creating parent directories.
func (c *Config) Save() error {
	path := c.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// Intercepts reports whether TLS for host should be terminated locally.
func (c *Config) Intercepts(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	for _, candidate := range c.InterceptHosts {
		if candidate == host {
			return true
		}
	}
	return false
}

// PrimaryInterceptHost reports the host used when a client omits SNI.
func (c *Config) PrimaryInterceptHost() string {
	if len(c.InterceptHosts) > 0 {
		return c.InterceptHosts[0]
	}
	return DefaultInterceptHost
}
