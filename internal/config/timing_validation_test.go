package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_TimingValidationDefaults verifies the [timing_validation] section
// defaults: the feature is OFF, the backlog drain is OFF, a modest batch, and
// the two non-destructive remediation actions.
//
// The default actions are deliberately the RECOVERABLE ones. A MisSynced
// lyric's words are content-correct (#438 Investigation-0), so demote keeps
// them as .txt; a categorical one is quarantined rather than purged, so an
// operator can inspect and restore. Nothing here deletes on a fresh install.
func TestLoad_TimingValidationDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TimingValidation.Enabled {
		t.Error("TimingValidation.Enabled = true; want false (off by default)")
	}
	if cfg.TimingValidation.RevalidateExisting {
		t.Error("TimingValidation.RevalidateExisting = true; want false (off by default)")
	}
	if cfg.TimingValidation.RevalidateBatch != timingValidationBatchDefault {
		t.Errorf("TimingValidation.RevalidateBatch = %d; want %d",
			cfg.TimingValidation.RevalidateBatch, timingValidationBatchDefault)
	}
	if cfg.TimingValidation.OnMisSynced != TimingActionDemote {
		t.Errorf("TimingValidation.OnMisSynced = %q; want %q",
			cfg.TimingValidation.OnMisSynced, TimingActionDemote)
	}
	if cfg.TimingValidation.OnCategorical != TimingActionQuarantine {
		t.Errorf("TimingValidation.OnCategorical = %q; want %q",
			cfg.TimingValidation.OnCategorical, TimingActionQuarantine)
	}
}

// TestLoad_TimingValidationEnvOverrides verifies every MXLRC_TIMING_VALIDATION_*
// env var overrides the file/default value (env > file precedence).
func TestLoad_TimingValidationEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_TIMING_VALIDATION_ENABLED", "true")
	t.Setenv("MXLRC_TIMING_VALIDATION_REVALIDATE_EXISTING", "true")
	t.Setenv("MXLRC_TIMING_VALIDATION_REVALIDATE_BATCH", "25")
	t.Setenv("MXLRC_TIMING_VALIDATION_ON_MIS_SYNCED", "purge")
	t.Setenv("MXLRC_TIMING_VALIDATION_ON_CATEGORICAL", "off")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TimingValidation.Enabled {
		t.Error("TimingValidation.Enabled = false; want true (env override)")
	}
	if !cfg.TimingValidation.RevalidateExisting {
		t.Error("TimingValidation.RevalidateExisting = false; want true (env override)")
	}
	if cfg.TimingValidation.RevalidateBatch != 25 {
		t.Errorf("TimingValidation.RevalidateBatch = %d; want 25 (env override)", cfg.TimingValidation.RevalidateBatch)
	}
	if cfg.TimingValidation.OnMisSynced != TimingActionPurge {
		t.Errorf("TimingValidation.OnMisSynced = %q; want %q (env override)",
			cfg.TimingValidation.OnMisSynced, TimingActionPurge)
	}
	if cfg.TimingValidation.OnCategorical != TimingActionOff {
		t.Errorf("TimingValidation.OnCategorical = %q; want %q (env override)",
			cfg.TimingValidation.OnCategorical, TimingActionOff)
	}
}

