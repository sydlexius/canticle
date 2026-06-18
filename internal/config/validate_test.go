package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestValidateAndSet_PathExists(t *testing.T) {
	if err := ValidateAndSet("server.tls.cert_file", "/no/such/cert.pem"); err == nil {
		t.Error("expected rejection of missing cert path")
	}
	// Empty means "unset" and is allowed.
	if err := ValidateAndSet("server.tls.cert_file", ""); err != nil {
		t.Errorf("empty TLS path rejected: %v", err)
	}
	existing := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAndSet("server.tls.cert_file", existing); err != nil {
		t.Errorf("existing cert path rejected: %v", err)
	}
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
