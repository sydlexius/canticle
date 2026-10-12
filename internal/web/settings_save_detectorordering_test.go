package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// TestSaveFieldRefusesUnknownDetectorOrdering: a value that is neither "front"
// nor "demoted" is refused on the single-field lane and the file is left
// byte-identical (#1507). Before the validator arm it was written as typed and
// the next load reset it to the default.
func TestSaveFieldRefusesUnknownDetectorOrdering(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, frontOrderedSeed)
	before := readConfig(t, cfgPath)

	rec := postField(t, mux, url.Values{
		"path":  {"instrumental_detector.ordering"},
		"value": {"first"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for ordering \"first\"; body=%s", rec.Code, rec.Body.String())
	}
	if after := readConfig(t, cfgPath); !bytes.Equal(before, after) {
		t.Errorf("config mutated on a refused ordering:\n%s", after)
	}
}

// TestSaveSectionRefusesUnknownDetectorOrdering is the same refusal on the
// multi-field lane, reported against the field (postRejectedBatch asserts the
// 400 and the byte-identical file).
func TestSaveSectionRefusesUnknownDetectorOrdering(t *testing.T) {
	got := postRejectedBatch(t, [][2]string{{"instrumental_detector.ordering", "first"}})
	if got.Errors["instrumental_detector.ordering"] == "" || len(got.Errors) != 1 {
		t.Errorf("errors = %v, want exactly one, for instrumental_detector.ordering", got.Errors)
	}
}

// TestSaveAcceptsMixedCaseDetectorOrdering: both web lanes accept a mixed-case
// spelling, as `config set` does, and the file is written with the lowercase
// one `config set` would have stored.
func TestSaveAcceptsMixedCaseDetectorOrdering(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n[instrumental_detector]\nordering = \"demoted\"\n"
	for name, post := range map[string]func(http.Handler) *httptest.ResponseRecorder{
		"field": func(h http.Handler) *httptest.ResponseRecorder {
			return postField(t, h, url.Values{"path": {"instrumental_detector.ordering"}, "value": {"FrOnT"}})
		},
		"section": func(h http.Handler) *httptest.ResponseRecorder {
			return postSection(t, h, [][2]string{{"instrumental_detector.ordering", "FrOnT"}})
		},
	} {
		mux, cfgPath := detectorOrderingTestUI(t, seed)
		if rec := post(mux); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body=%s", name, rec.Code, rec.Body.String())
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("%s: reload: %v", name, err)
		}
		if cfg.InstrumentalDetector.Ordering != "front" {
			t.Errorf("%s: loaded ordering = %q; want front", name, cfg.InstrumentalDetector.Ordering)
		}
		// The file itself holds the lowercase spelling, not only the loaded value.
		if raw := string(readConfig(t, cfgPath)); !strings.Contains(raw, `ordering = "front"`) {
			t.Errorf("%s: file does not hold ordering = \"front\":\n%s", name, raw)
		}
	}
}

// detectorOrderingSelectRE captures the body of the ordering dropdown.
var detectorOrderingSelectRE = regexp.MustCompile(`(?s)<select[^>]*name="instrumental_detector\.ordering"[^>]*>(.*?)</select>`)

// detectorOrderingOptionRE captures one option: its value, its attributes after
// the value, and its label.
var detectorOrderingOptionRE = regexp.MustCompile(`<option value="([^"]*)"([^>]*)>([^<]*)</option>`)

// TestSettingsPageSelectsStoredDetectorOrdering: the ordering dropdown marks
// the STORED value as selected. With no option marked the browser shows the
// first one ("demoted"), and saving the card would write it over a stored
// "front". The options carry plain labels, not the bare tokens.
func TestSettingsPageSelectsStoredDetectorOrdering(t *testing.T) {
	labels := map[string]string{
		"demoted": "After the lyrics sources miss, except weak instrumental titles",
		"front":   "Before any lyrics source",
	}
	for _, tc := range []struct{ stored, want string }{
		{"front", "front"},
		{"FrOnT", "front"},
		{"demoted", "demoted"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			seed := "[server]\naddr = \"127.0.0.1:3876\"\n\n[instrumental_detector]\nordering = \"" + tc.stored + "\"\n"
			mux, _ := detectorOrderingTestUI(t, seed)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /settings status = %d, want 200", rec.Code)
			}
			sel := detectorOrderingSelectRE.FindStringSubmatch(rec.Body.String())
			if sel == nil {
				t.Fatal("no select named instrumental_detector.ordering on the settings page")
			}
			opts := detectorOrderingOptionRE.FindAllStringSubmatch(sel[1], -1)
			if len(opts) != len(labels) {
				t.Fatalf("options = %d, want %d; select body:\n%s", len(opts), len(labels), sel[1])
			}
			for _, o := range opts {
				value, selected, label := o[1], strings.Contains(o[2], "selected"), o[3]
				if selected != (value == tc.want) {
					t.Errorf("stored %q: option %q selected = %v, want %v", tc.stored, value, selected, value == tc.want)
				}
				if want, ok := labels[value]; !ok || label != want {
					t.Errorf("option %q label = %q, want %q", value, label, want)
				}
			}
		})
	}
}
