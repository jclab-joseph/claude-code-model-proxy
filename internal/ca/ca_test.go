package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesMaterialWithoutStoringTheRootKey(t *testing.T) {
	dir := t.TempDir()

	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bundle.IssuedNow() {
		t.Fatal("the first Open must report that it created the material")
	}
	if !bundle.Root().IsCA {
		t.Fatal("the issued root is not a CA")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := map[string]os.FileMode{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", entry.Name(), err)
		}
		names[entry.Name()] = info.Mode().Perm()
	}

	for _, want := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("CA directory holds %v, want %s", names, want)
		}
	}
	if len(names) != 3 {
		t.Fatalf("CA directory holds %v, want exactly the three expected files", names)
	}
	if mode := names["tls.key"]; mode != 0o600 {
		t.Fatalf("tls.key mode = %o, want 600", mode)
	}

	// tls.key is the server key, which has to be stored. A root key must
	// not be, and the only way to tell them apart on disk is by file.
	if _, ok := names["ca.key"]; ok {
		t.Fatal("a root private key was written to disk")
	}
}

func TestStoredRootKeyIsRemovedOnReissue(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(legacy, []byte("-----BEGIN PRIVATE KEY-----\nstale\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}

	if _, err := Open(dir, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("a root key left by an earlier version must be removed")
	}
}

func TestOpenReusesStoredMaterial(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"api.anthropic.com"}

	first, err := Open(dir, hosts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	second, err := Open(dir, hosts)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}

	// This is the point of storing it: a restart must not change the trust
	// anchor a client was told to install.
	if second.IssuedNow() {
		t.Fatalf("the second Open created new material (%s)", second.IssuedReason())
	}
	if !first.Root().Equal(second.Root()) {
		t.Fatal("the root certificate changed across Open calls")
	}
	if string(first.CertPEM()) != string(second.CertPEM()) {
		t.Fatal("the stored certificate bytes changed across Open calls")
	}

	firstLeaf, err := first.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	secondLeaf, err := second.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	if !firstLeaf.Leaf.Equal(secondLeaf.Leaf) {
		t.Fatal("the server certificate changed across Open calls")
	}
}

func TestValidityIsOneYear(t *testing.T) {
	bundle, err := Open(t.TempDir(), []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for name, notAfter := range map[string]time.Time{
		"root":   bundle.Root().NotAfter,
		"server": bundle.leaf.Leaf.NotAfter,
	} {
		remaining := time.Until(notAfter)
		if remaining < 364*24*time.Hour || remaining > 366*24*time.Hour {
			t.Fatalf("%s certificate is valid for %v, want about a year", name, remaining)
		}
	}
	if got := bundle.NotAfter(); got.After(bundle.Root().NotAfter) {
		t.Fatalf("NotAfter() = %v, want the earlier of the two expiries", got)
	}
}

func TestLeafVerifiesAgainstRoot(t *testing.T) {
	bundle, err := Open(t.TempDir(), []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	leaf, err := bundle.Leaf("api.anthropic.com:443")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(bundle.Root())
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Fatalf("the server certificate does not verify for api.anthropic.com: %v", err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: pool}); err == nil {
		t.Fatal("the server certificate unexpectedly verified for an unrelated host")
	}
	assertServedChainOmitsTheRoot(t, leaf, bundle)
}

// assertServedChainOmitsTheRoot checks the invariant that broke Node:
// OpenSSL 3 fails a handshake with SELF_SIGNED_CERT_IN_CHAIN when the server
// presents its own self-signed root, so only the leaf may be sent.
func assertServedChainOmitsTheRoot(t *testing.T, served *tls.Certificate, bundle *Bundle) {
	t.Helper()
	if len(served.Certificate) != 1 {
		t.Fatalf("the served chain has %d certificates, want only the leaf", len(served.Certificate))
	}
	for i, der := range served.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("served chain entry %d could not be parsed: %v", i, err)
		}
		if cert.Equal(bundle.Root()) {
			t.Fatal("the served chain contains the self-signed root; OpenSSL clients reject that")
		}
		if cert.IsCA {
			t.Fatalf("served chain entry %d is a CA certificate", i)
		}
	}
}

