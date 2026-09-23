package commands

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// TestConfigTimingValidationGetSetRoundTrip verifies the [timing_validation]
// keys are reachable through all three hand-maintained CLI surfaces:
// configKeys (what `config list` enumerates), configValue (what `config get`
// reads), and setConfigValue (what `config set` writes).
//
// These three switches are SEPARATE from the config registry and have no drift
// test binding them to it, so a key can validate, load from TOML/env, and
// render in the web UI while `config get` still reports "unknown config key".
// That is exactly how it read before this test existed.
func TestConfigTimingValidationGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		TimingValidation: config.TimingValidationConfig{
			// EVERY FIELD CARRIES A DISTINCT VALUE, and that is the point rather
			// than an aesthetic choice. When two fields share a value, an arm that
			// returns the WRONG field is indistinguishable from one that returns
			// the right field, so the whole wrong-mapping defect class becomes
			// untestable. These two bools previously both read `true`: mutating
			// the `enabled` arm to return RevalidateExisting passed cleanly.
			// Mutation-verified: with the two differing, that mutation reddens.
			Enabled:            true,
			RevalidateExisting: false,
			RevalidateBatch:    250,
			OnMisSynced:        config.TimingActionQuarantine,
			OnCategorical:      config.TimingActionPurge,
		},
	}
	gets := map[string]string{
		"timing_validation.enabled":             "true",
		"timing_validation.revalidate_existing": "false",
		"timing_validation.revalidate_batch":    "250",
		"timing_validation.on_mis_synced":       "quarantine",
		"timing_validation.on_categorical":      "purge",
	}
	for key, want := range gets {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Errorf("configValue(%q) ok = false; want true", key)
			continue
		}
		if got != want {
			t.Errorf("configValue(%q) = %q; want %q", key, got, want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Errorf("configKeys missing %q", key)
		}
	}

	// A SECOND config, because one is not enough to prove a getter READS its
	// field. Against a single fixture, an arm returning a hardcoded constant
	// equal to the expectation is indistinguishable from one reading the field
	// -- mutating the batch arm to `return "250", true` passed cleanly with only
	// the fixture above. Two configs whose values differ leaves a constant
	// nowhere to hide. Mutation-verified.
	other := config.Config{
		TimingValidation: config.TimingValidationConfig{
			Enabled:            false,
			RevalidateExisting: true,
			RevalidateBatch:    7,
			OnMisSynced:        config.TimingActionOff,
			OnCategorical:      config.TimingActionQuarantine,
		},
	}
	otherGets := map[string]string{
		"timing_validation.enabled":             "false",
		"timing_validation.revalidate_existing": "true",
		"timing_validation.revalidate_batch":    "7",
		"timing_validation.on_mis_synced":       "off",
		"timing_validation.on_categorical":      "quarantine",
	}
	for key, want := range otherGets {
		got, ok := configValue(other, key)
		if !ok {
			t.Errorf("configValue(%q) ok = false; want true", key)
			continue
		}
		if got != want {
			t.Errorf("configValue(%q) = %q; want %q (second config: the arm must READ the field, not return a constant)", key, got, want)
		}
	}
}

// TestSetConfigTimingValidationValid verifies each key accepts a legal value.
func TestSetConfigTimingValidationValid(t *testing.T) {
	var cfg config.Config
	sets := []struct {
		key, value string
		check      func(config.Config) bool
	}{
		{"timing_validation.enabled", "true", func(c config.Config) bool { return c.TimingValidation.Enabled }},
		{"timing_validation.revalidate_existing", "true", func(c config.Config) bool { return c.TimingValidation.RevalidateExisting }},
		{"timing_validation.revalidate_batch", "42", func(c config.Config) bool { return c.TimingValidation.RevalidateBatch == 42 }},
		{"timing_validation.on_mis_synced", "purge", func(c config.Config) bool {
			return c.TimingValidation.OnMisSynced == config.TimingActionPurge
		}},
		{"timing_validation.on_categorical", "off", func(c config.Config) bool {
			return c.TimingValidation.OnCategorical == config.TimingActionOff
		}},
	}
	for _, s := range sets {
		if err := setConfigValue(&cfg, s.key, s.value); err != nil {
			t.Errorf("setConfigValue(%q, %q): %v", s.key, s.value, err)
			continue
		}
		if !s.check(cfg) {
			t.Errorf("setConfigValue(%q, %q) did not take effect", s.key, s.value)
		}
	}
}

// TestSetConfigTimingValidationRejectsInvalid verifies `config set` refuses a
// value the loader would refuse, so the CLI cannot write a config file that the
// next boot silently corrects. The on_categorical=demote case is the one that
// matters most: it is legal for the OTHER action key, so a switch that shared
// one value set between them would accept it here.
func TestSetConfigTimingValidationRejectsInvalid(t *testing.T) {
	bad := []struct{ key, value string }{
		{"timing_validation.enabled", "maybe"},
		{"timing_validation.revalidate_existing", "sometimes"},
		{"timing_validation.revalidate_batch", "0"},
		{"timing_validation.revalidate_batch", "-1"},
		{"timing_validation.revalidate_batch", "many"},
		{"timing_validation.on_mis_synced", "delete"},
		{"timing_validation.on_mis_synced", ""},
		{"timing_validation.on_categorical", "demote"},
	}
	for _, b := range bad {
		var cfg config.Config
		if err := setConfigValue(&cfg, b.key, b.value); err == nil {
			t.Errorf("setConfigValue(%q, %q) accepted; want rejected", b.key, b.value)
		}
	}
}

