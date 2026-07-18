package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
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

// TestRedactRawTOML asserts the raw config-file view redacts Sensitive values
// across TOML spacing/header variants while preserving non-sensitive text,
// comments, and formatting (Codoki High: brittle string matching leaked secrets).
func TestRedactRawTOML(t *testing.T) {
	const secret = "super-secret-token"
	raw := strings.Join([]string{
		"# top-level comment with token = should-not-change",
		"[api]",
		"token=" + secret, // no spaces around '='
		"cooldown = 60",
		"",
		"[[server]]", // array-of-tables header
		"webhook_api_keys = [\"k1\", \"k2\"]",
		"# api.token = inline-comment-must-survive",
	}, "\n")

	got := redactRawTOML(raw)

	if strings.Contains(got, secret) {
		t.Errorf("secret leaked through redaction:\n%s", got)
	}
	// no-space key=value under [api] is redacted.
	if !strings.Contains(got, `token = "(redacted)"`) {
		t.Errorf("no-space api.token not redacted:\n%s", got)
	}
	// [[server]] array-of-tables header still resolves the server section.
	if !strings.Contains(got, `webhook_api_keys = "(redacted)"`) {
		t.Errorf("webhook_api_keys under [[server]] not redacted:\n%s", got)
	}
	// Non-sensitive value is preserved verbatim.
	if !strings.Contains(got, "cooldown = 60") {
		t.Errorf("non-sensitive cooldown altered:\n%s", got)
	}
	// Comment lines are preserved verbatim, including ones mentioning sensitive keys.
	if !strings.Contains(got, "# top-level comment with token = should-not-change") {
		t.Errorf("top-level comment altered:\n%s", got)
	}
	if !strings.Contains(got, "# api.token = inline-comment-must-survive") {
		t.Errorf("commented api.token line was redacted/altered:\n%s", got)
	}
	// Headers themselves survive.
	if !strings.Contains(got, "[api]") || !strings.Contains(got, "[[server]]") {
		t.Errorf("section headers not preserved:\n%s", got)
	}
}

