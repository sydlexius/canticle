package config

import (
	"os"
	"path/filepath"
	"testing"
)

// assertFrontParallel fails unless cfg holds a detector-first ordering together
// with parallel dispatch.
func assertFrontParallel(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.InstrumentalDetector.Ordering != detectorOrderingFront {
		t.Errorf("instrumental_detector.ordering = %q, want %q", cfg.InstrumentalDetector.Ordering, detectorOrderingFront)
	}
	if cfg.Providers.Mode != providersModeParallel {
		t.Errorf("providers.mode = %q, want %q", cfg.Providers.Mode, providersModeParallel)
	}
}

// TestLoad_AcceptsFrontOrderingWithParallelMode verifies a config file that
// asks for the detector first under parallel dispatch loads with both values
// intact. Parallel dispatch stages the detector outside the provider race
// (#528), so the pair is a supported configuration.
func TestLoad_AcceptsFrontOrderingWithParallelMode(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[providers]\nmode = \"parallel\"\n\n[instrumental_detector]\nordering = \"front\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with front ordering + parallel mode: %v", err)
	}
	assertFrontParallel(t, cfg)
}

// TestLoadWithSources_AcceptsFrontOrderingWithParallelModeFromEnv is the same
// pair arriving through the environment overrides.
func TestLoadWithSources_AcceptsFrontOrderingWithParallelModeFromEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_ORDERING", "front")
	t.Setenv("MXLRC_PROVIDERS_MODE", "parallel")

	cfg, _, err := LoadWithSources("")
	if err != nil {
		t.Fatalf("LoadWithSources with front ordering + parallel mode via env: %v", err)
	}
	assertFrontParallel(t, cfg)
}
