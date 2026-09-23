package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeWSRConfig writes body to a temp config file and returns its path.
func writeWSRConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoad_WordSyncRecheckDefaults pins the dark-by-default contract (#1048):
// the sweep is off and the in-flight cap is 100.
func TestLoad_WordSyncRecheckDefaults(t *testing.T) {
	isolateEnv(t)
	// isolateEnv clears XDG_CONFIG_HOME to "" (unset), which makes
	// xdgConfigPath fall back to os.UserHomeDir() -- the real, unsandboxed
	// home directory. Load("") then resolves to the real
	// ~/.config/mxlrcgo-svc/config.toml, so a developer or CI box with a live
	// config there (any [word_sync_recheck] section, or any [db] path)
	// silently overrides these "defaults" assertions instead of testing them.
	// Point XDG_CONFIG_HOME at a fresh temp dir so Load("") resolves to a
	// path that provably has no config file, making this test hermetic
	// regardless of the host's real config.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncRecheck.Enabled {
		t.Error("WordSyncRecheck.Enabled = true; want false (dark by default)")
	}
	if cfg.WordSyncRecheck.Batch != 100 {
		t.Errorf("WordSyncRecheck.Batch = %d; want 100", cfg.WordSyncRecheck.Batch)
	}
}

// TestLoad_WordSyncRecheckFileThenEnv verifies the file sets both keys and the
// env overrides the file for both, recording each env override as applied.
func TestLoad_WordSyncRecheckFileThenEnv(t *testing.T) {
	isolateEnv(t)
	path := writeWSRConfig(t, "[word_sync_recheck]\nenabled = true\nbatch = 250\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(file): %v", err)
	}
	if !cfg.WordSyncRecheck.Enabled || cfg.WordSyncRecheck.Batch != 250 {
		t.Fatalf("file values not applied: %+v; want {Enabled:true Batch:250}", cfg.WordSyncRecheck)
	}

	t.Setenv("MXLRC_WORD_SYNC_RECHECK_ENABLED", "false")
	t.Setenv("MXLRC_WORD_SYNC_RECHECK_BATCH", "40")
	cfg, envSrc, err := LoadWithSources(path)
	if err != nil {
		t.Fatalf("LoadWithSources(file+env): %v", err)
	}
	if cfg.WordSyncRecheck.Enabled || cfg.WordSyncRecheck.Batch != 40 {
		t.Errorf("env did not override file: %+v; want {Enabled:false Batch:40}", cfg.WordSyncRecheck)
	}
	for _, k := range []string{"word_sync_recheck.enabled", "word_sync_recheck.batch"} {
		if !envSrc[k] {
			t.Errorf("env source map missing %q; the settings page would not lock the field", k)
		}
	}
}