func TestServedChainOmitsTheRootAfterReload(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The reload path builds the chain separately from the issue path, so
	// both have to be checked.
	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	if bundle.IssuedNow() {
		t.Fatalf("the stored material was not reused (%s)", bundle.IssuedReason())
	}
	leaf, err := bundle.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	assertServedChainOmitsTheRoot(t, leaf, bundle)
}

func TestOneCertificateCoversEveryInterceptedHost(t *testing.T) {
	hosts := []string{"api.anthropic.com", "api.example.test", "127.0.0.1"}
	bundle, err := Open(t.TempDir(), hosts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, host := range append(hosts, "API.Anthropic.com:443") {
		if _, err := bundle.Leaf(host); err != nil {
			t.Fatalf("Leaf(%q): %v", host, err)
		}
	}
	if _, err := bundle.Leaf("statsig.anthropic.com"); err == nil {
		t.Fatal("a host that was not issued must not get a certificate")
	}
	if _, err := bundle.Leaf(""); err == nil {
		t.Fatal("an empty host must be an error")
	}
}

func TestAddingAHostReissuesTheAuthority(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Without the root key there is nothing to sign an extra name with, so
	// the whole set has to be replaced.
	second, err := Open(dir, []string{"api.anthropic.com", "api.example.test"})
	if err != nil {
		t.Fatalf("Open with an extra host: %v", err)
	}
	if !second.IssuedNow() {
		t.Fatal("adding a host must re-issue the material")
	}
	if !strings.Contains(second.IssuedReason(), "api.example.test") {
		t.Fatalf("reason = %q, want it to name the host that forced the re-issue", second.IssuedReason())
	}
	if first.Root().Equal(second.Root()) {
		t.Fatal("the re-issued root is the same as the old one")
	}
	for _, host := range []string{"api.anthropic.com", "api.example.test"} {
		if _, err := second.Leaf(host); err != nil {
			t.Fatalf("Leaf(%q): %v", host, err)
		}
	}

	// Narrowing the set again reuses what is stored: the certificate still
	// covers everything asked for.
	third, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open with fewer hosts: %v", err)
	}
	if third.IssuedNow() {
		t.Fatalf("removing a host must not re-issue (%s)", third.IssuedReason())
	}
}

func TestDamagedMaterialIsReissued(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"missing server certificate": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "tls.crt")); err != nil {
				t.Fatal(err)
			}
		},
		"missing server key": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "tls.key")); err != nil {
				t.Fatal(err)
			}
		},
		"missing root certificate": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt root certificate": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt server key": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("not a key"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Open(dir, []string{"api.anthropic.com"}); err != nil {
				t.Fatalf("Open: %v", err)
			}
			damage(t, dir)

			bundle, err := Open(dir, []string{"api.anthropic.com"})
			if err != nil {
				t.Fatalf("Open after damage: %v", err)
			}
			if !bundle.IssuedNow() {
				t.Fatal("damaged material must be replaced, not used")
			}
			if _, err := bundle.Leaf("api.anthropic.com"); err != nil {
				t.Fatalf("Leaf: %v", err)
			}
		})
	}
}

