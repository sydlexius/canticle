package servetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// selfSignedCommonName is the subject/issuer CN for the generated certificate.
	selfSignedCommonName = "mxlrcgo-svc"
	// selfSignedValidity is the certificate lifetime (~365 days).
	selfSignedValidity = 365 * 24 * time.Hour
	// selfSignedCertFile and selfSignedKeyFile are the on-disk PEM filenames.
	selfSignedCertFile = "cert.pem"
	selfSignedKeyFile  = "key.pem"
	// keyFileMode and certFileMode are the persisted permissions. The private key
	// is owner-only (0600); the certificate is public material but kept tight too.
	keyFileMode  os.FileMode = 0o600
	certFileMode os.FileMode = 0o600
	dirMode      os.FileMode = 0o700
)

// selfSignedCertManager serves a self-signed certificate persisted under dir. The
// certificate is loaded (or generated and persisted) once at construction;
// regeneration happens only at startup when the stored pair is missing, unreadable,
// or expired.
type selfSignedCertManager struct {
	cert *tls.Certificate
}

func newSelfSignedCertManager(dir string) (*selfSignedCertManager, error) {
	cert, err := ensureSelfSignedCert(dir)
	if err != nil {
		return nil, err
	}
	slog.Warn("serving with a self-signed TLS certificate; browsers will show an untrusted-certificate prompt. Use a CA-issued cert (server.tls.cert_file/key_file) or a TLS-terminating reverse proxy for trusted access.",
		"dir", dir, "common_name", selfSignedCommonName)
	return &selfSignedCertManager{cert: cert}, nil
}

func (m *selfSignedCertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.cert, nil
}

// ensureSelfSignedCert loads the persisted self-signed pair under dir, generating
// and persisting a fresh one when the pair is missing, unreadable, or expired
// (or expires within the renewal window). The private key is written 0600.
func ensureSelfSignedCert(dir string) (*tls.Certificate, error) {
	certPath := filepath.Join(dir, selfSignedCertFile)
	keyPath := filepath.Join(dir, selfSignedKeyFile)

	if cert, ok := loadValidCert(certPath, keyPath); ok {
		slog.Info("reusing persisted self-signed TLS certificate", "cert", certPath)
		return cert, nil
	}

	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create TLS dir %s: %w", dir, err)
	}
	now := time.Now()
	certPEM, keyPEM, err := generateSelfSignedCert(now, now.Add(selfSignedValidity))
	if err != nil {
		return nil, err
	}
	// Write the key 0600 first (it is the secret); then the cert. WriteFile with
	// an explicit mode does not chmod an existing file, so remove any stale file
	// first to guarantee the mode on a regeneration over a wider-perm leftover.
	if err := writeFileMode(keyPath, keyPEM, keyFileMode); err != nil {
		return nil, fmt.Errorf("write TLS key %s: %w", keyPath, err)
	}
	if err := writeFileMode(certPath, certPEM, certFileMode); err != nil {
		return nil, fmt.Errorf("write TLS cert %s: %w", certPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse generated self-signed keypair: %w", err)
	}
	slog.Info("generated self-signed TLS certificate", "cert", certPath, "not_after", now.Add(selfSignedValidity).Format(time.RFC3339))
	return &cert, nil
}

// loadValidCert loads the PEM pair and reports whether it is usable: it must
// parse and not be expired (nor within the renewal window). Any failure returns
// ok=false so the caller regenerates.
func loadValidCert(certPath, keyPath string) (*tls.Certificate, bool) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, false
	}
	leaf := cert.Leaf
	if leaf == nil {
		// Go does not populate Leaf from LoadX509KeyPair; parse it to check expiry.
		if len(cert.Certificate) == 0 {
			return nil, false
		}
		parsed, perr := x509.ParseCertificate(cert.Certificate[0])
		if perr != nil {
			return nil, false
		}
		leaf = parsed
	}
	now := time.Now()
	// Regenerate if already expired or within 30 days of expiry, so a long-running
	// daemon rotates well before clients start failing handshakes.
	if now.Before(leaf.NotBefore) || now.Add(30*24*time.Hour).After(leaf.NotAfter) {
		return nil, false
	}
	return &cert, true
}

// generateSelfSignedCert builds an ECDSA P-256 self-signed certificate valid over
// [notBefore, notAfter], returning the cert and key as PEM. CN=mxlrcgo-svc, with
// loopback SANs so same-host HTTPS verifies against localhost/127.0.0.1/::1.
func generateSelfSignedCert(notBefore, notAfter time.Time) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ECDSA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial number: %w", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: selfSignedCommonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{selfSignedCommonName, "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// writeFileMode writes data to path with exactly the given mode, removing any
// existing file first so a regeneration cannot inherit a wider permission from a
// pre-existing file (os.WriteFile does not chmod an existing file).
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, data, mode)
}
