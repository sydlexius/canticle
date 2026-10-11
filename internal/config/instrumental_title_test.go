package config

import (
	"os"
	"path/filepath"
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

func TestInstrumentalTitleMode_Default(t *testing.T) {
	isolateEnv(t)
	cfg := loadTitleMode(t, "")
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Fatalf("default mode = %q, want off", got)
	}
}

func TestInstrumentalTitleMode_ValidValuesAndInvalidReset(t *testing.T) {
	isolateEnv(t)
	for _, v := range []string{"off", "strong", "strong_weak", " STRONG "} {
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
	t.Setenv("MXLRC_INSTRUMENTAL_TITLE_MODE", "strong_weak")
	cfg := loadTitleMode(t, "[instrumental_title]\nmode = \"strong\"\n")
	if cfg.InstrumentalTitle.Mode != InstrumentalTitleStrongWeak {
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
	if got := cfg.InstrumentalTitleMode(); got != InstrumentalTitleStrongWeak {
		t.Errorf("unnormalized raw mode read as %q, want %q", got, InstrumentalTitleStrongWeak)
	}
	if got := (&Config{}).InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Errorf("zero-value config read as %q, want off", got)
	}
	var nilCfg *Config
	if got := nilCfg.InstrumentalTitleMode(); got != InstrumentalTitleOff {
		t.Errorf("nil config read as %q, want off", got)
	}
}