// TestSetConfigTimingValidationNormalizesCase verifies an action is accepted
// case-insensitively and stored normalized, matching how the TOML and env paths
// treat the same value.
func TestSetConfigTimingValidationNormalizesCase(t *testing.T) {
	var cfg config.Config
	if err := setConfigValue(&cfg, "timing_validation.on_mis_synced", " Quarantine "); err != nil {
		t.Fatalf("setConfigValue: %v", err)
	}
	if cfg.TimingValidation.OnMisSynced != config.TimingActionQuarantine {
		t.Errorf("OnMisSynced = %q; want %q (normalized)",
			cfg.TimingValidation.OnMisSynced, config.TimingActionQuarantine)
	}
}

// TestConfigWordSyncRecheckGetList verifies `word_sync_recheck.enabled` and
// `.batch` are reachable through configKeys (what `config list` enumerates)
// and configValue (what `config get` reads) -- the same hand-maintained CLI
// surfaces TestConfigTimingValidationGetSetRoundTrip covers for
// [timing_validation]. These keys shipped in #1048 slice 6 without arms here,
// so `config get word_sync_recheck.enabled` read "unknown config key" even
// though the field loaded from TOML/env and rendered in the web UI (#1048
// slice 6 review finding I1).
func TestConfigWordSyncRecheckGetList(t *testing.T) {
	cfg := config.Config{
		WordSyncRecheck: config.WordSyncRecheckConfig{Enabled: true, Batch: 250},
	}
	gets := map[string]string{
		"word_sync_recheck.enabled": "true",
		"word_sync_recheck.batch":   "250",
	}
	for key, want := range gets {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Errorf("configValue(%q) ok = false; want true", key)
			continue
		}
		if got != want {
			t.Errorf("configValue(%q) = %q; want %q", key, got, want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Errorf("configKeys missing %q", key)
		}
	}

	// A second config with different values, so a hardcoded-constant arm has
	// nowhere to hide (same reasoning as the timing_validation test above).
	other := config.Config{
		WordSyncRecheck: config.WordSyncRecheckConfig{Enabled: false, Batch: 7},
	}
	otherGets := map[string]string{
		"word_sync_recheck.enabled": "false",
		"word_sync_recheck.batch":   "7",
	}
	for key, want := range otherGets {
		got, ok := configValue(other, key)
		if !ok {
			t.Errorf("configValue(%q) ok = false; want true", key)
			continue
		}
		if got != want {
			t.Errorf("configValue(%q) = %q; want %q (second config: the arm must READ the field, not return a constant)", key, got, want)
		}
	}
}

// TestConfigSetWordSyncRecheckBatchRange drives `config set` through runConfig
// end to end (not just setConfigValue) so the shared validator, the write, and
// the rejection path are all exercised together: the batch bound is 1..1000
// and must accept both ends and reject just outside them plus a non-integer,
// in every case leaving the on-disk file untouched on rejection.
func TestConfigSetWordSyncRecheckBatchRange(t *testing.T) {
	for _, good := range []string{"1", "1000"} {
		path := writeConfigTOML(t, "[word_sync_recheck]\nbatch = 100\n")
		var out bytes.Buffer
		code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
			Key: "word_sync_recheck.batch", Value: good, ConfigPath: path,
		}})
		if code != 0 {
			t.Errorf("word_sync_recheck.batch=%s: exit code = %d; want 0 (in range): %s", good, code, out.String())
		}
		got, ok := configValue(mustLoadConfigForTest(t, path), "word_sync_recheck.batch")
		if !ok || got != good {
			t.Errorf("word_sync_recheck.batch=%s: reloaded value = %q, ok=%v; want %q, true", good, got, ok, good)
		}
	}

	for _, bad := range []string{"0", "1001", "many"} {
		path := writeConfigTOML(t, "[word_sync_recheck]\nbatch = 100\n")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		var out bytes.Buffer
		code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
			Key: "word_sync_recheck.batch", Value: bad, ConfigPath: path,
		}})
		if code != 2 {
			t.Errorf("word_sync_recheck.batch=%s: exit code = %d; want 2 (out of range/non-integer)", bad, code)
		}
		if !strings.Contains(out.String(), "word_sync_recheck.batch") {
			t.Errorf("word_sync_recheck.batch=%s: output = %q; want the key named in the error", bad, out.String())
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("re-read config: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("word_sync_recheck.batch=%s: config file was rewritten despite the rejection:\nbefore=%q\nafter=%q", bad, before, after)
		}
	}
}

// mustLoadConfigForTest loads the config at path or fails the test, avoiding a
// third copy of the same three lines across the get/set tests in this file.
func mustLoadConfigForTest(t *testing.T, path string) config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}
	return cfg
}
