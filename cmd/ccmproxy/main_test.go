package main

import (
	"flag"
	"runtime"
	"strings"
	"testing"
)

func TestTrustHintFileName(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skipf("the ca-certificates layout is Linux specific")
	}

	cases := map[string]string{
		// update-ca-certificates only reads files ending in .crt, and an
		// input that already does must not become ca.crt.crt.
		"/tmp/ccmproxy-ca.crt": "/usr/local/share/ca-certificates/ccmproxy-ca.crt ",
		"/tmp/my-ca.pem":       "/usr/local/share/ca-certificates/my-ca.crt ",
		"/tmp/plain-name":      "/usr/local/share/ca-certificates/plain-name.crt ",
	}
	for path, want := range cases {
		got := trustHint(path)
		if !strings.Contains(got, want) {
			t.Fatalf("trustHint(%q) = %q, want it to copy to %q", path, got, strings.TrimSpace(want))
		}
		if strings.Contains(got, ".crt.crt") {
			t.Fatalf("trustHint(%q) doubled the extension: %q", path, got)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		args    []string
		command string
		rest    string
	}{
		{nil, "serve", ""},
		{[]string{"serve", "--log", "debug"}, "serve", "--log debug"},
		// How run.sh starts it: the common flags before the command.
		{[]string{"--config", "/c.yaml", "serve", "--experimental-modify-bootstrap-models"}, "serve", "--config /c.yaml --experimental-modify-bootstrap-models"},
		{[]string{"--config=/c.yaml", "status"}, "status", "--config=/c.yaml"},
		{[]string{"--log", "debug", "--no-tls-listener"}, "serve", "--log debug --no-tls-listener"},
	}
	for _, c := range cases {
		command, rest := splitCommand(c.args, "serve")
		if command != c.command || strings.Join(rest, " ") != c.rest {
			t.Fatalf("splitCommand(%q) = %q, %q; want %q, %q", c.args, command, rest, c.command, c.rest)
		}
	}
}

func TestCommonFlagsAreReadAfterAPositionalArgument(t *testing.T) {
	configPath := t.TempDir() + "/absent.yaml"
	var modify bool
	extra := func(fs *flag.FlagSet) { fs.BoolVar(&modify, "experimental-modify-bootstrap-models", false, "") }

	// A flag after a positional argument used to be left unparsed.
	_, hosts, err := commonFlags("dns", []string{"--config", configPath, "a.example", "--experimental-modify-bootstrap-models", "b.example"}, true, extra)
	if err != nil {
		t.Fatalf("commonFlags: %v", err)
	}
	if !modify || strings.Join(hosts, " ") != "a.example b.example" {
		t.Fatalf("modify = %v, positional = %q", modify, hosts)
	}

	// A command without positional arguments refuses them, so nothing can
	// be dropped without a word.
	if _, _, err := commonFlags("serve", []string{"--config", configPath, "serve", "--log", "debug"}, false, nil); err == nil || !strings.Contains(err.Error(), `unexpected argument "serve"`) {
		t.Fatalf("err = %v, want an unexpected argument error", err)
	}
}
