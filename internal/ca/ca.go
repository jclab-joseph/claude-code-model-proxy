// Package ca manages the certificate authority used to terminate TLS for
// intercepted hosts.
//
// The material is generated once and then reused, so the trust anchor a client
// was told to trust keeps working across restarts. Three files are kept:
//
//	ca.crt   the root certificate — the trust anchor clients install
//	tls.crt  the server certificate for the intercepted hosts
//	tls.key  its private key, mode 0600
//
// The root's private key is deliberately not among them. It is generated in
// memory, used once to sign ca.crt and tls.crt, and discarded before the proxy
// serves anything. Nothing on disk can therefore mint a certificate for a host
// that is not already in tls.crt: a leaked tls.key impersonates exactly the
// intercepted hosts, where a leaked CA key would have impersonated anything.
//
// The cost of not keeping the root key is that new material — a new trust
// anchor — is needed whenever the certificates expire or the set of intercepted
// hosts grows, and clients have to trust the new ca.crt at that point.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	rootCommonName = "claude-code-model-proxy local CA"
	// validity is how long both certificates are good for.
	validity = 365 * 24 * time.Hour
	// renewBefore re-issues while the certificates are still usable, so a
	// client is never handed one that expires mid-session.
	renewBefore = 30 * 24 * time.Hour
	// backdate absorbs small clock skew between the proxy and its clients.
	backdate = 1 * time.Hour

	rootCertFileName = "ca.crt"
	leafCertFileName = "tls.crt"
	leafKeyFileName  = "tls.key"
	// legacyRootKeyFileName is removed on sight: an early version stored the
	// root key, and leaving one behind would be a live CA key on disk.
	legacyRootKeyFileName = "ca.key"
)

// Bundle is the certificate material for one run.
type Bundle struct {
	dir string

	rootPEM []byte
	root    *x509.Certificate
	leaf    *tls.Certificate
	hosts   []string

	// issuedNow records whether this call had to create new material, and
	// why, so the caller can tell the user their clients must re-trust it.
	issuedNow    bool
	issuedReason string
}

// CertPathIn reports where the root certificate lives for a given directory,
// without creating anything.
func CertPathIn(dir string) string { return filepath.Join(dir, rootCertFileName) }

// ReadCert returns the root certificate previously written to dir.
func ReadCert(dir string) ([]byte, error) {
	path := CertPathIn(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ca: no certificate at %s yet; run `ccmproxy ca` or start `ccmproxy serve` to create one", path)
		}
		return nil, fmt.Errorf("ca: read %s: %w", path, err)
	}
	return data, nil
}

// Open returns the certificate material for hosts, reusing what is stored in
// dir when it is still valid and covers every host, and creating it otherwise.
func Open(dir string, hosts []string) (*Bundle, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("ca: directory is required")
	}
	names := normalizeHosts(hosts)
	if len(names) == 0 {
		return nil, fmt.Errorf("ca: at least one host is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ca: create %s: %w", dir, err)
	}

	bundle, err := load(dir, names)
	if err == nil {
		return bundle, nil
	}
	var unusable *unusableError
	if !errors.As(err, &unusable) {
		return nil, err
	}

	bundle, err = issue(dir, names)
	if err != nil {
		return nil, err
	}
	bundle.issuedNow = true
	bundle.issuedReason = unusable.reason
	return bundle, nil
}

// unusableError says the stored material cannot be used and why, which is the
// one condition that leads to re-issuing rather than failing.
type unusableError struct {
	reason string
}

func (e *unusableError) Error() string { return e.reason }

func unusable(format string, args ...any) error {
	return &unusableError{reason: fmt.Sprintf(format, args...)}
}

// IssuedNow reports whether Open had to create new material.
func (b *Bundle) IssuedNow() bool { return b.issuedNow }

// IssuedReason explains why new material was created.
func (b *Bundle) IssuedReason() string { return b.issuedReason }

