// Package servetls implements optional TLS for the serve-mode HTTP listener
// (issue #204, Area 4). It provides two certificate sources behind one
// CertManager seam: bring-your-own PEM files and a self-signed bootstrap that
// generates and persists a certificate for LAN use. The seam exists so a future
// ACME autocert path (lane 6) drops in without rewriting the listener.
//
// The security-relevant choices live here: TLS 1.2 minimum, self-signed keys
// persisted 0600, and a clear operator warning that a self-signed certificate is
// untrusted by browsers. The package never logs or persists secret material
// beyond the private-key file it deliberately writes 0600.
package servetls

import (
	"crypto/tls"
	"fmt"
	"path/filepath"
)

// CertManager supplies the TLS certificate for the serve listener. Its single
// method matches the signature of tls.Config.GetCertificate so an
// autocert.Manager (the deferred ACME lane) is a drop-in implementation and the
// listener never branches on TLS flavor.
type CertManager interface {
	// GetCertificate returns the certificate to present for a TLS handshake.
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
}

// BuildCertManager selects a CertManager from the resolved TLS settings, mirroring
// the config precedence: an explicit cert_file+key_file pair (bring-your-own)
// wins, else self_signed bootstraps a persisted certificate under <dbDir>/tls/,
// else TLS is disabled and (nil, nil) is returned so the caller serves plain
// HTTP. The caller is responsible for having validated that cert_file/key_file
// are set together and that self_signed is not combined with them (see
// config.validateServerTLS); this function trusts that contract and only acts on
// it.
func BuildCertManager(certFile, keyFile string, selfSigned bool, dbDir string) (CertManager, error) {
	switch {
	case certFile != "" && keyFile != "":
		return newFileCertManager(certFile, keyFile)
	case selfSigned:
		return newSelfSignedCertManager(filepath.Join(dbDir, "tls"))
	default:
		return nil, nil
	}
}

// TLSConfig builds the *tls.Config for the serve listener from a CertManager.
// MinVersion is pinned to TLS 1.2 (no SSLv3/TLS 1.0/1.1). The certificate is
// resolved per-handshake via the manager so a future rotating source (ACME)
// needs no listener change.
func TLSConfig(cm CertManager) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cm.GetCertificate,
	}
}

// fileCertManager serves a bring-your-own certificate loaded once from PEM files
// at startup. Loading at construction means a bad pair is a clean startup error
// rather than a per-handshake failure.
type fileCertManager struct {
	cert *tls.Certificate
}

func newFileCertManager(certFile, keyFile string) (*fileCertManager, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	return &fileCertManager{cert: &cert}, nil
}

func (m *fileCertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.cert, nil
}
