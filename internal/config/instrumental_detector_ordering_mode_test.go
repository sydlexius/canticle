package config

import (
	"strings"
	"testing"
)

// TestValidateInstrumentalDetectorOrdering covers the cross-field constraint
// between instrumental_detector.ordering and providers.mode: "front" ordering
// only delivers its zero-provider-request guarantee in ordered mode (findOrdered
// walks lanes in slice order and short-circuits on a terminal-suitable result).
// In parallel mode every lane is dispatched concurrently regardless of slice
// order, so "front" combined with "parallel" is a contradictory configuration
// that must be rejected at load rather than silently failing to deliver what
// its name promises. "demoted" (the default ordering) must remain accepted
// under both dispatch modes: it is the common case and must never regress.
func TestValidateInstrumentalDetectorOrdering(t *testing.T) {
	cases := []struct {
		name     string
		ordering string
		mode     string
		wantErr  bool
	}{
		{"front + parallel is rejected", detectorOrderingFront, providersModeParallel, true},
		{"front + ordered is accepted", detectorOrderingFront, providersModeDefault, false},
		{"demoted + parallel is accepted (default ordering must never regress)", detectorOrderingDemoted, providersModeParallel, false},
		{"demoted + ordered is accepted", detectorOrderingDemoted, providersModeDefault, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaults()
			cfg.InstrumentalDetector.Ordering = c.ordering
			cfg.Providers.Mode = c.mode
			err := validateInstrumentalDetectorOrdering(cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateInstrumentalDetectorOrdering(ordering=%q, mode=%q) err=%v, wantErr=%v", c.ordering, c.mode, err, c.wantErr)
			}
		})
	}
}

// TestValidateInstrumentalDetectorOrdering_ErrorNamesBothKeys verifies the
// rejection error names both config keys: an operator who cannot tell WHICH
// two settings conflict cannot fix it.
func TestValidateInstrumentalDetectorOrdering_ErrorNamesBothKeys(t *testing.T) {
	cfg := defaults()
	cfg.InstrumentalDetector.Ordering = detectorOrderingFront
	cfg.Providers.Mode = providersModeParallel

	err := validateInstrumentalDetectorOrdering(cfg)
	if err == nil {
		t.Fatal("validateInstrumentalDetectorOrdering(front, parallel) = nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "instrumental_detector.ordering") {
		t.Errorf("error %q does not name instrumental_detector.ordering", msg)
	}
	if !strings.Contains(msg, "providers.mode") {
		t.Errorf("error %q does not name providers.mode", msg)
	}
}

// TestLoadWithSources_RejectsFrontOrderingWithParallelModeFromEnv verifies the
// cross-field check fires through the full LoadWithSources path when BOTH
// conflicting values arrive via env overrides (not just when constructed
// directly on a Config in the unit test above), since validation must run
// after env overrides are resolved, not only after TOML decode.
func TestLoadWithSources_RejectsFrontOrderingWithParallelModeFromEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_ORDERING", "front")
	t.Setenv("MXLRC_PROVIDERS_MODE", "parallel")

	_, _, err := LoadWithSources("")
	if err == nil {
		t.Fatal("LoadWithSources with front ordering + parallel mode via env = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "instrumental_detector.ordering") || !strings.Contains(err.Error(), "providers.mode") {
		t.Errorf("error %q does not name both conflicting keys", err.Error())
	}
}
