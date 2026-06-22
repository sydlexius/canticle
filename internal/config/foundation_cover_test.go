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

// TestFieldLookups_ReturnImmutableEnvVars ensures FieldByPath/FieldByEnvVar do
// not leak the package-global EnvVars backing slice (CR #289).
func TestFieldLookups_ReturnImmutableEnvVars(t *testing.T) {
	f, _ := FieldByPath("api.token")
	if len(f.EnvVars) > 0 {
		f.EnvVars[0] = "MXLRC_TAMPERED"
	}
	if fresh, _ := FieldByPath("api.token"); fresh.EnvVars[0] == "MXLRC_TAMPERED" {
		t.Error("FieldByPath leaked mutable EnvVars backing storage")
	}

	g, _ := FieldByEnvVar("MUSIXMATCH_TOKEN")
	if len(g.EnvVars) > 0 {
		g.EnvVars[0] = "MXLRC_TAMPERED2"
	}
	if fresh, _ := FieldByEnvVar("MUSIXMATCH_TOKEN"); fresh.EnvVars[0] == "MXLRC_TAMPERED2" {
		t.Error("FieldByEnvVar leaked mutable EnvVars backing storage")
	}
}

// TestWriteAtomic_NonENOENTReadErrorAborts ensures WriteAtomic does NOT overwrite
// the config when the backup read fails for a reason other than "file missing"
// (CR #289): losing recoverability on an already-unhealthy path is worse than
// failing the write.
func TestWriteAtomic_NonENOENTReadErrorAborts(t *testing.T) {
	doc, err := LoadDocument(writeTempConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	// Make the target path a directory so the backup read fails with EISDIR
	// (a non-ENOENT error), not "file not found".
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(cfgPath, doc); err == nil {
		t.Error("expected WriteAtomic to abort on a non-ENOENT backup read error")
	}
	if fi, err := os.Stat(cfgPath); err != nil || !fi.IsDir() {
		t.Error("target was overwritten despite the read error")
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
	if err := SetValue(doc, "api.circuit_open_duration", TypeInt, "5", ""); err != nil {
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

// TestSetValue_CreatesAbsentSection confirms a field whose [section] table is
// absent from the file is written by CREATING that table (operators hand-write
// minimal configs), rather than failing -- the #288 P1 fix. Existing sections,
// keys, and comments are preserved.
func TestSetValue_CreatesAbsentSection(t *testing.T) {
	path := writeTempConfig(t)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	// [enrichment] is not present in the fixture.
	if err := SetValue(doc, "enrichment.enabled", TypeBool, "true", ""); err != nil {
		t.Fatalf("SetValue into an absent section should create it, got: %v", err)
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	got := string(out)
	if !strings.Contains(got, "[enrichment]") {
		t.Errorf("absent section [enrichment] was not created:\n%s", got)
	}
	if !strings.Contains(got, "enabled = true") {
		t.Errorf("value not written into the created section:\n%s", got)
	}
	// Existing content is untouched.
	for _, must := range []string{
		"# This top comment must survive a write.",
		"token = \"secret-abc\"",
		"[logging]",
		"level = \"info\"",
		"[providers]",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("creating a section disturbed existing content; missing %q:\n%s", must, got)
		}
	}
}

// TestApplyChangesCreatesAbsentSection exercises the same absent-section path
// through the public ApplyChanges entry the settings save uses (#288 P1).
func TestApplyChangesCreatesAbsentSection(t *testing.T) {
	path := writeTempConfig(t)
	if err := ApplyChanges(path, map[string]string{"enrichment.enabled": "true"}); err != nil {
		t.Fatalf("ApplyChanges into an absent section: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Enrichment.Enabled {
		t.Error("enrichment.enabled not persisted via ApplyChanges into an absent section")
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
		if err := SetValue(doc, c.path, c.ftype, c.value, ""); err == nil {
			t.Errorf("SetValue(%s,%q) should have failed type conversion", c.path, c.value)
		}
	}
}

func TestLoadDocument_MissingFile(t *testing.T) {
	// A missing config file is treated as an empty document (create-on-save),
	// not an error, so settings can be saved before any config.toml exists (#296).
	doc, err := LoadDocument(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Errorf("missing file should load as an empty document, got error: %v", err)
	}
	if doc == nil {
		t.Error("expected a non-nil empty document for a missing file")
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
	// Valid change, but the config file is present and malformed -> the parse
	// error surfaces. (A MISSING file is not an error: it is create-on-save, see
	// TestApplyChanges_CreatesConfigWhenAbsent.)
	path := filepath.Join(t.TempDir(), "malformed.toml")
	if err := os.WriteFile(path, []byte("this is = not [valid toml"), 0o600); err != nil {
		t.Fatalf("write malformed fixture: %v", err)
	}
	if err := ApplyChanges(path, map[string]string{"logging.level": "info"}); err == nil {
		t.Error("expected a parse error for a malformed config file")
	}
}
