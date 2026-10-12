package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// TestLoad_OrderingFromFile pins what the loader does with the file's
// instrumental_detector.ordering (#1507): a valid value is trimmed and
// lowercased, as `config set` and the env override do; an unrecognized one
// falls back to the default and the warning names the key and the value; a
// blank one falls back with nothing logged.
func TestLoad_OrderingFromFile(t *testing.T) {
	const warning = "instrumental_detector.ordering is not front or demoted"
	for _, tc := range []struct {
		name, value, want string
		warn              bool
	}{
		{"valid", "front", detectorOrderingFront, false},
		{"mixed case, padded", " FrOnT ", detectorOrderingFront, false},
		{"unrecognized", "first", detectorOrderingDemoted, true},
		{"blank", "", detectorOrderingDemoted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[instrumental_detector]\nordering = \"" + tc.value + "\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.InstrumentalDetector.Ordering != tc.want {
				t.Errorf("ordering = %q, want %q", cfg.InstrumentalDetector.Ordering, tc.want)
			}
			logged := buf.String()
			if got := strings.Contains(logged, warning); got != tc.warn {
				t.Errorf("warning logged = %v, want %v; log:\n%s", got, tc.warn, logged)
			}
			if tc.warn && (!strings.Contains(logged, "key=instrumental_detector.ordering") || !strings.Contains(logged, "value=first")) {
				t.Errorf("warning does not name the key and the rejected value:\n%s", logged)
			}
		})
	}
}

// TestAllowedValues_DetectorOrdering: the settings page renders its dropdown
// from this list, default first.
func TestAllowedValues_DetectorOrdering(t *testing.T) {
	got := AllowedValues("instrumental_detector.ordering")
	if len(got) != 2 || got[0] != detectorOrderingDemoted || got[1] != detectorOrderingFront {
		t.Errorf("AllowedValues = %v; want [demoted front]", got)
	}
}