// CertPath reports the PEM file holding the root certificate. This is the file
// clients must trust (for Claude Code: NODE_EXTRA_CA_CERTS).
func (b *Bundle) CertPath() string { return CertPathIn(b.dir) }

// CertPEM returns the PEM-encoded root certificate.
func (b *Bundle) CertPEM() []byte { return b.rootPEM }

// Root exposes the parsed root certificate.
func (b *Bundle) Root() *x509.Certificate { return b.root }

// NotAfter reports when the material stops being usable, which is the earlier
// of the two certificates' expiries.
func (b *Bundle) NotAfter() time.Time {
	if b.root.NotAfter.Before(b.leaf.Leaf.NotAfter) {
		return b.root.NotAfter
	}
	return b.leaf.Leaf.NotAfter
}

// Hosts lists the names this bundle serves.
func (b *Bundle) Hosts() []string {
	out := make([]string, len(b.hosts))
	copy(out, b.hosts)
	return out
}

// Leaf returns the certificate for host. host may carry a port, which is
// stripped. Only the hosts the certificate was issued for are available: the
// root key is gone, so nothing else can be signed.
func (b *Bundle) Leaf(host string) (*tls.Certificate, error) {
	name := normalizeHost(host)
	if name == "" {
		return nil, fmt.Errorf("ca: empty host")
	}
	if err := b.leaf.Leaf.VerifyHostname(name); err != nil {
		return nil, fmt.Errorf("ca: the stored certificate does not cover %q; add it to intercept_hosts and restart", name)
	}
	return b.leaf, nil
}

// GetCertificate is a tls.Config.GetCertificate callback. It falls back to
// fallbackHost when the client omits SNI or asks for a name this bundle does
// not cover.
func (b *Bundle) GetCertificate(fallbackHost string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello != nil {
			if leaf, err := b.Leaf(hello.ServerName); err == nil {
				return leaf, nil
			}
		}
		return b.Leaf(fallbackHost)
	}
}

// load reads the stored material and checks that it is still fit for use.
func load(dir string, hosts []string) (*Bundle, error) {
	rootPEM, err := os.ReadFile(CertPathIn(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, unusable("no certificate authority stored yet")
		}
		return nil, fmt.Errorf("ca: read %s: %w", CertPathIn(dir), err)
	}
	leafCertPEM, err := os.ReadFile(filepath.Join(dir, leafCertFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, unusable("%s is missing", leafCertFileName)
		}
		return nil, fmt.Errorf("ca: read %s: %w", leafCertFileName, err)
	}
	leafKeyPEM, err := os.ReadFile(filepath.Join(dir, leafKeyFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, unusable("%s is missing", leafKeyFileName)
		}
		return nil, fmt.Errorf("ca: read %s: %w", leafKeyFileName, err)
	}

	rootBlock, _ := pem.Decode(rootPEM)
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" {
		return nil, unusable("%s is not a PEM certificate", rootCertFileName)
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		return nil, unusable("%s could not be parsed: %v", rootCertFileName, err)
	}

	// X509KeyPair is what proves the stored key belongs to the certificate.
	leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, unusable("%s and %s do not form a usable key pair: %v", leafCertFileName, leafKeyFileName, err)
	}
	if leaf.Leaf == nil {
		if len(leaf.Certificate) == 0 {
			return nil, unusable("%s holds no certificate", leafCertFileName)
		}
		if leaf.Leaf, err = x509.ParseCertificate(leaf.Certificate[0]); err != nil {
			return nil, unusable("%s could not be parsed: %v", leafCertFileName, err)
		}
	}

	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, unusable("the stored server certificate does not chain to %s: %v", rootCertFileName, err)
	}

	deadline := time.Now().Add(renewBefore)
	if root.NotAfter.Before(deadline) {
		return nil, unusable("the stored authority expires %s", root.NotAfter.Format(time.RFC3339))
	}
	if leaf.Leaf.NotAfter.Before(deadline) {
		return nil, unusable("the stored server certificate expires %s", leaf.Leaf.NotAfter.Format(time.RFC3339))
	}

	// Adding a host to intercept_hosts needs a signature, and the key that
	// could produce one is gone, so the whole set is re-issued.
	for _, host := range hosts {
		if err := leaf.Leaf.VerifyHostname(host); err != nil {
			return nil, unusable("the stored server certificate does not cover %q", host)
		}
	}

	// Only the leaf is presented. A server must never send a self-signed
	// root in its chain: the client already has the anchor, and OpenSSL 3
	// (so Node, so Claude Code) rejects the handshake with
	// SELF_SIGNED_CERT_IN_CHAIN when it finds one there.
	leaf.Certificate = [][]byte{leaf.Leaf.Raw}

	return &Bundle{
		dir:     dir,
		rootPEM: rootPEM,
		root:    root,
		leaf:    &leaf,
		hosts:   hosts,
	}, nil
}