func TestMismatchedKeyPairIsReissued(t *testing.T) {
	// A tls.key from a different authority must not be paired with this
	// tls.crt, which is what X509KeyPair is checked for.
	other := t.TempDir()
	if _, err := Open(other, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	foreignKey, err := os.ReadFile(filepath.Join(other, "tls.key"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	dir := t.TempDir()
	if _, err := Open(dir, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), foreignKey, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bundle.IssuedNow() {
		t.Fatal("a key that does not match the certificate must force a re-issue")
	}
}

func TestForeignRootIsReissued(t *testing.T) {
	// A ca.crt the stored tls.crt does not chain to would make every
	// handshake fail verification, so it has to be replaced.
	other := t.TempDir()
	if _, err := Open(other, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	foreignRoot, err := os.ReadFile(filepath.Join(other, "ca.crt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	dir := t.TempDir()
	if _, err := Open(dir, []string{"api.anthropic.com"}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), foreignRoot, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bundle.IssuedNow() {
		t.Fatal("a root the server certificate does not chain to must force a re-issue")
	}
	if !strings.Contains(bundle.IssuedReason(), "chain") {
		t.Fatalf("reason = %q, want it to mention the broken chain", bundle.IssuedReason())
	}
}

func TestGetCertificateFallsBackToTheIssuedHost(t *testing.T) {
	bundle, err := Open(t.TempDir(), []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	get := bundle.GetCertificate("api.anthropic.com")

	withoutSNI, err := get(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate without SNI: %v", err)
	}
	if err := withoutSNI.Leaf.VerifyHostname("api.anthropic.com"); err != nil {
		t.Fatalf("the fallback certificate does not cover the intercepted host: %v", err)
	}

	// An unknown SNI falls back rather than failing the handshake.
	if _, err := get(&tls.ClientHelloInfo{ServerName: "elsewhere.example"}); err != nil {
		t.Fatalf("GetCertificate with unknown SNI: %v", err)
	}
}

func TestOpenRejectsEmptyInput(t *testing.T) {
	if _, err := Open("", []string{"api.anthropic.com"}); err == nil {
		t.Fatal("an empty directory must be rejected")
	}
	if _, err := Open(t.TempDir(), nil); err == nil {
		t.Fatal("opening with no hosts must be rejected")
	}
}

func TestReadCertBeforeAnythingIsCreated(t *testing.T) {
	_, err := ReadCert(t.TempDir())
	if err == nil {
		t.Fatal("reading a certificate that was never created must be an error")
	}
	if !strings.Contains(err.Error(), "ccmproxy") {
		t.Fatalf("error = %v, want it to point at the command that creates one", err)
	}
}

func TestReadCertReturnsWhatOpenWrote(t *testing.T) {
	dir := t.TempDir()
	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	stored, err := ReadCert(dir)
	if err != nil {
		t.Fatalf("ReadCert: %v", err)
	}
	if string(stored) != string(bundle.CertPEM()) {
		t.Fatal("ReadCert returned different bytes than Open wrote")
	}
	if CertPathIn(dir) != bundle.CertPath() {
		t.Fatalf("CertPathIn = %q, CertPath = %q", CertPathIn(dir), bundle.CertPath())
	}
}

func TestKeysAreECDSAP256(t *testing.T) {
	dir := t.TempDir()
	bundle, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for name, cert := range map[string]*x509.Certificate{
		"root":   bundle.Root(),
		"server": bundle.leaf.Leaf,
	} {
		public, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			t.Fatalf("%s public key is %T, want *ecdsa.PublicKey", name, cert.PublicKey)
		}
		if public.Curve != elliptic.P256() {
			t.Fatalf("%s key uses curve %s, want P-256", name, public.Curve.Params().Name)
		}
	}

	// The server certificate is signed by the root, so its signature
	// algorithm reflects the root key type.
	if got := bundle.leaf.Leaf.SignatureAlgorithm; got != x509.ECDSAWithSHA256 {
		t.Fatalf("server certificate signature algorithm = %s, want ECDSA-SHA256", got)
	}
	if got := bundle.Root().SignatureAlgorithm; got != x509.ECDSAWithSHA256 {
		t.Fatalf("root signature algorithm = %s, want ECDSA-SHA256", got)
	}

	// The stored key must round-trip as an EC key.
	reloaded, err := Open(dir, []string{"api.anthropic.com"})
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	if reloaded.IssuedNow() {
		t.Fatalf("the stored EC material was not reused (%s)", reloaded.IssuedReason())
	}
	if _, ok := reloaded.leaf.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("reloaded server key is %T, want *ecdsa.PrivateKey", reloaded.leaf.PrivateKey)
	}
}

func TestDiscardClearsTheRootSecret(t *testing.T) {
	key, _, _, err := newRoot()
	if err != nil {
		t.Fatalf("newRoot: %v", err)
	}
	if key.D.Sign() == 0 {
		t.Fatal("a freshly generated key has no secret scalar")
	}

	discard(key)
	if key.D.Sign() != 0 {
		t.Fatal("discard left the private scalar in place")
	}
	// discard is called on every path, including failures, so nil must be
	// tolerated.
	discard(nil)
}
