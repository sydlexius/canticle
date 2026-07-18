package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// secretCfg returns a config carrying both secret-bearing fields populated, so
// redaction tests have something to mask.
func secretCfg() config.Config {
	cfg := config.Config{}
	cfg.API.Token = "tok_SUPERSECRET_TOKEN_VALUE"
	cfg.Server.WebhookAPIKeys = []string{"mxlrc_secretkey_one", "mxlrc_secretkey_two"}
	return cfg
}

// newUIServer registers the UI routes on a fresh mux for httptest.
func newUIServer(cfg config.Config, version string) *http.ServeMux {
	mux := http.NewServeMux()
	NewUI(cfg, version).Register(mux)
	return mux
}

// TestConfigViewRedactsSecrets is the required in-package proof that the Raw
// config tab of the Settings page (which absorbed the old read-only Config view)
// never emits a secret-bearing value. It populates the token and webhook keys,
// renders the live /settings response, and asserts the raw secrets are absent
// while the redaction marker is present.
func TestConfigViewRedactsSecrets(t *testing.T) {
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
			t.Errorf("Config view leaked secret %q", secret)
		}
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Error("Config view missing [REDACTED] marker; expected redacted token/webhook keys")
	}
}

// TestConfigViewEmptyTokenNotSet confirms an unset token renders as "(not set)"
// rather than "[REDACTED]" in the Raw config tab, so an operator can tell unset
// from configured.
func TestConfigViewEmptyTokenNotSet(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "token = (not set)") {
		t.Errorf("empty token should render as (not set); body did not contain it")
	}
}

// TestActiveNavHighlight verifies the sidebar marks exactly the right row active
// with aria-current="page", which drives the active-item accent styling. The
// Settings page highlights the Settings link; the Reports landing (no report
// selected) highlights nothing.
func TestActiveNavHighlight(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	t.Run("settings marks Settings active", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /settings status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `href="/settings" class="mx-nav-link" aria-current="page"`) {
			t.Error("settings page did not mark the Settings nav link active")
		}
		// Exactly one row is active: Settings, not any report row.
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("settings page should have exactly one active nav row, got %d", n)
		}
	})

	t.Run("reports landing marks nothing active", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/reports", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /reports status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		// No report is selected and Config is not the active page, so no row is
		// marked active.
		if strings.Contains(body, `aria-current="page"`) {
			t.Error("reports landing should mark no nav row active before a report is selected")
		}
	})
}

// TestShellRendersLogoutControl verifies the shared sidebar shell renders a
// sign-out control that POSTs to /logout on every authed page (issue #204, lane
// 4 UAT). /logout is POST-only, so a GET anchor would be unreachable; the
// control must be a form submit.
func TestShellRendersLogoutControl(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	for _, path := range []string{"/settings", "/reports"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
			}
			body := rec.Body.String()
			for _, want := range []string{`method="post"`, `action="/logout"`, "Sign out", `type="submit"`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: shell missing logout control fragment %q", path, want)
				}
			}
		})
	}
}

// TestRootRedirectsToDashboard checks the bare root sends operators to the
// Dashboard, the default landing page after authentication.
func TestRootRedirectsToDashboard(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET / status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("GET / Location = %q, want /dashboard", loc)
	}
}

// TestConfigRedirectsToSettings confirms the retired /config route still
// resolves: it permanently redirects to /settings so old links and bookmarks
// keep working.
func TestConfigRedirectsToSettings(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /config status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings" {
		t.Errorf("GET /config Location = %q, want /settings", loc)
	}
}

// TestReportsWorkspaceShell confirms the Reports page renders the true two-pane
// shell: the sidebar's REPORTS group lists the five canned reports in design-doc
// order, each report row wires an htmx GET to its on-demand fragment route, the
// default content pane shows the "select a report" placeholder (nothing runs on
// load), and the version reaches the sidebar.
func TestReportsWorkspaceShell(t *testing.T) {
	mux := newUIServer(config.Config{}, "v1.2.3")

	req := httptest.NewRequest(http.MethodGet, "/reports", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reports status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Rail items in design-doc order: each title must appear, and its left-to-
	// right position must be monotonic so the order is exactly as specified.
	wantOrder := []string{
		"Queue summary",
		"Recent outcomes",
		"Provider effectiveness",
		"Instrumental inventory",
		"Failure analysis",
	}
	prev := -1
	for _, title := range wantOrder {
		idx := strings.Index(body, title)
		if idx < 0 {
			t.Errorf("rail missing report %q", title)
			continue
		}
		if idx <= prev {
			t.Errorf("report %q is out of design-doc order", title)
		}
		prev = idx
	}

	// On-demand wiring: each report's htmx GET target must be present.
	for _, key := range []string{
		"queue-summary", "recent-outcomes", "provider-effectiveness",
		"instrumental-inventory", "failure-analysis",
	} {
		if !strings.Contains(body, `hx-get="/reports/`+key+`"`) {
			t.Errorf("rail item %q missing hx-get wiring", key)
		}
	}

	// Default pane: a placeholder, not a rendered table (no query runs on load).
	if !strings.Contains(body, "Select a report") {
		t.Error("default pane missing the select-a-report placeholder")
	}
	// Reports are nested in the sidebar under the REPORTS group heading.
	if !strings.Contains(body, `id="mx-reports-heading"`) {
		t.Error("sidebar missing the REPORTS group heading")
	}
	// HARD CONSTRAINT: no polling / auto-refresh anywhere on the shell.
	if strings.Contains(body, "hx-trigger") {
		t.Error("reports shell must not use any hx-trigger (no polling/SSE/auto-refresh)")
	}
	if !strings.Contains(body, "v1.2.3") {
		t.Error("sidebar did not render the version string")
	}
}
