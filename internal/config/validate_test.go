package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateAndSet_RejectsUnknownKey(t *testing.T) {
	err := ValidateAndSet("api.nope", "x")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if !strings.Contains(ve.Error(), "unknown config key") {
		t.Errorf("ValidationError.Error() = %q, want it to mention the reason", ve.Error())
	}
}

func TestValidateAndSet_RejectsReadOnlyKeyFile(t *testing.T) {
	// secrets.key_file is Editable: false (key rotation is out of scope).
	if err := ValidateAndSet("secrets.key_file", "/tmp/whatever.key"); err == nil {
		t.Fatal("expected read-only rejection for secrets.key_file")
	}
}

func TestValidateAndSet_Enum(t *testing.T) {
	if err := ValidateAndSet("logging.level", "loud"); err == nil {
		t.Error("expected rejection of invalid log level")
	}
	if err := ValidateAndSet("logging.level", "debug"); err != nil {
		t.Errorf("valid log level rejected: %v", err)
	}
}

func TestValidateAndSet_UnitInterval(t *testing.T) {
	if err := ValidateAndSet("verification.min_confidence", "1.5"); err == nil {
		t.Error("expected rejection of out-of-range confidence")
	}
	if err := ValidateAndSet("verification.min_confidence", "0.8"); err != nil {
		t.Errorf("valid confidence rejected: %v", err)
	}
}

func TestValidateAndSet_TLSPEMFile(t *testing.T) {
	// Missing file is rejected.
	if err := ValidateAndSet("server.tls.cert_file", "/no/such/cert.pem"); err == nil {
		t.Error("expected rejection of missing cert path")
	}
	// Empty means "unset" and is allowed.
	if err := ValidateAndSet("server.tls.cert_file", ""); err != nil {
		t.Errorf("empty TLS path rejected: %v", err)
	}
	// Non-PEM content is rejected.
	nonPEM := filepath.Join(t.TempDir(), "notapem.pem")
	if err := os.WriteFile(nonPEM, []byte("not pem content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAndSet("server.tls.cert_file", nonPEM); err == nil {
		t.Error("expected rejection of non-PEM file")
	}
	// A valid PEM file passes for both cert and key fields.
	certPath, keyPath := makeTempPEMPair(t)
	if err := ValidateAndSet("server.tls.cert_file", certPath); err != nil {
		t.Errorf("valid cert PEM rejected: %v", err)
	}
	if err := ValidateAndSet("server.tls.key_file", keyPath); err != nil {
		t.Errorf("valid key PEM rejected: %v", err)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	accepted := []string{
		"http://localhost:9000",
		"https://infer.example.com/v1",
		"http://whisper:9000/v1/audio/transcriptions",
	}
	for _, u := range accepted {
		if err := ValidateHTTPURL(u); err != nil {
			t.Errorf("ValidateHTTPURL(%q) rejected; want accepted: %v", u, err)
		}
	}
	rejected := []string{
		"not a url",
		"/foo",             // scheme-less absolute path
		"example.com/path", // scheme-less host
		"example.com",      // no scheme, no path
		"",                 // empty
		"ftp://host",       // non-http(s) scheme
		"file:///x",        // non-http(s) scheme
		"ws://host",        // non-http(s) scheme
	}
	for _, u := range rejected {
		if err := ValidateHTTPURL(u); err == nil {
			t.Errorf("ValidateHTTPURL(%q) accepted; want rejected", u)
		}
	}
}

func TestValidateAndSet_URL(t *testing.T) {
	// Empty means "unset" and is allowed.
	if err := ValidateAndSet("verification.whisper_url", ""); err != nil {
		t.Errorf("empty whisper_url rejected: %v", err)
	}
	if err := ValidateAndSet("instrumental_detector.classifier_url", ""); err != nil {
		t.Errorf("empty classifier_url rejected: %v", err)
	}
	// A valid URL passes.
	if err := ValidateAndSet("verification.whisper_url", "http://localhost:9000"); err != nil {
		t.Errorf("valid whisper_url rejected: %v", err)
	}
	if err := ValidateAndSet("instrumental_detector.classifier_url", "https://infer.example.com/v1"); err != nil {
		t.Errorf("valid classifier_url rejected: %v", err)
	}
	// Scheme-less inputs are rejected: bare hostname, absolute path, no-scheme host+path.
	if err := ValidateAndSet("verification.whisper_url", "not a url"); err == nil {
		t.Error("expected rejection of whisper_url with no scheme")
	}
	if err := ValidateAndSet("instrumental_detector.classifier_url", "not a url"); err == nil {
		t.Error("expected rejection of classifier_url with no scheme")
	}
	if err := ValidateAndSet("verification.whisper_url", "/foo"); err == nil {
		t.Error("expected rejection of whisper_url with scheme-less path /foo")
	}
	if err := ValidateAndSet("verification.whisper_url", "example.com"); err == nil {
		t.Error("expected rejection of whisper_url with scheme-less host example.com")
	}
}

func TestValidatePEMFile(t *testing.T) {
	v := ValidatePEMFile()
	// Empty passes.
	if err := v(""); err != nil {
		t.Errorf("empty value rejected: %v", err)
	}
	// Missing file is rejected.
	if err := v("/no/such/file.pem"); err == nil {
		t.Error("expected rejection of missing file")
	}
	// Existing file with no PEM block is rejected.
	nonPEM := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(nonPEM, []byte("binary data, not PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v(nonPEM); err == nil {
		t.Error("expected rejection of non-PEM file")
	}
	// A valid PEM file passes.
	certPath, _ := makeTempPEMPair(t)
	if err := v(certPath); err != nil {
		t.Errorf("valid PEM file rejected: %v", err)
	}
}

// makeTempPEMPair generates a self-signed ECDSA P-256 certificate and writes
// the cert PEM and key PEM to separate temp files, returning their paths.
func makeTempPEMPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestValidateAndSet_DBPathNotPathChecked(t *testing.T) {
	// db.path must NOT be path-exists-checked: the SQLite file is created on
	// first boot, so a not-yet-existing path is valid.
	if err := ValidateAndSet("db.path", "/var/lib/mxlrc/fresh.db"); err != nil {
		t.Errorf("db.path wrongly rejected for non-existent file: %v", err)
	}
}

// TestApplyChanges_ZeroMutationOnInvalid is the headline guarantee: if any
// change is invalid, the file is left byte-identical and no .bak is written.
func TestApplyChanges_ZeroMutationOnInvalid(t *testing.T) {
	path := writeTempConfig(t)
	err := ApplyChanges(path, map[string]string{
		"logging.level":  "debug", // valid
		"logging.format": "xml",   // invalid enum -> whole apply must abort
	})
	if err == nil {
		t.Fatal("expected ApplyChanges to fail on the invalid change")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != roundtripFixture {
		t.Errorf("config was mutated despite a validation failure:\n%s", got)
	}
	if _, statErr := os.Stat(path + ".bak"); statErr == nil {
		t.Error(".bak was written despite a validation failure (file should be untouched)")
	}
}

func TestApplyChanges_WritesAllOnSuccess(t *testing.T) {
	path := writeTempConfig(t)
	err := ApplyChanges(path, map[string]string{
		"logging.level":  "warn",
		"api.cooldown":   "60",
		"providers.mode": "parallel",
	})
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	for _, want := range []string{`level = "warn"`, "cooldown = 60", `mode = "parallel"`, "# mxlrcgo-svc configuration"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}
