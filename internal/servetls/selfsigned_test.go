package servetls

import (
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGenerateSelfSignedCert(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := generateSelfSignedCert(now, now.Add(selfSignedValidity), nil, nil)
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
	cert, err := ensureSelfSignedCert(dir, nil, nil)
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
	first, err := ensureSelfSignedCert(dir, nil, nil)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := ensureSelfSignedCert(dir, nil, nil)
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
	certPEM, keyPEM, err := generateSelfSignedCert(past, past.Add(selfSignedValidity), nil, nil)
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

	cert, err := ensureSelfSignedCert(dir, nil, nil)
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
	m, err := newSelfSignedCertManager(dir, nil)
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

func TestGenerateSelfSignedCertWithExtraSANs(t *testing.T) {
	now := time.Now()
	extraDNS := []string{"nas.local"}
	extraIPs := []net.IP{net.ParseIP("192.168.1.100")}
	certPEM, _, err := generateSelfSignedCert(now, now.Add(selfSignedValidity), extraDNS, extraIPs)
	if err != nil {
		t.Fatalf("generateSelfSignedCert: %v", err)
	}
	cert := mustParseCert(t, certPEM)
	if !blockHasDNS(cert, "nas.local") {
		t.Errorf("extra DNS SAN missing: DNSNames=%v", cert.DNSNames)
	}
	if !blockHasDNS(cert, "localhost") {
		t.Errorf("built-in SAN localhost missing: DNSNames=%v", cert.DNSNames)
	}
	found := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("192.168.1.100")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extra IP SAN 192.168.1.100 missing: IPAddresses=%v", cert.IPAddresses)
	}
}

func TestEnsureSelfSignedCertRegeneratesOnSANChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	// Persist a cert with no extra SANs.
	now := time.Now()
	certPEM, keyPEM, err := generateSelfSignedCert(now, now.Add(selfSignedValidity), nil, nil)
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
	originalSerial := serialOfPEM(t, certPEM)

	// Request a cert with an extra DNS SAN the persisted cert doesn't cover.
	cert, err := ensureSelfSignedCert(dir, []string{"nas.local"}, nil)
	if err != nil {
		t.Fatalf("ensureSelfSignedCert: %v", err)
	}
	if serialOf(t, cert) == originalSerial {
		t.Error("certificate was reused; expected regeneration due to SAN change")
	}
	leaf := mustLeaf(t, cert)
	if !blockHasDNS(leaf, "nas.local") {
		t.Errorf("regenerated cert missing extra SAN nas.local; DNSNames=%v", leaf.DNSNames)
	}
}

func TestEnsureSelfSignedCertReusesWhenSANsCovered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	cert1, err := ensureSelfSignedCert(dir, []string{"nas.local"}, nil)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	serial1 := serialOf(t, cert1)
	// Same extras: should reuse without regenerating.
	cert2, err := ensureSelfSignedCert(dir, []string{"nas.local"}, nil)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if serialOf(t, cert2) != serial1 {
		t.Error("certificate was regenerated; expected reuse when configured SANs are already covered")
	}
}

func TestParseSelfSignedHosts(t *testing.T) {
	tests := []struct {
		name     string
		hosts    []string
		wantDNS  []string
		wantIPsN int
	}{
		{name: "empty input"},
		{
			name:    "hostname added",
			hosts:   []string{"nas.local"},
			wantDNS: []string{"nas.local"},
		},
		{
			name:    "builtin hostnames deduplicated",
			hosts:   []string{"localhost", selfSignedCommonName, "extra.host"},
			wantDNS: []string{"extra.host"},
		},
		{
			name:     "ip literal parsed",
			hosts:    []string{"192.168.1.1"},
			wantIPsN: 1,
		},
		{
			name:     "builtin ips deduplicated",
			hosts:    []string{"127.0.0.1", "::1", "192.168.1.1"},
			wantIPsN: 1,
		},
		{
			name:     "mixed hostname and ip",
			hosts:    []string{"nas.local", "192.168.1.100"},
			wantDNS:  []string{"nas.local"},
			wantIPsN: 1,
		},
		{
			name:    "dedup within extras",
			hosts:   []string{"nas.local", "nas.local"},
			wantDNS: []string{"nas.local"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dns, ips := parseSelfSignedHosts(tc.hosts)
			if len(dns) != len(tc.wantDNS) {
				t.Errorf("dnsNames = %v; want %v", dns, tc.wantDNS)
			} else {
				for i, want := range tc.wantDNS {
					if dns[i] != want {
						t.Errorf("dnsNames[%d] = %q; want %q", i, dns[i], want)
					}
				}
			}
			if len(ips) != tc.wantIPsN {
				t.Errorf("ips count = %d; want %d (ips=%v)", len(ips), tc.wantIPsN, ips)
			}
		})
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
