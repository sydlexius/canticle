package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_WatcherDefaults verifies the [watcher] section defaults match the
// watcher package behavior: disabled, 2000ms debounce, 100000 dir cap.
func TestLoad_WatcherDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Watcher.Enabled {
		t.Error("Watcher.Enabled = true; want false (off by default)")
	}
	if cfg.Watcher.DebounceMS != 2000 {
		t.Errorf("Watcher.DebounceMS = %d; want 2000", cfg.Watcher.DebounceMS)
	}
	if cfg.Watcher.MaxDirs != 100000 {
		t.Errorf("Watcher.MaxDirs = %d; want 100000", cfg.Watcher.MaxDirs)
	}
}

// TestLoad_WatcherEnvOverrides verifies the three MXLRCGO_WATCH_* env vars
// override the file/default values (env > file precedence).
func TestLoad_WatcherEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRCGO_WATCH_ENABLED", "true")
	t.Setenv("MXLRCGO_WATCH_DEBOUNCE_MS", "500")
	t.Setenv("MXLRCGO_WATCH_MAX_DIRS", "42")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Watcher.Enabled {
		t.Error("Watcher.Enabled = false; want true (env override)")
	}
	if cfg.Watcher.DebounceMS != 500 {
		t.Errorf("Watcher.DebounceMS = %d; want 500 (env override)", cfg.Watcher.DebounceMS)
	}
	if cfg.Watcher.MaxDirs != 42 {
		t.Errorf("Watcher.MaxDirs = %d; want 42 (env override)", cfg.Watcher.MaxDirs)
	}
}

// TestLoad_WatcherDebounceZeroAllowed verifies debounce_ms accepts 0 (non-
// negative rule); the watcher package clamps it to its default at construction.
func TestLoad_WatcherDebounceZeroAllowed(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRCGO_WATCH_DEBOUNCE_MS", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Watcher.DebounceMS != 0 {
		t.Errorf("Watcher.DebounceMS = %d; want 0 (explicit env value honored)", cfg.Watcher.DebounceMS)
	}
}

// TestLoad_WatcherEnvInvalidIgnored verifies invalid env values leave the
// default in place: a non-bool enabled, a negative debounce, a non-positive
// max_dirs are all rejected with the prior value kept.
func TestLoad_WatcherEnvInvalidIgnored(t *testing.T) {
	tests := []struct {
		name, env, val string
	}{
		{"enabled_notbool", "MXLRCGO_WATCH_ENABLED", "maybe"},
		{"debounce_negative", "MXLRCGO_WATCH_DEBOUNCE_MS", "-1"},
		{"debounce_notint", "MXLRCGO_WATCH_DEBOUNCE_MS", "fast"},
		{"max_dirs_zero", "MXLRCGO_WATCH_MAX_DIRS", "0"},
		{"max_dirs_negative", "MXLRCGO_WATCH_MAX_DIRS", "-5"},
		{"max_dirs_notint", "MXLRCGO_WATCH_MAX_DIRS", "lots"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(tc.env, tc.val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Watcher.Enabled {
				t.Error("Watcher.Enabled = true; want false (invalid env ignored)")
			}
			if cfg.Watcher.DebounceMS != 2000 {
				t.Errorf("Watcher.DebounceMS = %d; want 2000 (invalid env ignored)", cfg.Watcher.DebounceMS)
			}
			if cfg.Watcher.MaxDirs != 100000 {
				t.Errorf("Watcher.MaxDirs = %d; want 100000 (invalid env ignored)", cfg.Watcher.MaxDirs)
			}
		})
	}
}

// TestLoad_WatcherBlankFileRestoresDefaults verifies a config file that omits
// (or blanks) the int watcher keys restores the documented defaults rather than
// decoding to 0, mirroring the other zero-sensitive int sections.
func TestLoad_WatcherBlankFileRestoresDefaults(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[watcher]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Watcher.DebounceMS != 2000 {
		t.Errorf("Watcher.DebounceMS = %d; want 2000 (blank section restores default)", cfg.Watcher.DebounceMS)
	}
	if cfg.Watcher.MaxDirs != 100000 {
		t.Errorf("Watcher.MaxDirs = %d; want 100000 (blank section restores default)", cfg.Watcher.MaxDirs)
	}
}

// TestWatcherRegistryEntries verifies the three watcher fields are registered
// with the expected type, env var, tier, and editability so the settings UI and
// the env-override drift test both see them.
func TestWatcherRegistryEntries(t *testing.T) {
	want := []struct {
		path string
		typ  FieldType
		env  string
	}{
		{"watcher.enabled", TypeBool, "MXLRCGO_WATCH_ENABLED"},
		{"watcher.debounce_ms", TypeInt, "MXLRCGO_WATCH_DEBOUNCE_MS"},
		{"watcher.max_dirs", TypeInt, "MXLRCGO_WATCH_MAX_DIRS"},
	}
	for _, w := range want {
		f, ok := FieldByPath(w.path)
		if !ok {
			t.Errorf("registry missing %q", w.path)
			continue
		}
		if f.Section != "watcher" {
			t.Errorf("%s Section = %q; want watcher", w.path, f.Section)
		}
		if f.Type != w.typ {
			t.Errorf("%s Type = %v; want %v", w.path, f.Type, w.typ)
		}
		if len(f.EnvVars) != 1 || f.EnvVars[0] != w.env {
			t.Errorf("%s EnvVars = %v; want [%s]", w.path, f.EnvVars, w.env)
		}
		if f.Criticality != Safe {
			t.Errorf("%s Criticality = %v; want Safe", w.path, f.Criticality)
		}
		if !f.Editable {
			t.Errorf("%s Editable = false; want true", w.path)
		}
		if f.Sensitive {
			t.Errorf("%s Sensitive = true; want false", w.path)
		}
	}
}

// TestWatcherValidation verifies the registry-derived validators match the env
// rules: debounce_ms is non-negative, max_dirs is strictly positive.
func TestWatcherValidation(t *testing.T) {
	// debounce_ms: 0 ok, negative rejected.
	if err := ValidateAndSet("watcher.debounce_ms", "0"); err != nil {
		t.Errorf("watcher.debounce_ms=0 rejected: %v", err)
	}
	if err := ValidateAndSet("watcher.debounce_ms", "-1"); err == nil {
		t.Error("watcher.debounce_ms=-1 accepted; want rejected")
	}
	// max_dirs: positive ok, 0 and negative rejected.
	if err := ValidateAndSet("watcher.max_dirs", "1"); err != nil {
		t.Errorf("watcher.max_dirs=1 rejected: %v", err)
	}
	if err := ValidateAndSet("watcher.max_dirs", "0"); err == nil {
		t.Error("watcher.max_dirs=0 accepted; want rejected (strictly positive)")
	}
	if err := ValidateAndSet("watcher.max_dirs", "-1"); err == nil {
		t.Error("watcher.max_dirs=-1 accepted; want rejected")
	}
	// enabled: bool only.
	if err := ValidateAndSet("watcher.enabled", "true"); err != nil {
		t.Errorf("watcher.enabled=true rejected: %v", err)
	}
	if err := ValidateAndSet("watcher.enabled", "maybe"); err == nil {
		t.Error("watcher.enabled=maybe accepted; want rejected")
	}
}