// TestLoad_TimingValidationEnvInvalidIgnored verifies an invalid env value
// leaves the default in place rather than half-applying. The action fields
// matter most here: an unrecognized action must never fall through to a
// destructive one, so each case asserts the whole section is untouched.
func TestLoad_TimingValidationEnvInvalidIgnored(t *testing.T) {
	tests := []struct {
		name, env, val string
	}{
		{"enabled_notbool", "MXLRC_TIMING_VALIDATION_ENABLED", "maybe"},
		{"revalidate_existing_notbool", "MXLRC_TIMING_VALIDATION_REVALIDATE_EXISTING", "sometimes"},
		{"batch_zero", "MXLRC_TIMING_VALIDATION_REVALIDATE_BATCH", "0"},
		{"batch_negative", "MXLRC_TIMING_VALIDATION_REVALIDATE_BATCH", "-5"},
		{"batch_notint", "MXLRC_TIMING_VALIDATION_REVALIDATE_BATCH", "lots"},
		{"overrun_action_unknown", "MXLRC_TIMING_VALIDATION_ON_MIS_SYNCED", "delete"},
		{"overrun_action_bare_word", "MXLRC_TIMING_VALIDATION_ON_MIS_SYNCED", "yes"},
		{"on_categorical_unknown", "MXLRC_TIMING_VALIDATION_ON_CATEGORICAL", "demote"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(tc.env, tc.val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.TimingValidation.Enabled {
				t.Error("TimingValidation.Enabled = true; want false (invalid env ignored)")
			}
			if cfg.TimingValidation.RevalidateExisting {
				t.Error("TimingValidation.RevalidateExisting = true; want false (invalid env ignored)")
			}
			if cfg.TimingValidation.RevalidateBatch != timingValidationBatchDefault {
				t.Errorf("TimingValidation.RevalidateBatch = %d; want %d (invalid env ignored)",
					cfg.TimingValidation.RevalidateBatch, timingValidationBatchDefault)
			}
			if cfg.TimingValidation.OnMisSynced != TimingActionDemote {
				t.Errorf("TimingValidation.OnMisSynced = %q; want %q (invalid env ignored)",
					cfg.TimingValidation.OnMisSynced, TimingActionDemote)
			}
			if cfg.TimingValidation.OnCategorical != TimingActionQuarantine {
				t.Errorf("TimingValidation.OnCategorical = %q; want %q (invalid env ignored)",
					cfg.TimingValidation.OnCategorical, TimingActionQuarantine)
			}
		})
	}
}

// TestLoad_TimingValidationOnCategoricalRejectsDemote pins the one asymmetry
// between the two action enums: a categorical lyric is the WRONG SONG'S WORDS
// (#438), so there is nothing worth demoting to .txt and "demote" is not an
// accepted value there. Without this the two fields would look interchangeable.
func TestLoad_TimingValidationOnCategoricalRejectsDemote(t *testing.T) {
	if err := ValidateAndSet("timing_validation.on_categorical", string(TimingActionDemote)); err == nil {
		t.Error("timing_validation.on_categorical=demote accepted; want rejected (no words worth keeping)")
	}
	if err := ValidateAndSet("timing_validation.on_mis_synced", string(TimingActionDemote)); err != nil {
		t.Errorf("timing_validation.on_mis_synced=demote rejected: %v", err)
	}
}

// TestLoad_TimingValidationBlankFileRestoresDefaults verifies a config file that
// declares the section but omits its keys restores the documented defaults
// rather than decoding to a zero batch and two empty action strings.
func TestLoad_TimingValidationBlankFileRestoresDefaults(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[timing_validation]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TimingValidation.RevalidateBatch != timingValidationBatchDefault {
		t.Errorf("TimingValidation.RevalidateBatch = %d; want %d (blank section restores default)",
			cfg.TimingValidation.RevalidateBatch, timingValidationBatchDefault)
	}
	if cfg.TimingValidation.OnMisSynced != TimingActionDemote {
		t.Errorf("TimingValidation.OnMisSynced = %q; want %q (blank section restores default)",
			cfg.TimingValidation.OnMisSynced, TimingActionDemote)
	}
	if cfg.TimingValidation.OnCategorical != TimingActionQuarantine {
		t.Errorf("TimingValidation.OnCategorical = %q; want %q (blank section restores default)",
			cfg.TimingValidation.OnCategorical, TimingActionQuarantine)
	}
}

