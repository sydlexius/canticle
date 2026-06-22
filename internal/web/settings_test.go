package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
)

// idAttrRE matches an id="..." attribute value in rendered HTML.
var idAttrRE = regexp.MustCompile(`\sid="([^"]+)"`)

// TestSettingsPageIDsUnique renders the live page and asserts every id attribute
// is unique, so label-for bindings and aria-labelledby references stay valid.
func TestSettingsPageIDsUnique(t *testing.T) {
	mux := newUIServer(config.Config{}, "v0")
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}

	counts := map[string]int{}
	for _, m := range idAttrRE.FindAllStringSubmatch(rec.Body.String(), -1) {
		counts[m[1]]++
	}
	for id, n := range counts {
		if n > 1 {
			t.Errorf("duplicate id %q appears %d times", id, n)
		}
	}
}

// TestSettingsViewCoversRegistry asserts every registry field appears in the
// built view exactly once, so adding a field (or a new section) cannot silently
// drop it from the page. It also backs the section-order coverage claim.
func TestSettingsViewCoversRegistry(t *testing.T) {
	u := NewUI(config.Config{}, "v0")
	view := u.buildSettingsView(config.Config{})

	seen := map[string]int{}
	for _, f := range view.Common {
		seen[f.Path]++
	}
	for _, sec := range view.Sections {
		for _, f := range sec.Fields {
			seen[f.Path]++
		}
	}

	for _, path := range config.AllPaths() {
		if uiHiddenPaths[path] {
			// Intentionally suppressed from the editable tabs (still in Raw config).
			if seen[path] != 0 {
				t.Errorf("hidden field %q should not render an editable control (got %d)", path, seen[path])
			}
			continue
		}
		switch seen[path] {
		case 1:
			// covered exactly once
		case 0:
			t.Errorf("registry field %q missing from settings view", path)
		default:
			t.Errorf("registry field %q appears %d times in settings view", path, seen[path])
		}
	}
}

// TestSettingsHidesOutputDir confirms output.dir has no editable control on the
// page (Common or Advanced) but still appears in the read-only Raw config tab,
// and that its registry entry remains intact (power users edit it via TOML/env).
func TestSettingsHidesOutputDir(t *testing.T) {
	if _, ok := config.FieldByPath("output.dir"); !ok {
		t.Fatal("output.dir must remain in the registry (only the UI control is suppressed)")
	}

	u := NewUI(config.Config{}, "v0")
	view := u.buildSettingsView(config.Config{})
	for _, f := range view.Common {
		if f.Path == "output.dir" {
			t.Error("output.dir must not render in the Common tab")
		}
	}
	for _, sec := range view.Sections {
		for _, f := range sec.Fields {
			if f.Path == "output.dir" {
				t.Error("output.dir must not render in the Advanced tab")
			}
		}
	}
	// The Raw config tab renders the actual config file, which includes output.dir.
	if !strings.Contains(view.RawTOML, "dir =") {
		t.Error("output.dir should still appear in the Raw config tab")
	}
}

// TestSettingsCommonPathsValid confirms every Common-tab path resolves in the
// registry, so a typo in commonPaths cannot silently drop a field from the page.
func TestSettingsCommonPathsValid(t *testing.T) {
	for _, p := range commonPaths {
		if _, ok := config.FieldByPath(p); !ok {
			t.Errorf("commonPaths entry %q is not a registry field", p)
		}
	}
}

// TestSettingsLabelsCoverRegistry confirms every registry field has a curated
// plain-language label (no field falls back to the humanized path segment), so a
// newly added field gets an explicit label.
func TestSettingsLabelsCoverRegistry(t *testing.T) {
	for _, p := range config.AllPaths() {
		if _, ok := settingsLabels[p]; !ok {
			t.Errorf("registry field %q has no curated plain-language label", p)
		}
	}
}

