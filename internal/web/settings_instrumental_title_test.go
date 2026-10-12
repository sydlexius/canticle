package web

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

var titleModeSelectedRE = regexp.MustCompile(`(?s)<select[^>]*instrumental_title\.mode.*?</select>`)

func TestSettingsInstrumentalTitleModeSelect(t *testing.T) {
	cfg := config.Config{InstrumentalTitle: config.InstrumentalTitleConfig{Mode: "strong"}}
	if got := rawConfigValue(cfg, "instrumental_title.mode"); got != "strong" {
		t.Fatalf("rawConfigValue = %q; want strong", got)
	}
	sel := titleModeSelectedRE.FindString(renderSettings(t, cfg))
	if sel == "" {
		t.Fatal("no select for instrumental_title.mode on the settings page")
	}
	if !regexp.MustCompile(`<option[^>]*value="strong"[^>]*selected|<option[^>]*selected[^>]*value="strong"`).MatchString(sel) {
		t.Errorf("strong is not selected:\n%s", sel)
	}
}

func TestSaveInstrumentalTitleMode(t *testing.T) {
	mux, cfgPath := detectorOrderingTestUI(t, "[server]\naddr = \"127.0.0.1:3876\"\n")

	rec := postField(t, mux, url.Values{"path": {"instrumental_title.mode"}, "value": {"strong"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.InstrumentalTitle.Mode != "strong" {
		t.Fatalf("reloaded mode = %q; want strong", cfg.InstrumentalTitle.Mode)
	}

	before := readConfig(t, cfgPath)
	rec = postField(t, mux, url.Values{"path": {"instrumental_title.mode"}, "value": {"bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus status = %d, want 400", rec.Code)
	}
	if !bytes.Equal(before, readConfig(t, cfgPath)) {
		t.Error("config mutated on a rejected save")
	}
}

func TestSettingsInstrumentalTitleModeLegacyAliasSelectsAll(t *testing.T) {
	cfg := config.Config{InstrumentalTitle: config.InstrumentalTitleConfig{Mode: "strong_weak"}}
	sel := titleModeSelectedRE.FindString(renderSettings(t, cfg))
	if !regexp.MustCompile(`<option[^>]*value="all"[^>]*selected|<option[^>]*selected[^>]*value="all"`).MatchString(sel) {
		t.Errorf("all is not selected for a strong_weak config:\n%s", sel)
	}
	if regexp.MustCompile(`value="strong_weak"`).MatchString(sel) {
		t.Errorf("the dropdown offers the deprecated spelling:\n%s", sel)
	}
}

// TestSaveInstrumentalTitleModeLegacyAliasStoresAll covers both save lanes:
// the alias is accepted and the file holds the canonical value, not the typed one.
func TestSaveInstrumentalTitleModeLegacyAliasStoresAll(t *testing.T) {
	seed := "[server]\naddr = \"127.0.0.1:3876\"\n"
	for name, post := range map[string]func(*testing.T, http.Handler) int{
		"field": func(t *testing.T, mux http.Handler) int {
			return postField(t, mux, url.Values{"path": {"instrumental_title.mode"}, "value": {"strong_weak"}}).Code
		},
		"section": func(t *testing.T, mux http.Handler) int {
			return postSection(t, mux, [][2]string{{"instrumental_title.mode", "strong_weak"}}).Code
		},
	} {
		t.Run(name, func(t *testing.T) {
			mux, cfgPath := detectorOrderingTestUI(t, seed)
			if code := post(t, mux); code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			raw := readConfig(t, cfgPath)
			if !bytes.Contains(raw, []byte(`mode = "all"`)) || bytes.Contains(raw, []byte("strong_weak")) {
				t.Errorf("file does not hold the canonical value:\n%s", raw)
			}
		})
	}
}
