package config

import "testing"

// The backfill sweep's env overrides (#708). Each block has a validation arm
// that silently keeps the current value on a bad input, so an untested block
// could reject every value an operator sets and only ever log a warning.
func TestBackfillEnvOverrides(t *testing.T) {
	t.Run("valid values are applied", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_ENABLED", "false")
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_BATCH_SIZE", "250")
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_INTERVAL_MINUTES", "15")
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_COOLDOWN_SECONDS", "3")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		bf := cfg.InstrumentalDetector.Backfill
		if bf.Enabled {
			t.Error("enabled = true; the env said false, so an operator could not turn the sweep off")
		}
		if bf.BatchSize != 250 || bf.IntervalMinutes != 15 || bf.CooldownSeconds != 3 {
			t.Errorf("bounds = (batch %d, interval %d, cooldown %d); want (250, 15, 3)",
				bf.BatchSize, bf.IntervalMinutes, bf.CooldownSeconds)
		}
	})

	// A zero cooldown is the DOCUMENTED DEFAULT (a contiguous burst), not a
	// missing value, so it must survive. The batch/interval blocks reject < 1 and
	// this one must not copy that rule.
	t.Run("zero cooldown is honored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_COOLDOWN_SECONDS", "0")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.InstrumentalDetector.Backfill.CooldownSeconds; got != 0 {
			t.Errorf("cooldown = %d; want 0 preserved -- it is a real value, not an unset one", got)
		}
	})

	// Each invalid input must leave the DEFAULT standing rather than writing a
	// value that would break the sweep: a zero/negative batch classifies nothing,
	// and a zero/negative interval panics time.NewTicker.
	t.Run("invalid values keep the defaults", func(t *testing.T) {
		for _, tc := range []struct{ name, env, val string }{
			{"batch zero", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_BATCH_SIZE", "0"},
			{"batch negative", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_BATCH_SIZE", "-5"},
			{"batch non-numeric", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_BATCH_SIZE", "lots"},
			{"interval zero", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_INTERVAL_MINUTES", "0"},
			{"interval negative", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_INTERVAL_MINUTES", "-1"},
			{"interval non-numeric", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_INTERVAL_MINUTES", "hourly"},
			{"cooldown negative", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_COOLDOWN_SECONDS", "-1"},
			{"cooldown non-numeric", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_COOLDOWN_SECONDS", "slow"},
			{"enabled non-boolean", "MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_ENABLED", "yesplease"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				isolateEnv(t)
				t.Setenv(tc.env, tc.val)

				cfg, err := Load("")
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				bf := cfg.InstrumentalDetector.Backfill
				if !bf.Enabled {
					t.Error("enabled = false; an unparsable value must not disable the sweep")
				}
				if bf.BatchSize != detectorBackfillBatchSizeDefault {
					t.Errorf("batch = %d; want the %d default -- a bad value must not leave a batch that classifies nothing",
						bf.BatchSize, detectorBackfillBatchSizeDefault)
				}
				if bf.IntervalMinutes != detectorBackfillIntervalMinutesDefault {
					t.Errorf("interval = %d; want the %d default -- a non-positive interval PANICS time.NewTicker",
						bf.IntervalMinutes, detectorBackfillIntervalMinutesDefault)
				}
				if bf.CooldownSeconds < 0 {
					t.Errorf("cooldown = %d; a negative gap is meaningless and must clamp to 0", bf.CooldownSeconds)
				}
			})
		}
	})
}
