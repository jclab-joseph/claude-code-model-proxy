// Command ccmproxy routes Claude Code's Anthropic API traffic: everything is
// forwarded to the real API untouched except the model list, which is extended
// with OpenAI Codex models, and requests naming one of those models, which are
// answered from a ChatGPT subscription over OAuth.
//
// Claude Code needs no ANTHROPIC_BASE_URL change: the proxy terminates TLS for
// api.anthropic.com using its own local CA.
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/jclab-joseph/claude-code-model-proxy/internal/ca"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/codex"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/config"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/httplog"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/logx"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/proxy"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/resolve"
	"github.com/jclab-joseph/claude-code-model-proxy/internal/router"
)

const usage = `ccmproxy - add models to Claude Code by proxying the Anthropic API

Usage:
  ccmproxy serve [flags]        run the proxy (default command)
      --experimental-modify-bootstrap-models
                                also add the models to Claude Code's startup
                                response, so /model lists them with no
                                settings.json change
  ccmproxy login codex [flags]  sign in to a ChatGPT account for Codex models
  ccmproxy logout codex         remove the stored Codex credentials
  ccmproxy status               show configuration, CA path and login state
  ccmproxy models [--settings-json]
                                list the models this proxy adds;
                                --settings-json prints a modelPicker block
                                to merge into Claude Code's settings.json
  ccmproxy ca [--export path]   print or export the local CA certificate
  ccmproxy env [--shell fish]   print the environment Claude Code needs
  ccmproxy dns HOST...          resolve names with the internal resolver
  ccmproxy config init          write a config file with the current defaults
  ccmproxy version              print the version and build details

Common flags:
  --config PATH   configuration file (default %s)
  --log LEVEL     debug, info, warn or error
`

// version is the release this binary was built from. It is stamped at build
// time with -ldflags "-X main.version=..."; a build without that reports
// "dev", which is what a local `go build` produces.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ccmproxy: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// --version is handled before the command dispatch, which would
	// otherwise read a leading flag as an argument to `serve`.
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version" || args[0] == "-v") {
		return runVersion()
	}

	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	switch command {
	case "serve":
		return runServe(args)
	case "login":
		return runLogin(args)
	case "logout":
		return runLogout(args)
	case "status":
		return runStatus(args)
	case "models":
		return runModels(args)
	case "ca":
		return runCA(args)
	case "env":
		return runEnv(args)
	case "dns":
		return runDNS(args)
	case "config":
		return runConfig(args)
	case "version":
		return runVersion()
	case "help", "-h", "--help":
		fmt.Printf(usage, config.DefaultPath())
		return nil
	default:
		return fmt.Errorf("unknown command %q (try `ccmproxy help`)", command)
	}
}

// commonFlags registers the flags every subcommand accepts and returns the
// loaded configuration.
func commonFlags(name string, args []string, extra func(*flag.FlagSet)) (*config.Config, *flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := fs.String("config", "", "configuration file")
	logLevel := fs.String("log", "", "log level: debug, info, warn, error")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, nil, err
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	logx.SetLevel(cfg.LogLevel)
	return cfg, fs, nil
}

// resolverOrNil keeps a nil *resolve.Resolver from becoming a non-nil dialer
// interface, which would then be called and panic.
func resolverOrNil(resolver *resolve.Resolver) dialer {
	if resolver == nil {
		return nil
	}
	return resolver
}

// storeOptions maps the configuration onto the credential lookup order.
func storeOptions(cfg *config.Config) codex.StoreOptions {
	return codex.StoreOptions{
		Dir:          cfg.AuthDir,
		ExplicitPath: cfg.Codex.AuthFile,
		UseCodexCLI:  cfg.Codex.UseCLIAuth,
	}
}

