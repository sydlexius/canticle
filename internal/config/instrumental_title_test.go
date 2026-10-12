package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTitleMode(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestInstrumentalTitleMode_AbsentKeyLoadsAsStrong(t *testing.T) {
	isolateEnv(t)
	for name, body := range map[string]string{
		"empty file":      "",
		"section no mode": "[instrumental_title]\n",
	} {
		cfg := loadTitleMode(t, body)
		if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleStrong {
			t.Errorf("%s: absent mode = %q, want strong", name, got)
		}
	}
}

func TestInstrumentalTitleMode_ExplicitOffLoadsAsOff(t *testing.T) {
	isolateEnv(t)
	cfg := loadTitleMode(t, "[instrumental_title]\nmode = \"off\"\n")
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Fatalf("explicit off = %q, want off", got)
	}
}

func TestInstrumentalTitleMode_UnrecognizedFileValueLoadsAsOffAndWarns(t *testing.T) {
	isolateEnv(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfg := loadTitleMode(t, "[instrumental_title]\nmode = \"sideways\"\n")
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Fatalf("unrecognized file value = %q, want off (not the strong default)", got)
	}
	if !strings.Contains(buf.String(), "key=instrumental_title.mode") {
		t.Errorf("want a warning naming the key; log:\n%s", buf.String())
	}
}

func TestInstrumentalTitleMode_UnrecognizedEnvValueKeepsCurrent(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_TITLE_MODE", "sideways")
	cfg := loadTitleMode(t, "")
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleStrong {
		t.Fatalf("unrecognized env over absent key = %q, want the current value strong", got)
	}
	cfg = loadTitleMode(t, "[instrumental_title]\nmode = \"off\"\n")
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Fatalf("unrecognized env over file off = %q, want off", got)
	}
}

func TestInstrumentalTitleMode_ZeroValueConfigReadsOff(t *testing.T) {
	if got := (&Config{}).InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Fatalf("zero-value config read as %q, want off", got)
	}
}

func TestInstrumentalTitleMode_ValidValuesAndInvalidReset(t *testing.T) {
	isolateEnv(t)
	for _, v := range []string{"off", "strong", "all", " STRONG "} {
		cfg := loadTitleMode(t, "[instrumental_title]\nmode = \""+v+"\"\n")
		want, _ := normalizeInstrumentalTitleMode(v)
		if cfg.InstrumentalTitle.Mode != want {
			t.Errorf("mode %q loaded as %q, want %q", v, cfg.InstrumentalTitle.Mode, want)
		}
	}
	cfg := loadTitleMode(t, "[instrumental_title]\nmode = \"sideways\"\n")
	if cfg.InstrumentalTitle.Mode != InstrumentalTitleOff {
		t.Fatalf("invalid mode = %q, want reset to off", cfg.InstrumentalTitle.Mode)
	}
}

func TestInstrumentalTitleMode_EnvBeatsFileAndInvalidEnvKeeps(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_TITLE_MODE", "all")
	cfg := loadTitleMode(t, "[instrumental_title]\nmode = \"strong\"\n")
	if cfg.InstrumentalTitle.Mode != InstrumentalTitleAll {
		t.Fatalf("env did not beat file: %q", cfg.InstrumentalTitle.Mode)
	}
	t.Setenv("MXLRC_INSTRUMENTAL_TITLE_MODE", "bogus")
	cfg = loadTitleMode(t, "[instrumental_title]\nmode = \"strong\"\n")
	if cfg.InstrumentalTitle.Mode != InstrumentalTitleStrong {
		t.Fatalf("invalid env must keep the file value, got %q", cfg.InstrumentalTitle.Mode)
	}
}

func TestInstrumentalTitleMode_AccessorNormalizesAndIsNilSafe(t *testing.T) {
	cfg := &Config{InstrumentalTitle: InstrumentalTitleConfig{Mode: " Strong_Weak "}}
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleAll {
		t.Errorf("unnormalized raw mode read as %q, want %q", got, InstrumentalTitleAll)
	}
	if got := (&Config{}).InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Errorf("zero-value config read as %q, want off", got)
	}
	var nilCfg *Config
	if got := nilCfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Errorf("nil config read as %q, want off", got)
	}
}

// TestInstrumentalTitleMode_LegacyAliasLoadsAsAll: strong_weak shipped in
// v1.48.0 and is a deprecated alias of all. Whatever the spelling, the loaded
// value is all, one warning names the key and both values, and the file is not
// rewritten.
func TestInstrumentalTitleMode_LegacyAliasLoadsAsAll(t *testing.T) {
	for _, v := range []string{"strong_weak", "  Strong_Weak "} {
		isolateEnv(t)
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		body := "[instrumental_title]\nmode = \"" + v + "\"\n"
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalTitle.Mode != InstrumentalTitleAll {
			t.Errorf("mode %q loaded as %q, want all", v, cfg.InstrumentalTitle.Mode)
		}
		logged := buf.String()
		if strings.Count(logged, "key=instrumental_title.mode") != 1 || !strings.Contains(logged, "replacement=all") {
			t.Errorf("want exactly one warning naming the key and the new value for %q; log:\n%s", v, logged)
		}
		if got, _ := os.ReadFile(path); string(got) != body { //nolint:gosec // reason: G304: test temp path
			t.Errorf("load rewrote the file:\n%s", got)
		}
	}
}

func TestInstrumentalTitleMode_LegacyAliasFromEnvLoadsAsAll(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_TITLE_MODE", "strong_weak")
	cfg := loadTitleMode(t, "")
	if cfg.InstrumentalTitle.Mode != InstrumentalTitleAll {
		t.Fatalf("env strong_weak loaded as %q, want all", cfg.InstrumentalTitle.Mode)
	}
}

func TestApplyChanges_TitleModeAliasStoresAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[instrumental_title]\nmode = \"strong\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyChanges(path, map[string]string{"instrumental_title.mode": " Strong_Weak "}); err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	got, _ := os.ReadFile(path) //nolint:gosec // reason: G304: test temp path
	if !strings.Contains(string(got), `mode = "all"`) || strings.Contains(string(got), "strong_weak") {
		t.Errorf("file did not store the canonical value:\n%s", got)
	}
}