// TestLoad_TimingValidationFileOutOfRangeRestoresDefault verifies a bad value
// read from TOML is corrected exactly as the env path corrects it. A file is
// not more trusted than an env var: a batch of 0 would drain nothing forever
// while still waking the array on every tick.
func TestLoad_TimingValidationFileOutOfRangeRestoresDefault(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	// on_categorical is "demote" ON PURPOSE, and a blank would NOT do here.
	// "demote" is the one value that DISCRIMINATES between the two enum sets:
	// it is legal for on_mis_synced and illegal for on_categorical, so this
	// assertion fails if the file path ever consults the wider set. A blank is
	// rejected by BOTH sets, so it cannot tell them apart and would let a
	// one-token slip on the file path admit demote into on_categorical -- the
	// exact state this asymmetry exists to prevent. Mutation-verified: pointing
	// the file path's on_categorical check at timingMisSyncedActions() reddens
	// this test, and reddened nothing while the value was blank.
	body := "[timing_validation]\nrevalidate_batch = 0\non_mis_synced = \"resync\"\non_categorical = \"demote\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TimingValidation.RevalidateBatch != timingValidationBatchDefault {
		t.Errorf("TimingValidation.RevalidateBatch = %d; want %d (out-of-range file value reset)",
			cfg.TimingValidation.RevalidateBatch, timingValidationBatchDefault)
	}
	if cfg.TimingValidation.OnMisSynced != TimingActionDemote {
		t.Errorf("TimingValidation.OnMisSynced = %q; want %q (unknown file value reset)",
			cfg.TimingValidation.OnMisSynced, TimingActionDemote)
	}
	if cfg.TimingValidation.OnCategorical != TimingActionQuarantine {
		t.Errorf("TimingValidation.OnCategorical = %q; want %q (a value legal only for the OTHER arm must reset)",
			cfg.TimingValidation.OnCategorical, TimingActionQuarantine)
	}
}

// TestLoad_TimingValidationExplicitFalseHonored verifies an explicit `false` in
// the file survives. Both bools default to false, so a naive re-default cannot
// tell "absent" from "explicitly off" -- this pins that neither is clobbered.
func TestLoad_TimingValidationExplicitFalseHonored(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[timing_validation]\nenabled = false\nrevalidate_existing = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TimingValidation.Enabled || cfg.TimingValidation.RevalidateExisting {
		t.Errorf("explicit false not honored: Enabled=%t RevalidateExisting=%t",
			cfg.TimingValidation.Enabled, cfg.TimingValidation.RevalidateExisting)
	}
}