// dialer is what every outbound connection is made with.
type dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// newResolver builds the proxy's own DNS client from the configuration, or
// returns nil when the internal resolver is switched off.
func newResolver(cfg *config.Config) (*resolve.Resolver, error) {
	if !cfg.DNS.Enabled {
		return nil, nil
	}
	return resolve.New(resolve.Config{
		Servers: cfg.DNS.Servers,
		Timeout: time.Duration(cfg.DNS.TimeoutMS) * time.Millisecond,
		Static:  cfg.DNS.Static,
		MinTTL:  time.Duration(cfg.DNS.MinTTLSeconds) * time.Second,
		MaxTTL:  time.Duration(cfg.DNS.MaxTTLSeconds) * time.Second,
		// Refusing the proxy's own listeners turns a hosts-file loop into a
		// clear error instead of a hang.
		Deny:        cfg.ListenAddrs(),
		DialTimeout: 30 * time.Second,
	})
}

// directTransport dials upstreams with the given dialer and without consulting
// the proxy environment variables, which would otherwise point the proxy at
// itself.
func directTransport(dial dialer) *http.Transport {
	if dial == nil {
		dial = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func runServe(args []string) error {
	var noProxy, noTLS, noHTTP, modifyBootstrap bool
	cfg, _, err := commonFlags("serve", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&noProxy, "no-proxy-listener", false, "do not start the HTTP CONNECT proxy")
		fs.BoolVar(&noTLS, "no-tls-listener", false, "do not start the direct TLS listener")
		fs.BoolVar(&noHTTP, "no-http-listener", false, "do not start the plain HTTP listener")
		fs.BoolVar(&modifyBootstrap, "experimental-modify-bootstrap-models", false,
			"add the models to Claude Code's startup response so they appear in /model "+
				"without editing settings.json (rewrites an undocumented response)")
	})
	if err != nil {
		return err
	}

	// Folding the flags into the configuration keeps one source of truth for
	// which listeners exist, which the resolver's deny list also reads.
	if noProxy {
		cfg.ListenProxy = ""
	}
	if noTLS {
		cfg.ListenTLS = ""
	}
	if noHTTP {
		cfg.ListenHTTP = ""
	}
	cfg.ExperimentalModifyBootstrapModels = modifyBootstrap

	// Stored material is reused so the trust anchor survives restarts; the
	// root key is discarded whenever new material has to be created.
	authority, err := ca.Open(cfg.CADir, cfg.InterceptHosts)
	if err != nil {
		return err
	}

	resolver, err := newResolver(cfg)
	if err != nil {
		return err
	}
	// Every outbound call the proxy makes — to Anthropic, to the Codex
	// backend, to the OAuth token endpoint — goes through this one
	// transport, so wrapping it here logs all of them with one change.
	transport := &httplog.Transport{Next: directTransport(resolverOrNil(resolver))}

	oauth := codex.NewClient(&http.Client{Transport: transport, Timeout: 30 * time.Second})
	store := codex.NewStore(oauth, storeOptions(cfg))
	backend := codex.NewBackend(cfg.Codex.BaseURL, cfg.Codex.UserAgent, cfg.Codex.Originator, store, transport)
	registry := codex.NewRegistry(cfg.Codex.Models, cfg.Codex.ModelPrefix, cfg.Codex.EffortVariants)

	handler, err := router.New(cfg, registry, backend, transport, authority.CertPEM())
	if err != nil {
		return err
	}
	// Every inbound request — passthrough, Codex, model list, admin —
	// reaches the router through this handler, so wrapping it here logs
	// all of them regardless of which listener they arrived on.
	loggedHandler := httplog.Log(handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logx.Infof("CA certificate: %s (valid until %s)",
		authority.CertPath(), authority.NotAfter().Format(time.RFC3339))
	if authority.IssuedNow() {
		logx.Warnf("a new certificate authority was created (%s)", authority.IssuedReason())
		logx.Warnf("clients must trust the new %s; restart Claude Code so it re-reads NODE_EXTRA_CA_CERTS", authority.CertPath())
	}
	logx.Infof("upstream: %s", cfg.Upstream)
	logx.Infof("intercepting: %s", strings.Join(cfg.InterceptHosts, ", "))
	if cfg.ExperimentalModifyBootstrapModels {
		logx.Warnf("experimental: rewriting %s to add %d models to the /model picker",
			"/api/claude_cli/bootstrap", len(registry.BootstrapOptions()))
	}
	if resolver != nil {
		logx.Infof("dns: resolving upstream names with %s", strings.Join(resolver.Servers(), ", "))
	} else {
		logx.Warnf("dns: internal resolver disabled; a hosts-file redirect of an intercepted host will loop")
	}
	if store.Authenticated() {
		if token, err := store.Peek(); err == nil {
			logx.Infof("codex account: %s (%s)", accountLabel(token), planLabel(token))
		}
	} else {
		logx.Warnf("no Codex credentials yet; run `ccmproxy login codex` to enable the added models")
	}

	for _, listener := range cfg.ListenerSummary() {
		if listener[1] == "disabled" {
			logx.Infof("%s disabled", listener[0])
		}
	}

	errs := make(chan error, 3)
	started := 0

	if cfg.ListenProxy != "" {
		started++
		connectProxy := proxy.NewServer(cfg, authority, loggedHandler, transport, resolverOrNil(resolver))
		go func() {
			errs <- proxy.Serve(ctx, "proxy", cfg.ListenProxy, connectProxy, nil, "")
		}()
	}
	if cfg.ListenTLS != "" {
		started++
		go func() {
			errs <- proxy.Serve(ctx, "tls", cfg.ListenTLS, loggedHandler, authority, cfg.PrimaryInterceptHost())
		}()
	}
	if cfg.ListenHTTP != "" {
		started++
		go func() {
			errs <- proxy.Serve(ctx, "http", cfg.ListenHTTP, loggedHandler, nil, "")
		}()
	}
	if started == 0 {
		return fmt.Errorf("no listener is enabled: set listen_proxy, listen_tls or listen_http in %s "+
			"(a key that is absent from the file disables its listener)", cfg.Path())
	}

	if cfg.ListenProxy != "" {
		fmt.Println()
		fmt.Println("Point Claude Code at the proxy with:")
		fmt.Println()
		printEnv(cfg, authority.CertPath(), "posix")
		fmt.Println()
	}

	// The first listener to fail brings the process down, so a half-running
	// proxy never looks healthy.
	for i := 0; i < started; i++ {
		if err := <-errs; err != nil {
			stop()
			return err
		}
	}
	return nil
}

func runLogin(args []string) error {
	provider := "codex"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider, args = args[0], args[1:]
	}
	if provider != "codex" {
		return fmt.Errorf("unknown provider %q; only `codex` is supported", provider)
	}

	var noBrowser bool
	cfg, _, err := commonFlags("login", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&noBrowser, "no-browser", false, "print the URL instead of opening a browser")
	})
	if err != nil {
		return err
	}

	resolver, err := newResolver(cfg)
	if err != nil {
		return err
	}
	oauth := codex.NewClient(&http.Client{
		Transport: directTransport(resolverOrNil(resolver)),
		Timeout:   30 * time.Second,
	})
	store := codex.NewStore(oauth, storeOptions(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	announce := func(url string) {
		if noBrowser {
			fmt.Println("Open this URL to authorize the proxy:")
		} else {
			fmt.Println("Opening your browser. If nothing happens, open this URL:")
		}
		fmt.Println()
		fmt.Println("  " + url)
		fmt.Println()
		fmt.Printf("Waiting for the callback on %s ...\n", codex.RedirectURI(cfg.Codex.CallbackPort))
	}

	result, err := oauth.Login(ctx, cfg.Codex.CallbackPort, !noBrowser, announce)
	if err != nil {
		return err
	}
	if err := store.Save(result.Token); err != nil {
		return err
	}

	fmt.Printf("Signed in as %s (%s).\n", accountLabel(result.Token), planLabel(result.Token))
	fmt.Printf("Credentials stored in %s\n", store.NativePath())
	return nil
}

func runLogout(args []string) error {
	provider := "codex"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider, args = args[0], args[1:]
	}
	if provider != "codex" {
		return fmt.Errorf("unknown provider %q; only `codex` is supported", provider)
	}

	cfg, _, err := commonFlags("logout", args, nil)
	if err != nil {
		return err
	}
	store := codex.NewStore(nil, storeOptions(cfg))
	if err := os.Remove(store.NativePath()); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No Codex credentials were stored.")
			return nil
		}
		return err
	}
	fmt.Printf("Removed %s\n", store.NativePath())
	if path := codex.CodexCLIAuthPath(); cfg.Codex.UseCLIAuth && path != "" {
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("The Codex CLI login at %s is untouched and will still be used.\n", path)
		}
	}
	return nil
}