// issue creates a new authority and server certificate, writes all three files
// and discards the root key.
func issue(dir string, hosts []string) (*Bundle, error) {
	rootKey, root, rootPEM, err := newRoot()
	// The root key must not outlive this function whatever happens next.
	defer discard(rootKey)
	if err != nil {
		return nil, err
	}

	leaf, leafCertPEM, leafKeyPEM, err := newLeaf(hosts, root, rootKey)
	if err != nil {
		return nil, err
	}

	// The certificate goes out last: a reader that sees a new ca.crt can
	// rely on the material behind it already being in place.
	if err := writeFileAtomic(filepath.Join(dir, leafKeyFileName), leafKeyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, leafCertFileName), leafCertPEM, 0o644); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(CertPathIn(dir), rootPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(dir, legacyRootKeyFileName)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("ca: remove stored root key %s: %w",
			filepath.Join(dir, legacyRootKeyFileName), err)
	}

	return &Bundle{
		dir:     dir,
		rootPEM: rootPEM,
		root:    root,
		leaf:    leaf,
		hosts:   hosts,
	}, nil
}

func newRoot() (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	// P-256 for the root, matching the server key: every trust store and TLS
	// stack this proxy's clients use has supported it for years, and the
	// smaller key is quicker to generate on every cold start.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca: generate root key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return key, nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   rootCommonName,
			Organization: []string{"claude-code-model-proxy"},
		},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return key, nil, nil, fmt.Errorf("ca: self-sign root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return key, nil, nil, fmt.Errorf("ca: parse root: %w", err)
	}
	return key, cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// newLeaf signs one certificate covering every intercepted host, so a single
// stored key pair serves them all.
func newLeaf(hosts []string, root *x509.Certificate, rootKey *ecdsa.PrivateKey) (*tls.Certificate, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca: generate server key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, &key.PublicKey, rootKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca: sign server certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca: parse server certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca: marshal server key: %w", err)
	}

	return &tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        cert,
		},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// discard overwrites the root key's secret material before the last reference
// to it is dropped.
//
// This narrows the window rather than closing it: big.Int arithmetic during
// key generation and signing may have left copies behind, and Go's runtime
// makes no promise about scrubbing them. What is guaranteed is that the key was
// never written to disk and that no certificate can be signed with it once
// Open has returned.
func discard(key *ecdsa.PrivateKey) {
	if key == nil {
		return
	}
	// D is the whole secret for an EC key; the public point is not secret.
	if key.D != nil {
		key.D.SetInt64(0)
	}
}

// writeFileAtomic replaces path in one step, so a reader never sees a
// half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".ca-*.tmp")
	if err != nil {
		return fmt.Errorf("ca: create temporary file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("ca: write %s: %w", tempName, err)
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("ca: chmod %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("ca: close %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("ca: install %s: %w", path, err)
	}
	return nil
}

func normalizeHosts(hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		name := normalizeHost(host)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("ca: generate serial: %w", err)
	}
	return serial, nil
}
