package web

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// detectorOrderingTestUI seeds a minimal config and returns a writable settings
// handler plus the config path, mirroring scheduleTestUI.
func detectorOrderingTestUI(t *testing.T, seed string) (http.Handler, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, cfgPath
}

// readConfig reads the config file, failing the test if the read errors.
//
// The error is not ignorable here: bytes.Equal(nil, nil) is TRUE, so a pair of
// failed reads would make the "config was not mutated" assertion pass without
// comparing anything -- and that assertion is the whole safety claim these
// rejection tests exist to make (Copilot, PR #846).
func readConfig(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // reason: G304: test temp path
	if err != nil {
		t.Fatalf("read config %s: %v", path, err)
	}
	return b
}

// frontOrderedSeed is the config an operator legitimately runs: a detector-first
// ordering, which is only meaningful while dispatch is ordered.
const frontOrderedSeed = "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
	"[providers]\nmode = \"ordered\"\n\n" +
	"[instrumental_detector]\nordering = \"front\"\n"

// TestSaveFieldRejectsParallelModeUnderFrontOrdering is the reported defect
// (#845). Both fields are Safe-tier and hot-save individually, so flipping mode
// to parallel is one click -- and the resulting pair is exactly what
// config.Load refuses to boot on. The save must be refused and the file left
// byte-identical.
func TestSaveFieldRejectsParallelModeUnderFrontOrdering(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, frontOrderedSeed)

	before := readConfig(t, cfgPath)
	rec := postField(t, mux, url.Values{
		"path":  {"providers.mode"},
		"value": {"parallel"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (parallel mode under front ordering); body=%s", rec.Code, rec.Body.String())
	}
	after := readConfig(t, cfgPath)
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected field save:\n%s", after)
	}

	// The actual contract: reject exactly what Load would reject, so the config
	// on disk still boots.
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("config no longer loads after a rejected save: %v", err)
	}
}

// TestSaveFieldRejectsFrontOrderingUnderParallelMode is the same invariant
// approached from the other field. Whichever half the operator edits last, the
// resulting combination is what matters -- a checker keyed to only one path
// would pass this and still write a non-booting config.
func TestSaveFieldRejectsFrontOrderingUnderParallelMode(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
		"[providers]\nmode = \"parallel\"\n\n" +
		"[instrumental_detector]\nordering = \"demoted\"\n"
	mux, cfgPath := detectorOrderingTestUI(t, seed)

	before := readConfig(t, cfgPath)
	rec := postField(t, mux, url.Values{
		"path":  {"instrumental_detector.ordering"},
		"value": {"front"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (front ordering under parallel mode); body=%s", rec.Code, rec.Body.String())
	}
	after := readConfig(t, cfgPath)
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected field save:\n%s", after)
	}
}

// TestSaveFieldAcceptsParallelModeUnderDemotedOrdering is the positive control.
// Without it the rejections above could equally mean "mode saves never work".
func TestSaveFieldAcceptsParallelModeUnderDemotedOrdering(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
		"[providers]\nmode = \"ordered\"\n\n" +
		"[instrumental_detector]\nordering = \"demoted\"\n"
	mux, cfgPath := detectorOrderingTestUI(t, seed)

	rec := postField(t, mux, url.Values{
		"path":  {"providers.mode"},
		"value": {"parallel"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (parallel is fine while ordering is demoted); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Providers.Mode != "parallel" {
		t.Fatalf("saved providers.mode = %q; want parallel", cfg.Providers.Mode)
	}
}

// TestSaveSectionRejectsFrontOrderingWithParallelMode covers the multi-field
// lane: both halves in ONE batch. A per-field checker cannot see this at all --
// each value is individually valid, and only their combination is fatal.
func TestSaveSectionRejectsFrontOrderingWithParallelMode(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, frontOrderedSeed)

	before := readConfig(t, cfgPath)
	rec := postSection(t, mux, [][2]string{
		{"providers.mode", "parallel"},
		{"instrumental_detector.ordering", "front"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (front + parallel in one batch); body=%s", rec.Code, rec.Body.String())
	}
	after := readConfig(t, cfgPath)
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("config no longer loads after a rejected save: %v", err)
	}
}

// TestSaveSectionAcceptsAPairThatResolvesTheConflict is the batch positive
// control, and the case the resulting-state read exists for: switching to
// parallel is legal precisely because the SAME batch demotes the ordering. A
// checker that judged either field alone against the CURRENT config would
// wrongly refuse this.
func TestSaveSectionAcceptsAPairThatResolvesTheConflict(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, frontOrderedSeed)

	rec := postSection(t, mux, [][2]string{
		{"providers.mode", "parallel"},
		{"instrumental_detector.ordering", "demoted"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the batch demotes the ordering as it parallelizes); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Providers.Mode != "parallel" || cfg.InstrumentalDetector.Ordering != "demoted" {
		t.Fatalf("saved mode/ordering = %q/%q; want parallel/demoted",
			cfg.Providers.Mode, cfg.InstrumentalDetector.Ordering)
	}
}