func runStatus(args []string) error {
	cfg, _, err := commonFlags("status", args, nil)
	if err != nil {
		return err
	}

	fmt.Printf("config          %s\n", cfg.Path())
	fmt.Printf("upstream        %s\n", cfg.Upstream)
	fmt.Printf("intercepting    %s\n", strings.Join(cfg.InterceptHosts, ", "))
	for _, listener := range cfg.ListenerSummary() {
		fmt.Printf("%-15s %s\n", listener[0], listener[1])
	}
	fmt.Printf("ca directory    %s\n", cfg.CADir)
	if certPEM, err := ca.ReadCert(cfg.CADir); err != nil {
		fmt.Printf("ca              not created yet\n")
	} else if block, _ := pem.Decode(certPEM); block == nil {
		fmt.Printf("ca              %s is not a PEM certificate\n", ca.CertPathIn(cfg.CADir))
	} else if cert, err := x509.ParseCertificate(block.Bytes); err != nil {
		fmt.Printf("ca              %s could not be parsed\n", ca.CertPathIn(cfg.CADir))
	} else {
		fmt.Printf("ca              valid until %s\n", cert.NotAfter.Format(time.RFC3339))
	}
	if resolver, err := newResolver(cfg); err != nil {
		fmt.Printf("dns             misconfigured: %v\n", err)
	} else if resolver == nil {
		fmt.Printf("dns             disabled (the operating system resolver is used)\n")
	} else {
		fmt.Printf("dns             %s\n", strings.Join(resolver.Servers(), ", "))
	}

	store := codex.NewStore(nil, storeOptions(cfg))
	fmt.Printf("codex sources   %s\n", strings.Join(store.Sources(), ", "))
	token, err := store.Peek()
	if err != nil {
		fmt.Printf("codex           not signed in\n")
		return nil
	}
	fmt.Printf("codex           %s (%s)\n", accountLabel(token), planLabel(token))
	fmt.Printf("codex file      %s\n", store.Path())
	if !token.ExpiresAt.IsZero() {
		fmt.Printf("codex token     expires %s\n", token.ExpiresAt.Local().Format(time.RFC3339))
	}
	return nil
}

