package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_WordSyncGenerateDefaults verifies the [word_sync_generate] section
// defaults: the feature is OFF (forced alignment is heavy
// compute), serialized concurrency, and no URL/model configured.
func TestLoad_WordSyncGenerateDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncGenerate.Enabled {
		t.Error("WordSyncGenerate.Enabled = true; want false (off by default)")
	}
	if cfg.WordSyncGenerate.URL != "" {
		t.Errorf("WordSyncGenerate.URL = %q; want empty", cfg.WordSyncGenerate.URL)
	}
	if cfg.WordSyncGenerate.Concurrency != wordSyncGenerateConcurrencyDefault {
		t.Errorf("WordSyncGenerate.Concurrency = %d; want %d",
			cfg.WordSyncGenerate.Concurrency, wordSyncGenerateConcurrencyDefault)
	}
	if cfg.WordSyncGenerate.Model != "" {
		t.Errorf("WordSyncGenerate.Model = %q; want empty", cfg.WordSyncGenerate.Model)
	}
}

// TestLoad_WordSyncGenerateEnvOverrides verifies every
// MXLRC_WORD_SYNC_GENERATE_* env var overrides the file/default value.
func TestLoad_WordSyncGenerateEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_WORD_SYNC_GENERATE_ENABLED", "true")
	t.Setenv("MXLRC_WORD_SYNC_GENERATE_URL", "http://aligner.example:9000")
	t.Setenv("MXLRC_WORD_SYNC_GENERATE_CONCURRENCY", "4")
	t.Setenv("MXLRC_WORD_SYNC_GENERATE_MODEL", "large-v3")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WordSyncGenerate.Enabled {
		t.Error("WordSyncGenerate.Enabled = false; want true (env override)")
	}
	if cfg.WordSyncGenerate.URL != "http://aligner.example:9000" {
		t.Errorf("WordSyncGenerate.URL = %q; want the env value", cfg.WordSyncGenerate.URL)
	}
	if cfg.WordSyncGenerate.Concurrency != 4 {
		t.Errorf("WordSyncGenerate.Concurrency = %d; want 4 (env override)", cfg.WordSyncGenerate.Concurrency)
	}
	if cfg.WordSyncGenerate.Model != "large-v3" {
		t.Errorf("WordSyncGenerate.Model = %q; want the env value", cfg.WordSyncGenerate.Model)
	}
}

// TestLoad_WordSyncGenerateEnvInvalidIgnored verifies an invalid env value
// leaves the default in place and does not half-apply.
func TestLoad_WordSyncGenerateEnvInvalidIgnored(t *testing.T) {
	tests := []struct {
		name, env, val string
	}{
		{"enabled_notbool", "MXLRC_WORD_SYNC_GENERATE_ENABLED", "maybe"},
		{"concurrency_zero", "MXLRC_WORD_SYNC_GENERATE_CONCURRENCY", "0"},
		{"concurrency_negative", "MXLRC_WORD_SYNC_GENERATE_CONCURRENCY", "-1"},
		{"concurrency_notint", "MXLRC_WORD_SYNC_GENERATE_CONCURRENCY", "many"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(tc.env, tc.val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.WordSyncGenerate.Enabled {
				t.Error("WordSyncGenerate.Enabled = true; want false (invalid env ignored)")
			}
			if cfg.WordSyncGenerate.Concurrency != wordSyncGenerateConcurrencyDefault {
				t.Errorf("WordSyncGenerate.Concurrency = %d; want %d (invalid env ignored)",
					cfg.WordSyncGenerate.Concurrency, wordSyncGenerateConcurrencyDefault)
			}
		})
	}
}

// TestLoad_WordSyncGenerateFileOutOfRangeRestoresDefault verifies a bad value
// read from TOML is corrected exactly as the env path corrects it -- a file is
// not more trusted than an env var.
func TestLoad_WordSyncGenerateFileOutOfRangeRestoresDefault(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[word_sync_generate]\nconcurrency = -3\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncGenerate.Concurrency != wordSyncGenerateConcurrencyDefault {
		t.Errorf("WordSyncGenerate.Concurrency = %d; want %d (out-of-range file value reset)",
			cfg.WordSyncGenerate.Concurrency, wordSyncGenerateConcurrencyDefault)
	}
}