// TestTimingValidationRegistryEntries verifies every field is registered with
// the expected type, env var, tier, and editability so the settings UI and the
// env-override drift test both see them.
//
// The two action fields are Caution, not Safe: they decide what happens to a
// file on disk, and one of their accepted values is irreversible.
func TestTimingValidationRegistryEntries(t *testing.T) {
	want := []struct {
		path string
		typ  FieldType
		env  string
		crit Criticality
	}{
		{"timing_validation.enabled", TypeBool, "MXLRC_TIMING_VALIDATION_ENABLED", Safe},
		{"timing_validation.revalidate_existing", TypeBool, "MXLRC_TIMING_VALIDATION_REVALIDATE_EXISTING", Caution},
		{"timing_validation.revalidate_batch", TypeInt, "MXLRC_TIMING_VALIDATION_REVALIDATE_BATCH", Safe},
		{"timing_validation.on_mis_synced", TypeString, "MXLRC_TIMING_VALIDATION_ON_MIS_SYNCED", Caution},
		{"timing_validation.on_categorical", TypeString, "MXLRC_TIMING_VALIDATION_ON_CATEGORICAL", Caution},
	}
	for _, w := range want {
		f, ok := FieldByPath(w.path)
		if !ok {
			t.Errorf("registry missing %q", w.path)
			continue
		}
		if f.Section != "timing_validation" {
			t.Errorf("%s Section = %q; want timing_validation", w.path, f.Section)
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

// TestTimingValidationValidation verifies the registry-derived validators match
// the env rules: the batch is strictly positive and each action accepts exactly
// its own value set.
func TestTimingValidationValidation(t *testing.T) {
	if err := ValidateAndSet("timing_validation.revalidate_batch", "1"); err != nil {
		t.Errorf("timing_validation.revalidate_batch=1 rejected: %v", err)
	}
	for _, bad := range []string{"0", "-1", "some"} {
		if err := ValidateAndSet("timing_validation.revalidate_batch", bad); err == nil {
			t.Errorf("timing_validation.revalidate_batch=%s accepted; want rejected", bad)
		}
	}
	for _, ok := range []string{"demote", "quarantine", "purge", "off"} {
		if err := ValidateAndSet("timing_validation.on_mis_synced", ok); err != nil {
			t.Errorf("timing_validation.on_mis_synced=%s rejected: %v", ok, err)
		}
	}
	for _, ok := range []string{"quarantine", "purge", "off"} {
		if err := ValidateAndSet("timing_validation.on_categorical", ok); err != nil {
			t.Errorf("timing_validation.on_categorical=%s rejected: %v", ok, err)
		}
	}
	// "DEMOTE" is deliberately NOT in this list. Case and surrounding
	// whitespace are forgiven on every path (the loader folds them, so the
	// writer must too -- see TestTimingValidationNormalizationIsSymmetric);
	// what stays rejected is a value that is not a member at all.
	for _, bad := range []string{"", "delete", "resync"} {
		if err := ValidateAndSet("timing_validation.on_mis_synced", bad); err == nil {
			t.Errorf("timing_validation.on_mis_synced=%q accepted; want rejected", bad)
		}
	}
}

// TestTimingValidationAllowedValuesDrivesUI verifies both action fields expose
// their value set through AllowedValues, so the settings dropdown cannot drift
// from what validation accepts.
func TestTimingValidationAllowedValuesDrivesUI(t *testing.T) {
	overrun := AllowedValues("timing_validation.on_mis_synced")
	if len(overrun) != 4 {
		t.Errorf("AllowedValues(on_mis_synced) = %v; want 4 values", overrun)
	}
	cat := AllowedValues("timing_validation.on_categorical")
	if len(cat) != 3 {
		t.Errorf("AllowedValues(on_categorical) = %v; want 3 values", cat)
	}
	for _, v := range cat {
		if v == string(TimingActionDemote) {
			t.Error("AllowedValues(on_categorical) offers demote; want it absent")
		}
	}
}

// TestTimingValidationNormalizationIsSymmetric pins that every path agrees
// about what is legal, not just about the value SET.
//
// The loader normalizes (trim + lowercase) before validating, so a TOML file
// holding " PURGE " boots fine and purges files. ValidateEnum compares raw, so
// the settings UI and `config set` rejected that same value. The operator
// symptom is nasty and hard to diagnose: a working on-disk config, and opening
// the settings page and saving ANY field in the section fails with "must be one
// of ..." on a field they never touched -- because the section save validates
// every value it renders, including the one already on disk.
//
// Whitespace and case are forgiven everywhere or nowhere. This asserts
// everywhere, matching the loader's documented intent.
func TestTimingValidationNormalizationIsSymmetric(t *testing.T) {
	variants := []string{"purge", "PURGE", "Purge", " purge", "purge ", "  PuRgE  "}
	for _, v := range variants {
		t.Run("mis_synced/"+v, func(t *testing.T) {
			if err := ValidateAndSet("timing_validation.on_mis_synced", v); err != nil {
				t.Errorf("ValidateAndSet(on_mis_synced, %q) = %v; want accepted -- the loader accepts it, so the writer must too", v, err)
			}
		})
	}
	for _, v := range []string{"quarantine", "QUARANTINE", " Quarantine "} {
		t.Run("categorical/"+v, func(t *testing.T) {
			if err := ValidateAndSet("timing_validation.on_categorical", v); err != nil {
				t.Errorf("ValidateAndSet(on_categorical, %q) = %v; want accepted", v, err)
			}
		})
	}
	// Normalizing must NOT widen the accepted set: the asymmetry has to survive
	// case-folding, or " DEMOTE " becomes a back door into on_categorical.
	for _, v := range []string{"demote", "DEMOTE", " Demote "} {
		t.Run("categorical_rejects/"+v, func(t *testing.T) {
			if err := ValidateAndSet("timing_validation.on_categorical", v); err == nil {
				t.Errorf("ValidateAndSet(on_categorical, %q) accepted; want rejected -- normalization must not widen the set", v)
			}
		})
	}
	// And a genuine typo stays rejected however it is cased.
	for _, v := range []string{"resync", "RESYNC", " delete "} {
		t.Run("typo_rejected/"+v, func(t *testing.T) {
			if err := ValidateAndSet("timing_validation.on_mis_synced", v); err == nil {
				t.Errorf("ValidateAndSet(on_mis_synced, %q) accepted; want rejected", v)
			}
		})
	}
}
