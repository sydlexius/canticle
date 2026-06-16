package servetls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTLSConfigMinVersionAndCert(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	cm, err := newSelfSignedCertManager(dir)
	if err != nil {
		t.Fatalf("newSelfSignedCertManager: %v", err)
	}
	cfg := TLSConfig(cm)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x; want TLS 1.2 (%x)", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate not wired")
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Fatal("nil/empty certificate")
	}
}

func TestBuildCertManagerDisabled(t *testing.T) {
	cm, err := BuildCertManager("", "", false, t.TempDir())
	if err != nil {
		t.Fatalf("BuildCertManager: %v", err)
	}
	if cm != nil {
		t.Error("expected nil CertManager when TLS is disabled")
	}
}

func TestBuildCertManagerSelfSigned(t *testing.T) {
	dbDir := t.TempDir()
	cm, err := BuildCertManager("", "", true, dbDir)
	if err != nil {
		t.Fatalf("BuildCertManager: %v", err)
	}
	if cm == nil {
		t.Fatal("expected a CertManager for self_signed")
	}
	if _, ok := cm.(*selfSignedCertManager); !ok {
		t.Errorf("got %T; want *selfSignedCertManager", cm)
	}
	// Persisted under <dbDir>/tls/.
	if _, err := os.Stat(filepath.Join(dbDir, "tls", selfSignedCertFile)); err != nil {
		t.Errorf("self-signed cert not persisted under dbDir/tls: %v", err)
	}
}

func TestBuildCertManagerBYO(t *testing.T) {
	certPath, keyPath := writeFixturePair(t)
	cm, err := BuildCertManager(certPath, keyPath, false, t.TempDir())
	if err != nil {
		t.Fatalf("BuildCertManager: %v", err)
	}
	if _, ok := cm.(*fileCertManager); !ok {
		t.Errorf("got %T; want *fileCertManager", cm)
	}
	cert, err := cm.GetCertificate(nil)
	if err != nil || cert == nil {
		t.Fatalf("GetCertificate: cert=%v err=%v", cert, err)
	}
}

func TestBuildCertManagerBYOTakesPrecedenceOverSelfSigned(t *testing.T) {
	certPath, keyPath := writeFixturePair(t)
	cm, err := BuildCertManager(certPath, keyPath, true, t.TempDir())
	if err != nil {
		t.Fatalf("BuildCertManager: %v", err)
	}
	if _, ok := cm.(*fileCertManager); !ok {
		t.Errorf("got %T; BYO cert/key must win over self_signed", cm)
	}
}

func TestNewFileCertManagerBadFiles(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newFileCertManager(bad, bad); err == nil {
		t.Error("expected error for invalid keypair")
	}
}

// TestServeTLSEndToEnd is the strongest evidence for lane 5: a real http.Server
// using the TLSConfig built from a CertManager serves HTTPS, and the handler sees
// r.TLS != nil (the exact signal lane 3's secureRequest reads to flip the cookie
// Secure flag). Covers both BYO and self-signed managers.
func TestServeTLSEndToEnd(t *testing.T) {
	byoCert, byoKey := writeFixturePair(t)
	byo, err := newFileCertManager(byoCert, byoKey)
	if err != nil {
		t.Fatalf("newFileCertManager: %v", err)
	}
	selfsigned, err := newSelfSignedCertManager(filepath.Join(t.TempDir(), "tls"))
	if err != nil {
		t.Fatalf("newSelfSignedCertManager: %v", err)
	}

	for name, cm := range map[string]CertManager{"byo": byo, "self_signed": selfsigned} {
		t.Run(name, func(t *testing.T) {
			sawTLS := make(chan bool, 1)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawTLS <- (r.TLS != nil)
				fmt.Fprintln(w, "ok")
			})
			srv := httptest.NewUnstartedServer(h)
			srv.TLS = TLSConfig(cm)
			srv.StartTLS()
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%q", resp.StatusCode, body)
			}
			if saw := <-sawTLS; !saw {
				t.Error("handler did not see r.TLS (expected TLS connection)")
			}
			// MinVersion is enforced: a fresh handshake capped at TLS 1.1 must be
			// refused. Dial directly (not via the HTTP client) to avoid reusing a
			// pooled TLS 1.3 connection.
			conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // G402: test-only handshake-version probe; we assert it is REFUSED.
				MinVersion:         tls.VersionTLS10,
				MaxVersion:         tls.VersionTLS11,
			})
			if err == nil {
				_ = conn.Close()
				t.Error("server accepted a TLS 1.1 handshake; MinVersion not enforced")
			}
		})
	}
}

// --- shared cert helpers (used across the package's tests) ---

// writeFixturePair generates a fresh self-signed pair and writes it to temp files,
// returning their paths. Used as a stand-in for an operator's bring-your-own cert.
func writeFixturePair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	now := time.Now()
	certPEM, keyPEM, err := generateSelfSignedCert(now, now.Add(selfSignedValidity))
	if err != nil {
		t.Fatalf("generate fixture: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// certFromPEM parses the first CERTIFICATE block from PEM.
func certFromPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func mustLeaf(t *testing.T, cert *tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

func serialOf(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	return mustLeaf(t, cert).SerialNumber.String()
}

func serialOfPEM(t *testing.T, certPEM []byte) string {
	t.Helper()
	c, err := certFromPEM(certPEM)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c.SerialNumber.String()
}
