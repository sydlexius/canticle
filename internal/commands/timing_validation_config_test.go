package commands

import (
	"slices"
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