// TestLoad_WordSyncGenerateBlankFileRestoresDefaults verifies a config file
// that declares the section but omits its keys restores the documented
// default rather than decoding to a zero concurrency.
func TestLoad_WordSyncGenerateBlankFileRestoresDefaults(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[word_sync_generate]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncGenerate.Concurrency != wordSyncGenerateConcurrencyDefault {
		t.Errorf("WordSyncGenerate.Concurrency = %d; want %d (blank section restores default)",
			cfg.WordSyncGenerate.Concurrency, wordSyncGenerateConcurrencyDefault)
	}
}

// TestLoad_WordSyncGenerateExplicitFalseHonored verifies an explicit `false`
// in the file survives (Enabled defaults false, so a naive re-default cannot
// tell "absent" from "explicitly off").
func TestLoad_WordSyncGenerateExplicitFalseHonored(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[word_sync_generate]\nenabled = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncGenerate.Enabled {
		t.Error("explicit false not honored: Enabled = true")
	}
}

// TestWordSyncGenerateRegistryEntries verifies every field is registered with
// the expected type, env var, tier, and editability.
func TestWordSyncGenerateRegistryEntries(t *testing.T) {
	want := []struct {
		path string
		typ  FieldType
		env  string
		crit Criticality
	}{
		{"word_sync_generate.enabled", TypeBool, "MXLRC_WORD_SYNC_GENERATE_ENABLED", Safe},
		{"word_sync_generate.url", TypeString, "MXLRC_WORD_SYNC_GENERATE_URL", Caution},
		{"word_sync_generate.concurrency", TypeInt, "MXLRC_WORD_SYNC_GENERATE_CONCURRENCY", Safe},
		{"word_sync_generate.model", TypeString, "MXLRC_WORD_SYNC_GENERATE_MODEL", Safe},
	}
	for _, w := range want {
		f, ok := FieldByPath(w.path)
		if !ok {
			t.Errorf("registry missing %q", w.path)
			continue
		}
		if f.Section != "word_sync_generate" {
			t.Errorf("%s Section = %q; want word_sync_generate", w.path, f.Section)
		}
		if f.Type != w.typ {
			t.Errorf("%s Type = %v; want %v", w.path, f.Type, w.typ)
		}
		if len(f.EnvVars) != 1 || f.EnvVars[0] != w.env {
			t.Errorf("%s EnvVars = %v; want [%s]", w.path, f.EnvVars, w.env)
		}
		if f.Criticality != w.crit {
			t.Errorf("%s Criticality = %v; want %v", w.path, f.Criticality, w.crit)
		}
		if !f.Editable {
			t.Errorf("%s Editable = false; want true", w.path)
		}
		if f.Sensitive {
			t.Errorf("%s Sensitive = true; want false", w.path)
		}
	}
}

// TestWordSyncGenerateValidation verifies the registry-derived validators:
// the URL field accepts blank or a valid http(s) URL and rejects garbage; the
// concurrency is strictly positive.
func TestWordSyncGenerateValidation(t *testing.T) {
	if err := ValidateAndSet("word_sync_generate.url", ""); err != nil {
		t.Errorf("word_sync_generate.url=\"\" rejected: %v", err)
	}
	if err := ValidateAndSet("word_sync_generate.url", "http://aligner.example:9000"); err != nil {
		t.Errorf("word_sync_generate.url=valid rejected: %v", err)
	}
	for _, bad := range []string{"not a url", "ftp://example.com", "://bad"} {
		if err := ValidateAndSet("word_sync_generate.url", bad); err == nil {
			t.Errorf("word_sync_generate.url=%q accepted; want rejected", bad)
		}
	}
	for _, path := range []string{"word_sync_generate.concurrency"} {
		if err := ValidateAndSet(path, "1"); err != nil {
			t.Errorf("%s=1 rejected: %v", path, err)
		}
		for _, bad := range []string{"0", "-1", "some"} {
			if err := ValidateAndSet(path, bad); err == nil {
				t.Errorf("%s=%s accepted; want rejected", path, bad)
			}
		}
	}
}