// TestRedactRawTOMLEdgeCases covers the #367 redaction edge cases: trailing
// inline comments on sensitive lines (quote-aware), quoted keys, dotted/sub-table
// key paths, multi-line array values, and whitespace/indentation variants. Every
// case asserts the secret never survives AND that comments, formatting, headers,
// and non-sensitive text are preserved.
func TestRedactRawTOMLEdgeCases(t *testing.T) {
	const secret = "super-secret-token"
	tests := []struct {
		name        string
		in          string
		wantContain []string // substrings that must appear in the output
		wantAbsent  []string // substrings that must NOT appear (beyond the secret)
	}{
		{
			name:        "trailing comment on sensitive line survives",
			in:          "[api]\ntoken = \"" + secret + "\" # rotate me monthly",
			wantContain: []string{`token = "(redacted)" # rotate me monthly`},
		},
		{
			name:        "hash inside quoted value is not a comment",
			in:          "[api]\ntoken = \"a#b#c\"",
			wantContain: []string{`token = "(redacted)"`},
			// The redacted line must not carry a spurious trailing comment built
			// from the '#' that lived inside the quoted secret.
			wantAbsent: []string{"# "},
		},
		{
			name:        "comment after sensitive value with literal-string hash",
			in:          "[api]\ntoken = 'p#ss' # real comment",
			wantContain: []string{`token = "(redacted)" # real comment`},
		},
		{
			name:        "no-space key with trailing comment",
			in:          "[api]\ntoken=" + secret + " # note",
			wantContain: []string{`token = "(redacted)" # note`},
		},
		{
			name:        "quoted key under section is redacted",
			in:          "[api]\n\"token\" = \"" + secret + "\"",
			wantContain: []string{`"token" = "(redacted)"`},
		},
		{
			name:        "dotted sub-table key path is redacted",
			in:          "[server]\nwebhook_api_keys = [\"" + secret + "\"]",
			wantContain: []string{`webhook_api_keys = "(redacted)"`},
		},
		{
			name:        "indentation preserved on sensitive line",
			in:          "[api]\n\ttoken = \"" + secret + "\"",
			wantContain: []string{"\ttoken = \"(redacted)\""},
		},
		{
			name: "non-sensitive multi-line array preserved verbatim",
			in: strings.Join([]string{
				"[guard]",
				"accepted_scripts = [",
				"  \"Latin\",",
				"  \"Han\",",
				"]",
			}, "\n"),
			wantContain: []string{
				"accepted_scripts = [",
				"  \"Latin\",",
				"  \"Han\",",
				"]",
			},
		},
		{
			name:        "non-sensitive value with hash-bearing comment preserved",
			in:          "[server]\naddr = \"127.0.0.1:8080\" # bind host:port",
			wantContain: []string{`addr = "127.0.0.1:8080" # bind host:port`},
		},
		{
			// A sensitive value written as a multi-line array must collapse to the
			// single redacted line; every continuation element is dropped, never
			// emitted verbatim. This is the leak the prior suite missed: it only
			// exercised a NON-sensitive multi-line array.
			name: "sensitive multi-line array does not leak continuation elements",
			in: strings.Join([]string{
				"[server]",
				"webhook_api_keys = [",
				"  \"" + secret + "-1\",",
				"  \"" + secret + "-2\",",
				"]",
				"addr = \"127.0.0.1:8080\"",
			}, "\n"),
			wantContain: []string{
				`webhook_api_keys = "(redacted)"`,
				// Lines after the closing ']' are unaffected.
				`addr = "127.0.0.1:8080"`,
			},
			wantAbsent: []string{
				secret + "-1",
				secret + "-2",
				// The opened array bracket must not survive on the redacted line.
				`webhook_api_keys = "(redacted)" [`,
			},
		},
		{
			// A ']' inside a quoted array element must not be read as the array's
			// close, or the redaction would stop short and leak later elements.
			name: "sensitive multi-line array with bracket inside quoted element",
			in: strings.Join([]string{
				"[server]",
				"webhook_api_keys = [",
				"  \"a]b" + secret + "\",",
				"  \"" + secret + "-tail\",",
				"]",
			}, "\n"),
			wantContain: []string{`webhook_api_keys = "(redacted)"`},
			wantAbsent: []string{
				"a]b" + secret,
				secret + "-tail",
			},
		},
		{
			// A table header with a trailing inline comment must still update the
			// current section. Otherwise the following sensitive key is keyed at
			// top level, misses the registry match, and leaks its value verbatim.
			name: "table header with trailing comment still scopes the section",
			in: strings.Join([]string{
				"[server] # bind + webhook config",
				"webhook_api_keys = [\"" + secret + "-1\", \"" + secret + "-2\"]",
			}, "\n"),
			wantContain: []string{
				"[server] # bind + webhook config",
				`webhook_api_keys = "(redacted)"`,
			},
			wantAbsent: []string{
				secret + "-1",
				secret + "-2",
			},
		},
		{
			// A sensitive multi-line basic string must collapse to the single
			// redacted line; its body lines (the secret) are dropped, never
			// emitted verbatim.
			name: "sensitive multi-line basic string does not leak body",
			in: strings.Join([]string{
				"[api]",
				`token = """`,
				secret + "-line1",
				secret + "-line2",
				`"""`,
				"cooldown = 60",
			}, "\n"),
			wantContain: []string{
				`token = "(redacted)"`,
				"cooldown = 60",
			},
			wantAbsent: []string{
				secret + "-line1",
				secret + "-line2",
			},
		},
		{
			// Same leak for a sensitive multi-line literal string ('''...''').
			name: "sensitive multi-line literal string does not leak body",
			in: strings.Join([]string{
				"[api]",
				"token = '''",
				secret + "-lit1",
				secret + "-lit2",
				"'''",
				"cooldown = 60",
			}, "\n"),
			wantContain: []string{
				`token = "(redacted)"`,
				"cooldown = 60",
			},
			wantAbsent: []string{
				secret + "-lit1",
				secret + "-lit2",
			},
		},
		{
			// A NON-sensitive multi-line string must render verbatim: the body
			// lines are preserved and nothing is over-redacted.
			name: "non-sensitive multi-line string preserved verbatim",
			in: strings.Join([]string{
				"[guard]",
				`note = """`,
				"first body line",
				"second body line",
				`"""`,
			}, "\n"),
			wantContain: []string{
				`note = """`,
				"first body line",
				"second body line",
				`"""`,
			},
		},
		{
			// A sensitive triple-quote string that opens AND closes on the same
			// line must not swallow the following (non-secret) line.
			name: "sensitive single-line triple-quote does not swallow next line",
			in: strings.Join([]string{
				"[api]",
				`token = """` + secret + `"""`,
				"cooldown = 60",
			}, "\n"),
			wantContain: []string{
				`token = "(redacted)"`,
				"cooldown = 60",
			},
			wantAbsent: []string{secret},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactRawTOML(tc.in)
			if strings.Contains(got, secret) {
				t.Errorf("secret leaked:\n%s", got)
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("unexpected %q in:\n%s", absent, got)
				}
			}
		})
	}
}

