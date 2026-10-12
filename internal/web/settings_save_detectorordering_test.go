package web

import (
	"net/http"
	"net/http/httptest"
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
// failed reads would make a "config was not mutated" assertion pass without
// comparing anything (Copilot, PR #846).
func readConfig(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // reason: G304: test temp path
	if err != nil {
		t.Fatalf("read config %s: %v", path, err)
	}
	return b
}

// frontOrderedSeed is a detector-first ordering under ordered dispatch.
const frontOrderedSeed = "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
	"[providers]\nmode = \"ordered\"\n\n" +
	"[instrumental_detector]\nordering = \"front\"\n"

// assertSavedFrontParallel fails unless the save answered 200 and the reloaded
// config holds a detector-first ordering together with parallel dispatch.
// Parallel dispatch stages the detector outside the provider race (#528), so the
// pair is a supported configuration.
func assertSavedFrontParallel(t *testing.T, rec *httptest.ResponseRecorder, cfgPath string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (front ordering with parallel mode); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Providers.Mode != "parallel" || cfg.InstrumentalDetector.Ordering != "front" {
		t.Fatalf("saved mode/ordering = %q/%q; want parallel/front",
			cfg.Providers.Mode, cfg.InstrumentalDetector.Ordering)
	}
}

// TestSaveFieldAcceptsParallelModeUnderFrontOrdering saves the dispatch mode
// alone while the stored ordering is already detector-first.
func TestSaveFieldAcceptsParallelModeUnderFrontOrdering(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, frontOrderedSeed)

	rec := postField(t, mux, url.Values{
		"path":  {"providers.mode"},
		"value": {"parallel"},
	})
	assertSavedFrontParallel(t, rec, cfgPath)
}

// TestSaveFieldAcceptsFrontOrderingUnderParallelMode reaches the same pair by
// saving the other field.
func TestSaveFieldAcceptsFrontOrderingUnderParallelMode(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
		"[providers]\nmode = \"parallel\"\n\n" +
		"[instrumental_detector]\nordering = \"demoted\"\n"
	mux, cfgPath := detectorOrderingTestUI(t, seed)

	rec := postField(t, mux, url.Values{
		"path":  {"instrumental_detector.ordering"},
		"value": {"front"},
	})
	assertSavedFrontParallel(t, rec, cfgPath)
}

// TestSaveSectionAcceptsFrontOrderingWithParallelMode covers the multi-field
// lane: both values arrive in one batch.
func TestSaveSectionAcceptsFrontOrderingWithParallelMode(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n" +
		"[providers]\nmode = \"ordered\"\n\n" +
		"[instrumental_detector]\nordering = \"demoted\"\n"
	mux, cfgPath := detectorOrderingTestUI(t, seed)

	rec := postSection(t, mux, [][2]string{
		{"providers.mode", "parallel"},
		{"instrumental_detector.ordering", "front"},
	})
	assertSavedFrontParallel(t, rec, cfgPath)
}
