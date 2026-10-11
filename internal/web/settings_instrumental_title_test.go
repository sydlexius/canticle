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