// TestLoad_WordSyncRecheckInvalidBatchReDefaults verifies an out-of-range batch
// is never honored. From the file it resets to the default; from the env it is
// ignored, so the file (or default) value stands. Both bounds and a
// non-integer are covered, since the ceiling is the part a lower-bound-only
// check (the timing_validation shape) would miss.
func TestLoad_WordSyncRecheckInvalidBatchReDefaults(t *testing.T) {
	over := strconv.Itoa(wordSyncRecheckBatchMax + 1)
	for _, bad := range []string{"0", "-5", over} {
		t.Run("file="+bad, func(t *testing.T) {
			isolateEnv(t)
			cfg, err := Load(writeWSRConfig(t, "[word_sync_recheck]\nbatch = "+bad+"\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.WordSyncRecheck.Batch != wordSyncRecheckBatchDefault {
				t.Errorf("Batch = %d; want %d (file value %s reset)", cfg.WordSyncRecheck.Batch, wordSyncRecheckBatchDefault, bad)
			}
		})
	}
	for _, bad := range []string{"0", "-5", over, "lots", "2.5"} {
		t.Run("env="+bad, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("MXLRC_WORD_SYNC_RECHECK_BATCH", bad)
			cfg, envSrc, err := LoadWithSources(writeWSRConfig(t, "[word_sync_recheck]\nbatch = 30\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.WordSyncRecheck.Batch != 30 {
				t.Errorf("Batch = %d; want 30 (invalid env %q ignored, file value stands)", cfg.WordSyncRecheck.Batch, bad)
			}
			if envSrc["word_sync_recheck.batch"] {
				t.Errorf("invalid env %q recorded as applied", bad)
			}
		})
	}
	// The ceiling itself is legal on every path.
	isolateEnv(t)
	// See the comment in TestLoad_WordSyncRecheckDefaults: Load("") without
	// this falls through to the real, unsandboxed home directory.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MXLRC_WORD_SYNC_RECHECK_BATCH", strconv.Itoa(wordSyncRecheckBatchMax))
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WordSyncRecheck.Batch != wordSyncRecheckBatchMax {
		t.Errorf("Batch = %d; want the ceiling %d accepted", cfg.WordSyncRecheck.Batch, wordSyncRecheckBatchMax)
	}
}

// TestWordSyncRecheckConfigSetRoundTrip drives ApplyChanges -- the write path
// shared by the web settings-save handlers (internal/web/settings_save*.go)
// -- for both keys, then reloads the written file, so the registry, validator,
// writer, and decoder are proven to agree. It also verifies the validator
// rejects exactly what the loader would reset, leaving the file untouched.
//
// This does NOT cover the CLI `config set` subcommand: that command has its
// own separate get/set/list switches in internal/commands/commands.go
// (configKeys/configValue/setConfigValue) that do not call ApplyChanges and
// do not derive from this package's registry, so a key reachable here can
// still be "unknown config key" on the CLI (#1048 slice 6 review finding I1).
// See TestConfigWordSyncRecheckGetList and TestConfigSetWordSyncRecheckBatchRange
// in internal/commands/timing_validation_config_test.go for that surface.
func TestWordSyncRecheckConfigSetRoundTrip(t *testing.T) {
	isolateEnv(t)
	path := writeWSRConfig(t, "[api]\ncooldown = 60\n")
	if err := ApplyChanges(path, map[string]string{
		"word_sync_recheck.enabled": "true",
		"word_sync_recheck.batch":   "500",
	}); err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after write: %v", err)
	}
	if !cfg.WordSyncRecheck.Enabled || cfg.WordSyncRecheck.Batch != 500 {
		t.Errorf("written values did not round-trip: %+v; want {Enabled:true Batch:500}", cfg.WordSyncRecheck)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, bad := range []string{"0", "-1", strconv.Itoa(wordSyncRecheckBatchMax + 1), "many"} {
		if err := ApplyChanges(path, map[string]string{"word_sync_recheck.batch": bad}); err == nil {
			t.Errorf("config set word_sync_recheck.batch=%s accepted; want rejected", bad)
		}
	}
	if err := ApplyChanges(path, map[string]string{"word_sync_recheck.enabled": "sometimes"}); err == nil {
		t.Error("config set word_sync_recheck.enabled=sometimes accepted; want rejected")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("rejected set rewrote the file:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestWordSyncRecheckRenders verifies `config show` prints the section with the
// effective values and annotates an env-sourced key, and that the slog startup
// dump carries it.
func TestWordSyncRecheckRenders(t *testing.T) {
	cfg := defaults()
	cfg.WordSyncRecheck = WordSyncRecheckConfig{Enabled: true, Batch: 7}
	text := FormatConfigText(cfg, map[string]bool{"word_sync_recheck.batch": true}, nil)
	i := strings.Index(text, "[word_sync_recheck]\n")
	if i < 0 {
		t.Fatalf("section header missing from rendered config:\n%s", text)
	}
	section := text[i:]
	if j := strings.Index(section, "\n\n"); j >= 0 {
		section = section[:j]
	}
	if !strings.Contains(section, "enabled = true\n") {
		t.Errorf("rendered section lacks an unannotated enabled = true:\n%s", section)
	}
	if !strings.Contains(section, "batch = 7 (env)") {
		t.Errorf("rendered section lacks batch = 7 with its env annotation:\n%s", section)
	}

	var sb strings.Builder
	for _, a := range ConfigToSlogAttrs(cfg, nil, nil) {
		sb.WriteString(a.String())
	}
	if !strings.Contains(sb.String(), "word_sync_recheck=[enabled=true batch=7]") {
		t.Errorf("slog attrs lack the word_sync_recheck group: %s", sb.String())
	}
}