// TestTrailingTOMLComment covers the quote-aware comment extractor that keeps a
// trailing inline comment while never mistaking a '#' inside a quoted string for
// a comment (#367).
func TestTrailingTOMLComment(t *testing.T) {
	tests := []struct {
		in          string
		wantComment string
	}{
		{` "abc" # trailing`, "# trailing"},
		{` "a#b"`, ""},                     // hash inside a basic string
		{` 'a#b'`, ""},                     // hash inside a literal string
		{` "a\"#b" # real`, "# real"},      // escaped quote does not end the string
		{` "abc"`, ""},                     // no comment
		{` 60`, ""},                        // bare value
		{` ["k1", "k2"] # keys`, "# keys"}, // array with trailing comment
		{` "with # inside" # outside`, "# outside"},
	}
	for _, tc := range tests {
		if got := trailingTOMLComment(tc.in); got != tc.wantComment {
			t.Errorf("trailingTOMLComment(%q) = %q, want %q", tc.in, got, tc.wantComment)
		}
	}
}

// TestBuildRawFileTOML covers buildRawFileTOML's three outcomes (#367): an
// unconfigured path returns ("", nil); a wired-but-unreadable path returns a
// non-nil error so the page can show a distinct read-error state; a readable
// file returns its redacted contents.
func TestBuildRawFileTOML(t *testing.T) {
	t.Run("unconfigured path returns empty without error", func(t *testing.T) {
		u := NewUI(config.Config{}, "v0")
		raw, err := u.buildRawFileTOML()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if raw != "" {
			t.Errorf("raw = %q, want empty", raw)
		}
	})

	t.Run("unreadable path returns error", func(t *testing.T) {
		// A path inside a nonexistent directory cannot be read.
		missing := filepath.Join(t.TempDir(), "nope", "config.toml")
		u := NewUI(config.Config{}, "v0", WithConfigPath(missing))
		raw, err := u.buildRawFileTOML()
		if err == nil {
			t.Fatal("expected a read error, got nil")
		}
		if raw != "" {
			t.Errorf("raw = %q, want empty on error", raw)
		}
	})

	t.Run("readable file returns redacted contents", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		const secret = "file-secret-token"
		content := "[api]\ntoken = \"" + secret + "\"\ncooldown = 60\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		u := NewUI(config.Config{}, "v0", WithConfigPath(path))
		raw, err := u.buildRawFileTOML()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(raw, secret) {
			t.Errorf("secret leaked from file view:\n%s", raw)
		}
		if !strings.Contains(raw, `token = "(redacted)"`) {
			t.Errorf("token not redacted:\n%s", raw)
		}
		if !strings.Contains(raw, "cooldown = 60") {
			t.Errorf("non-sensitive value altered:\n%s", raw)
		}
	})
}

// TestSettingsRawFileReadErrorRendered asserts the settings page renders the
// read-error state distinctly from the unconfigured/empty state when the wired
// config file cannot be read (#367): the error message shows and no empty raw
// <pre> is silently presented in its place.
func TestSettingsRawFileReadErrorRendered(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "config.toml")
	u := NewUI(config.Config{}, "v0", WithConfigPath(missing))
	view := u.buildSettingsView(config.Config{})
	if view.RawFileTOMLError == "" {
		t.Fatal("RawFileTOMLError empty; read failure was not surfaced")
	}
	if view.RawFileTOML != "" {
		t.Errorf("RawFileTOML = %q, want empty on read error", view.RawFileTOML)
	}
}
