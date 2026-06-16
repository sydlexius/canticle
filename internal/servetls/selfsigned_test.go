package servetls

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGenerateSelfSignedCert(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := generateSelfSignedCert(now, now.Add(selfSignedValidity))
	if err != nil {
		t.Fatalf("generateSelfSignedCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty PEM output")
	}
	// The pair must parse and carry the expected subject + loopback SANs.
	block := mustParseCert(t, certPEM)
	if block.Subject.CommonName != selfSignedCommonName {
		t.Errorf("CN = %q; want %q", block.Subject.CommonName, selfSignedCommonName)
	}
	if !blockHasDNS(block, "localhost") {
		t.Errorf("missing localhost SAN; DNSNames=%v", block.DNSNames)
	}
	if block.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("public key algorithm = %v; want ECDSA", block.PublicKeyAlgorithm)
	}
}

func TestEnsureSelfSignedCertGeneratesAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	cert, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("ensureSelfSignedCert: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("nil/empty certificate")
	}
	certPath := filepath.Join(dir, selfSignedCertFile)
	keyPath := filepath.Join(dir, selfSignedKeyFile)
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert not persisted: %v", err)
	}
	assertMode0600(t, keyPath)
	assertMode0600(t, certPath)
}

func TestEnsureSelfSignedCertReusesExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	first, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	// Reuse means the same serial number (no regeneration).
	if serialOf(t, first) != serialOf(t, second) {
		t.Error("certificate was regenerated; expected reuse of the persisted pair")
	}
}

func TestEnsureSelfSignedCertRegeneratesWhenExpired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	// Write an already-expired pair.
	past := time.Now().Add(-2 * selfSignedValidity)
	certPEM, keyPEM, err := generateSelfSignedCert(past, past.Add(selfSignedValidity))
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, selfSignedCertFile)
	keyPath := filepath.Join(dir, selfSignedKeyFile)
	if err := os.WriteFile(certPath, certPEM, certFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, keyFileMode); err != nil {
		t.Fatal(err)
	}
	expiredSerial := serialOfPEM(t, certPEM)

	cert, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("ensureSelfSignedCert: %v", err)
	}
	if serialOf(t, cert) == expiredSerial {
		t.Error("expired certificate was reused; expected regeneration")
	}
	// The regenerated cert must be valid now.
	leaf := mustLeaf(t, cert)
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		t.Errorf("regenerated cert not valid now: NotBefore=%v NotAfter=%v", leaf.NotBefore, leaf.NotAfter)
	}
}

func TestLoadValidCertRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadValidCert(filepath.Join(dir, "nope.pem"), filepath.Join(dir, "nope.key")); ok {
		t.Error("loadValidCert returned ok for missing files")
	}
}

func TestNewSelfSignedCertManagerGetCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	m, err := newSelfSignedCertManager(dir)
	if err != nil {
		t.Fatalf("newSelfSignedCertManager: %v", err)
	}
	cert, err := m.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("nil/empty certificate from manager")
	}
}

// --- helpers ---

func mustParseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	c, err := certFromPEM(certPEM)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func blockHasDNS(c *x509.Certificate, name string) bool {
	for _, d := range c.DNSNames {
		if d == name {
			return true
		}
	}
	return false
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not POSIX on Windows")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o; want 600", path, perm)
	}
}
