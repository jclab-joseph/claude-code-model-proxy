package main

import (
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