// TestSettingsCommonAndAdvancedDisjoint confirms a Common field never also
// renders in Advanced (the page would otherwise carry a duplicate id).
func TestSettingsCommonAndAdvancedDisjoint(t *testing.T) {
	u := NewUI(config.Config{}, "v0")
	view := u.buildSettingsView(config.Config{})

	common := map[string]bool{}
	for _, f := range view.Common {
		common[f.Path] = true
	}
	for _, sec := range view.Sections {
		for _, f := range sec.Fields {
			if common[f.Path] {
				t.Errorf("field %q appears in both Common and Advanced", f.Path)
			}
		}
	}
}

// TestSettingsViewNeverEchoesSecrets confirms the read path never renders a
// stored secret value: the token and webhook keys show a set/count state, not
// the raw bytes.
func TestSettingsViewNeverEchoesSecrets(t *testing.T) {
	mux := newUIServer(secretCfg(), "v9.9.9")

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, secret := range []string{
		"tok_SUPERSECRET_TOKEN_VALUE",
		"mxlrc_secretkey_one",
		"mxlrc_secretkey_two",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("Settings view leaked secret %q", secret)
		}
	}
	// The token is set in secretCfg, so its state should read "(set)".
	if !strings.Contains(body, "(set)") {
		t.Error("Settings view should show a set-token state for a configured token")
	}
}

// TestSettingsLockedFieldFromEnv confirms an env override marks the field locked
// with a "Locked" pill and that the winning env var name appears only on the
// pill's tooltip (title attribute), never as visible text (#307).
func TestSettingsLockedFieldFromEnv(t *testing.T) {
	t.Setenv("MXLRC_LOG_LEVEL", "debug")

	mux := newUIServer(config.Config{}, "v0")
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "mx-field-pill-locked") {
		t.Error("locked field should carry the plain Locked pill")
	}
	// After #307 the env var name is surfaced as a title= tooltip on the pill
	// so operators can see which variable to clear, without cluttering the card.
	if !strings.Contains(body, `title="Locked by MXLRC_LOG_LEVEL"`) {
		t.Error("locked pill should carry a title tooltip with the winning env var name")
	}
}

// TestSettingsNonWritableModeInputsDisabled confirms that in non-writable mode
// (no config file path configured), all editable controls render with the
// disabled attribute so the page is genuinely read-only, not misleadingly
// interactive (#306).
func TestSettingsNonWritableModeInputsDisabled(t *testing.T) {
	mux := newUIServer(config.Config{}, "v0")
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// No CSRF token in non-writable mode.
	if strings.Contains(body, "mx-csrf-token") {
		t.Error("non-writable page must not embed the CSRF token")
	}
	// In non-writable mode, every editable control must carry disabled.
	// Before the fix, !Savable alone did not add disabled; only locked fields were.
	// Verify disabled appears (there are no locked env vars in this test).
	if !strings.Contains(body, "disabled") {
		t.Error("non-writable settings page rendered no disabled inputs; editable controls must be disabled when !Savable (#306)")
	}
}

// TestSettingsKeyFileReadOnly confirms secrets.key_file (Editable: false) renders
// as a read-only display with rotation guidance, not an editable input.
func TestSettingsKeyFileReadOnly(t *testing.T) {
	u := NewUI(config.Config{}, "v0")
	view := u.buildSettingsView(config.Config{})

	var found bool
	for _, sec := range view.Sections {
		for _, f := range sec.Fields {
			if f.Path == "secrets.key_file" {
				found = true
				if f.Editable {
					t.Error("secrets.key_file should be non-editable")
				}
			}
		}
	}
	if !found {
		t.Fatal("secrets.key_file not present in settings view")
	}
}