func runModels(args []string) error {
	var settingsJSON, bootstrapJSON bool
	cfg, _, err := commonFlags("models", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&settingsJSON, "settings-json", false,
			"print a modelPicker block to merge into Claude Code's settings.json")
		fs.BoolVar(&bootstrapJSON, "bootstrap-json", false,
			"print the additional_model_options this proxy injects into the bootstrap response")
	})
	if err != nil {
		return err
	}
	registry := codex.NewRegistry(cfg.Codex.Models, cfg.Codex.ModelPrefix, cfg.Codex.EffortVariants)

	switch {
	case settingsJSON:
		return printSettingsJSON(registry)
	case bootstrapJSON:
		return printJSON(map[string]any{"additional_model_options": registry.BootstrapOptions()})
	}

	for _, row := range registry.PickerOptions() {
		resolution, ok := registry.Resolve(row.Model)
		if !ok {
			continue
		}
		effort := resolution.Effort
		if effort == "" {
			effort = resolution.Model.DefaultEffort + " (default)"
		}
		fmt.Printf("%-28s %-24s context %7d  effort %-16s behaves as %s\n",
			row.Model, resolution.Model.ID, resolution.Model.ContextLength, effort, row.BehavesAs)
	}
	return nil
}

// printSettingsJSON emits the settings.json fragment that puts the added
// models in Claude Code's /model picker.
//
// This is the client-side half of the integration and needs no cooperation
// from the proxy: Claude Code's picker is built from its own catalog plus
// `modelPicker.options`, and never from the /v1/models endpoint.
func printSettingsJSON(registry *codex.Registry) error {
	block := map[string]any{
		"modelPicker": map[string]any{
			"options": registry.PickerOptions(),
		},
	}
	if err := printJSON(block); err != nil {
		return err
	}

	settingsPath := "~/.claude/settings.json"
	fmt.Fprintf(os.Stderr, "\n# Merge the block above into %s, for example:\n", settingsPath)
	fmt.Fprintf(os.Stderr, "#   ccmproxy models --settings-json > /tmp/picker.json\n")
	fmt.Fprintf(os.Stderr, "#   jq -s '.[0] * .[1]' %s /tmp/picker.json > /tmp/merged.json\n", settingsPath)
	fmt.Fprintf(os.Stderr, "#   mv /tmp/merged.json %s\n", settingsPath)
	fmt.Fprintf(os.Stderr, "# Restart Claude Code, then open /model.\n")
	fmt.Fprintf(os.Stderr, "# Reasoning effort is Claude Code's own control: /effort or --effort.\n")
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}

