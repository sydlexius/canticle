package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- registry lookups ---

func TestRegistry_LookupsAndFlags(t *testing.T) {
	if got := len(Registry()); got != len(AllPaths()) {
		t.Fatalf("Registry/AllPaths length mismatch: %d vs %d", got, len(AllPaths()))
	}
	if len(Registry()) == 0 {
		t.Fatal("registry is empty")
	}

	if _, ok := FieldByPath("does.not.exist"); ok {
		t.Error("FieldByPath returned ok for an unknown path")
	}
	f, ok := FieldByPath("api.token")
	if !ok || !f.Sensitive || f.Criticality != Critical {
		t.Errorf("api.token: ok=%v sensitive=%v crit=%v", ok, f.Sensitive, f.Criticality)
	}

	// Alias lookup resolves to the same field.
	if g, ok := FieldByEnvVar("MXLRC_API_TOKEN"); !ok || g.Path != "api.token" {
		t.Errorf("FieldByEnvVar alias: ok=%v path=%q", ok, g.Path)
	}
	if _, ok := FieldByEnvVar("MXLRC_NOPE"); ok {
		t.Error("FieldByEnvVar returned ok for an unknown env var")
	}

	// secrets.key_file is the one read-only field.
	if kf, _ := FieldByPath("secrets.key_file"); kf.Editable {
		t.Error("secrets.key_file should be Editable: false")
	}
}

// TestRegistry_ReturnsImmutableCopy ensures a caller cannot mutate the global
// registry through the returned slice (e.g. flipping Editable/Sensitive or an
// env var name), which would desync validation/ApplyChanges.
func TestRegistry_ReturnsImmutableCopy(t *testing.T) {
	r := Registry()
	origEditable := r[0].Editable
	origEnv := ""
	if len(r[0].EnvVars) > 0 {
		origEnv = r[0].EnvVars[0]
	}

	// Tamper with the returned slice and its nested EnvVars.
	r[0].Editable = !r[0].Editable
	r[0].Sensitive = !r[0].Sensitive
	if len(r[0].EnvVars) > 0 {
		r[0].EnvVars[0] = "MXLRC_TAMPERED"
	}

	fresh := Registry()
	if fresh[0].Editable != origEditable {
		t.Error("mutating the returned slice changed the global registry (Editable)")
	}
	if len(fresh[0].EnvVars) > 0 && fresh[0].EnvVars[0] != origEnv {
		t.Error("mutating the returned slice's EnvVars changed the global registry")
	}
}

// --- writer: insert, missing section, type errors, load errors ---

func TestSetValue_InsertsAbsentKeyIntoSection(t *testing.T) {
	path := writeTempConfig(t)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	// api.circuit_open_duration is absent from the [api] section in the fixture.
	if err := SetValue(doc, "api.circuit_open_duration", TypeInt, "5"); err != nil {
		t.Fatalf("SetValue insert: %v", err)
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "circuit_open_duration = 5") {
		t.Errorf("inserted key missing from output:\n%s", out)
	}
}

func TestSetValue_MissingSectionErrors(t *testing.T) {
	path := writeTempConfig(t)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	// [enrichment] is not present in the fixture.
	if err := SetValue(doc, "enrichment.enabled", TypeBool, "true"); err == nil {
		t.Error("expected an error setting a key in an absent section")
	}
}

func TestSetValue_TypeErrors(t *testing.T) {
	path := writeTempConfig(t)
	doc, _ := LoadDocument(path)
	cases := []struct {
		path  string
		ftype FieldType
		value string
	}{
		{"api.cooldown", TypeInt, "abc"},
		{"output.bilingual_output", TypeBool, "maybe"},
		{"verification.min_confidence", TypeFloat64, "high"},
	}
	for _, c := range cases {
		if err := SetValue(doc, c.path, c.ftype, c.value); err == nil {
			t.Errorf("SetValue(%s,%q) should have failed type conversion", c.path, c.value)
		}
	}
}

func TestLoadDocument_MissingFile(t *testing.T) {
	if _, err := LoadDocument(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Error("expected error loading a missing file")
	}
}

// --- validate: direct validator branches + ApplyChanges edges ---

func TestValidatorConstructors(t *testing.T) {
	if ValidateNonNegativeInt()("x") == nil || ValidateNonNegativeInt()("-1") == nil {
		t.Error("non-negative int should reject non-numeric and negative")
	}
	if ValidateNonNegativeInt()("0") != nil {
		t.Error("non-negative int should accept 0")
	}
	if ValidateBool()("nope") == nil || ValidateBool()("true") != nil {
		t.Error("bool validator wrong")
	}
	if ValidateUnitInterval()("0") == nil || ValidateUnitInterval()("1") != nil {
		t.Error("unit interval should reject 0, accept 1")
	}
	if ValidateEnum("a", "b")("c") == nil || ValidateEnum("a", "b")("a") != nil {
		t.Error("enum validator wrong")
	}
}

func TestApplyChanges_EmptyMapNoOp(t *testing.T) {
	path := writeTempConfig(t)
	if err := ApplyChanges(path, map[string]string{}); err != nil {
		t.Errorf("empty ApplyChanges should be a no-op: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("empty ApplyChanges should not write a .bak")
	}
}

func TestApplyChanges_LoadErrorPropagates(t *testing.T) {
	// Valid change, but the config file does not exist -> load error surfaces.
	missing := filepath.Join(t.TempDir(), "absent.toml")
	if err := ApplyChanges(missing, map[string]string{"logging.level": "info"}); err == nil {
		t.Error("expected a load error for a missing config file")
	}
}