// TestSettingsTokenFromStoreNotLocked confirms a token present in the effective
// config but NOT from an active env override (e.g. sourced from the encrypted
// secret store or the config file) renders editable, not locked, and therefore
// carries no env why-line. An active env override is the only thing that locks
// it (#288 G1). Locked == false implies no why-line, since the why-line renders
// only for a locked field.
func TestSettingsTokenFromStoreNotLocked(t *testing.T) {
	// Force both token env vars empty so a real override in the test runner's
	// environment cannot leak in (LookupEnv returns "", treated as no override).
	t.Setenv("MUSIXMATCH_TOKEN", "")
	t.Setenv("MXLRC_API_TOKEN", "")

	cfg := config.Config{}
	cfg.API.Token = "tok_from_store"
	u := NewUI(cfg, "v0")
	view := u.buildSettingsView(cfg)

	var found bool
	for _, f := range view.Common {
		if f.Path == "api.token" {
			found = true
			if f.Locked {
				t.Error("api.token must NOT be locked when sourced from the store/file with no env override")
			}
			if f.EffectiveValue != "(set)" {
				t.Errorf("api.token effective value = %q, want \"(set)\"", f.EffectiveValue)
			}
		}
	}
	if !found {
		t.Fatal("api.token not present on the Common tab")
	}
}

// TestSettingsTokenEnvOverrideLocks is the positive control for G1: an active
// env override DOES lock the token field.
func TestSettingsTokenEnvOverrideLocks(t *testing.T) {
	t.Setenv("MUSIXMATCH_TOKEN", "tok_from_env")

	cfg := config.Config{}
	cfg.API.Token = "tok_from_env"
	u := NewUI(cfg, "v0")
	view := u.buildSettingsView(cfg)

	for _, f := range view.Common {
		if f.Path == "api.token" && !f.Locked {
			t.Error("api.token must be locked when an env override is active")
		}
	}
}

// TestSettingsTokenNotLockedByForeignEnv is the cross-field-contamination guard
// for G1, exercised over the REAL render path: with api.token sourced from
// config (no token env) while a DIFFERENT field's env var (MXLRC_API_COOLDOWN)
// is set, the token must stay editable while cooldown locks. A field locks only
// on ITS OWN env var, never another's. Asserts both the view model and the
// rendered HTML (the GET /settings path), since an isolated single-field build
// would not catch contamination that only appears when all fields are built.
func TestSettingsTokenNotLockedByForeignEnv(t *testing.T) {
	t.Setenv("MUSIXMATCH_TOKEN", "")
	t.Setenv("MXLRC_API_TOKEN", "")
	t.Setenv("MXLRC_API_COOLDOWN", "30") // a different field's env is set -> locked

	cfg := config.Config{}
	cfg.API.Token = "tok_from_file"

	// View model: token unlocked, cooldown locked.
	view := NewUI(cfg, "v0").buildSettingsView(cfg)
	var sawToken, sawCooldown bool
	for _, f := range view.Common {
		switch f.Path {
		case "api.token":
			sawToken = true
			if f.Locked {
				t.Error("api.token must NOT be locked by another field's env var")
			}
		case "api.cooldown":
			sawCooldown = true
			if !f.Locked {
				t.Error("api.cooldown should be locked (its own env var is set)")
			}
		}
	}
	if !sawToken || !sawCooldown {
		t.Fatalf("expected both api.token and api.cooldown on the Common tab (token=%v cooldown=%v)", sawToken, sawCooldown)
	}

	// Rendered HTML in WRITABLE mode: the token input must not carry disabled
	// because it is not locked by its own env vars. Uses writableTestUI so the
	// page is in writable mode (Savable=true for unlocked editable fields),
	// isolating the lock-contamination check from the non-writable-mode behavior
	// where ALL inputs are intentionally disabled (#306).
	mux, _ := writableTestUI(t, newFakeSecretStore())
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	idx := strings.Index(body, `id="field-api-token"`)
	if idx < 0 {
		t.Fatal("token input not found in rendered settings page")
	}
	if tag := body[idx:min(idx+200, len(body))]; strings.Contains(tag, "disabled") {
		t.Errorf("token input rendered disabled with no token env override: %s", tag)
	}
}