func runCA(args []string) error {
	var export string
	cfg, _, err := commonFlags("ca", args, func(fs *flag.FlagSet) {
		fs.StringVar(&export, "export", "", "write the certificate to this path")
	})
	if err != nil {
		return err
	}

	// Stored material is reused, so creating it here is safe: a running
	// proxy loaded the same files and keeps serving the same certificate.
	authority, err := ca.Open(cfg.CADir, cfg.InterceptHosts)
	if err != nil {
		return err
	}
	certPEM := authority.CertPEM()
	if export == "" {
		fmt.Printf("%s\n", authority.CertPath())
		fmt.Printf("valid until %s for %s\n",
			authority.NotAfter().Format(time.RFC3339), strings.Join(authority.Hosts(), ", "))
		fmt.Print(string(certPEM))
		return nil
	}
	export = config.ExpandUser(export)
	if err := os.WriteFile(export, certPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", export)
	fmt.Println("Trust it for Claude Code with NODE_EXTRA_CA_CERTS, or install it system-wide:")
	fmt.Println("  " + trustHint(export))
	fmt.Printf("It is valid until %s. A copy goes stale when the authority is\n",
		authority.NotAfter().Format(time.RFC3339))
	fmt.Println("re-issued, which happens on expiry or when intercept_hosts grows.")
	return nil
}

func runEnv(args []string) error {
	var shell string
	cfg, _, err := commonFlags("env", args, func(fs *flag.FlagSet) {
		fs.StringVar(&shell, "shell", "posix", "posix, fish or powershell")
	})
	if err != nil {
		return err
	}
	// Only the path is needed, which exists independently of whether a
	// proxy is currently running.
	printEnv(cfg, ca.CertPathIn(cfg.CADir), shell)
	return nil
}

// runDNS resolves names the way the proxy does, which is how to confirm that
// a hosts-file entry is being bypassed.
func runDNS(args []string) error {
	cfg, fs, err := commonFlags("dns", args, nil)
	if err != nil {
		return err
	}
	hosts := fs.Args()
	if len(hosts) == 0 {
		hosts = []string{strings.TrimPrefix(strings.TrimPrefix(cfg.Upstream, "https://"), "http://")}
	}

	resolver, err := newResolver(cfg)
	if err != nil {
		return err
	}
	if resolver == nil {
		return errors.New("the internal resolver is disabled in the configuration (dns.enabled)")
	}
	fmt.Printf("nameservers %s\n", strings.Join(resolver.Servers(), ", "))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var failed bool
	for _, host := range hosts {
		host = strings.TrimSuffix(host, "/")
		if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			host = h
		}
		addrs, err := resolver.LookupIP(ctx, host)
		if err != nil {
			failed = true
			fmt.Printf("%-28s %v\n", host, err)
			continue
		}
		formatted := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			formatted = append(formatted, addr.String())
		}
		fmt.Printf("%-28s %s\n", host, strings.Join(formatted, ", "))
	}
	if failed {
		return errors.New("one or more names could not be resolved")
	}
	return nil
}