// TestWordSyncGenerateModelIsFreeForm verifies the model field accepts any
// string, since it is sidecar-defined and canticle does not enumerate model
// names it does not control.
func TestWordSyncGenerateModelIsFreeForm(t *testing.T) {
	for _, v := range []string{"", "large-v3", "whatever-the-sidecar-supports"} {
		if err := ValidateAndSet("word_sync_generate.model", v); err != nil {
			t.Errorf("word_sync_generate.model=%q rejected: %v", v, err)
		}
	}
}

// captureRetiredWarn routes slog into a buffer for one test and returns it.
func captureRetiredWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoad_RetiredBudgetPerCycleFileBoots pins #1324: a file still carrying the
// retired word_sync_generate.budget_per_cycle boots, whatever the leftover's
// type (an int written by every earlier `config set`, or a wrong-typed value),
// the section's live keys still decode beside it, and the loader says the key
// has no effect.
func TestLoad_RetiredBudgetPerCycleFileBoots(t *testing.T) {
	for name, leftover := range map[string]string{
		"int":    "10",
		"zero":   "0",
		"string": `"lots"`,
		"bool":   "true",
		"array":  "[1, 2]",
	} {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			logs := captureRetiredWarn(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[word_sync_generate]\nbudget_per_cycle = " + leftover + "\nconcurrency = 3\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load with leftover budget_per_cycle = %s: %v", leftover, err)
			}
			if cfg.WordSyncGenerate.Concurrency != 3 {
				t.Errorf("Concurrency = %d; want 3 (live key beside the leftover)", cfg.WordSyncGenerate.Concurrency)
			}
			if !strings.Contains(logs.String(), "budget_per_cycle is retired and has no effect") {
				t.Errorf("no retired-key warning logged; logs:\n%s", logs.String())
			}
		})
	}
}

// TestLoad_RetiredBudgetPerCycleEnvBoots pins #1324: a leftover env var boots
// whatever its value, records no provenance (nothing is applied), and warns.
func TestLoad_RetiredBudgetPerCycleEnvBoots(t *testing.T) {
	for _, v := range []string{"25", "0", "lots"} {
		t.Run(v, func(t *testing.T) {
			isolateEnv(t)
			logs := captureRetiredWarn(t)
			t.Setenv(retiredWordSyncGenerateBudgetEnv, v)
			_, applied, err := LoadWithSources("")
			if err != nil {
				t.Fatalf("Load with %s=%q: %v", retiredWordSyncGenerateBudgetEnv, v, err)
			}
			if applied["word_sync_generate.budget_per_cycle"] {
				t.Error("provenance recorded for the retired key; nothing is applied")
			}
			if !strings.Contains(logs.String(), retiredWordSyncGenerateBudgetEnv+" is retired and has no effect") {
				t.Errorf("no retired-env warning logged; logs:\n%s", logs.String())
			}
		})
	}
}

// TestRetiredBudgetPerCycleLeftRegistry pins that the retired key is gone from
// the registry and the render, so the settings page and validation no longer
// offer it, and a save naming it is refused as unknown rather than written.
func TestRetiredBudgetPerCycleLeftRegistry(t *testing.T) {
	if _, ok := FieldByPath("word_sync_generate.budget_per_cycle"); ok {
		t.Fatal("word_sync_generate.budget_per_cycle is still in the registry")
	}
	var ve *ValidationError
	if err := ValidateAndSet("word_sync_generate.budget_per_cycle", "10"); !errors.As(err, &ve) || ve.Message != "unknown config key" {
		t.Errorf("ValidateAndSet(budget_per_cycle) = %v; want unknown config key", err)
	}
	if strings.Contains(FormatConfigText(defaults(), nil, nil), "budget_per_cycle") {
		t.Error("rendered config still shows budget_per_cycle")
	}
}

// TestApplyChanges_OverRetiredBudgetPerCycle pins that saving settings into a
// file that still carries a wrong-typed leftover succeeds, and the result still
// boots with the saved value.
func TestApplyChanges_OverRetiredBudgetPerCycle(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[word_sync_generate]\nbudget_per_cycle = \"lots\"\nconcurrency = 1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := ApplyChanges(path, map[string]string{"word_sync_generate.concurrency": "2"}); err != nil {
		t.Fatalf("ApplyChanges over a leftover: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if cfg.WordSyncGenerate.Concurrency != 2 {
		t.Errorf("Concurrency = %d; want 2 (the saved value)", cfg.WordSyncGenerate.Concurrency)
	}
}