// runVersion reports the build, which matters when a released binary is
// being compared against a local one.
func runVersion() error {
	fmt.Printf("ccmproxy %s\n", version)
	fmt.Printf("  go       %s\n", runtime.Version())
	fmt.Printf("  platform %s/%s\n", runtime.GOOS, runtime.GOARCH)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				fmt.Printf("  revision %s\n", setting.Value)
			case "vcs.time":
				fmt.Printf("  built    %s\n", setting.Value)
			}
		}
	}
	return nil
}

func runConfig(args []string) error {
	action := "init"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if action != "init" {
		return fmt.Errorf("unknown config action %q; only `init` is supported", action)
	}

	cfg, _, err := commonFlags("config", args, nil)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.Path()); err == nil {
		return fmt.Errorf("%s already exists", cfg.Path())
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", cfg.Path())
	return nil
}

// printEnv emits the environment Claude Code needs: the proxy address and the
// trust anchor for the intercepted TLS.
func printEnv(cfg *config.Config, certPath string, shell string) {
	proxyURL := "http://" + cfg.ListenProxy
	if cfg.ProxyUsername != "" && cfg.ProxyPassword != "" {
		proxyURL = fmt.Sprintf("http://%s:%s@%s", cfg.ProxyUsername, cfg.ProxyPassword, cfg.ListenProxy)
	}
	vars := [][2]string{
		{"HTTPS_PROXY", proxyURL},
		{"HTTP_PROXY", proxyURL},
		{"NODE_EXTRA_CA_CERTS", certPath},
	}

	switch strings.ToLower(shell) {
	case "fish":
		for _, kv := range vars {
			fmt.Printf("set -gx %s %s\n", kv[0], kv[1])
		}
	case "powershell", "pwsh":
		for _, kv := range vars {
			fmt.Printf("$env:%s = \"%s\"\n", kv[0], kv[1])
		}
	default:
		for _, kv := range vars {
			fmt.Printf("export %s=%s\n", kv[0], kv[1])
		}
	}
}

// trustHint suggests the platform's system trust store command.
func trustHint(path string) string {
	switch runtime.GOOS {
	case "darwin":
		return "sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain " + path
	case "windows":
		return "certutil -addstore -f Root " + path
	default:
		return fmt.Sprintf("sudo cp %s /usr/local/share/ca-certificates/%s && sudo update-ca-certificates",
			path, filepath.Base(strings.TrimSuffix(path, ".pem"))+".crt")
	}
}

func accountLabel(token *codex.Token) string {
	if token == nil {
		return "unknown account"
	}
	if token.Email != "" {
		return token.Email
	}
	if token.AccountID != "" {
		return token.AccountID
	}
	return "unknown account"
}

func planLabel(token *codex.Token) string {
	if token == nil || token.PlanType == "" {
		return "plan unknown"
	}
	return token.PlanType + " plan"
}
