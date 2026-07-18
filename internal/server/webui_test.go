package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
)

// TestWithWebUIServesPages verifies that mounting the web UI registers its
// routes on the handler alongside the JSON API: the Settings view renders (with
// secrets redacted), the legacy /config path permanently redirects to /settings,
// and the root redirects to the Dashboard (the default landing page). /config
// replaced its read-only page with a redirect to /settings (the editable
// settings page, #288).
func TestWithWebUIServesPages(t *testing.T) {
	cfg := config.Config{}
	cfg.API.Token = "tok_should_not_appear"

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithWebUI(cfg, "vtest"))

	t.Run("settings page redacts and renders", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET /settings status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "tok_should_not_appear") {
			t.Error("Settings view leaked the token through the server handler")
		}
		if !strings.Contains(body, "[REDACTED]") {
			t.Error("Settings view missing [REDACTED] sentinel; redaction did not render")
		}
		if !strings.Contains(body, "vtest") {
			t.Error("sidebar version not rendered")
		}
	})

	t.Run("config path permanently redirects to settings", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("GET /config status = %d, want 301", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/settings" {
			t.Errorf("Location = %q, want /settings", loc)
		}
	})

	t.Run("root redirects to dashboard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusFound {
			t.Fatalf("GET / status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/dashboard" {
			t.Errorf("Location = %q, want /dashboard", loc)
		}
	})
}

// TestWithReportsDBMountsReports confirms WithReportsDB wires the read-only
// reports.Repo onto the mounted web UI so the Reports workspace runs a report
// on demand end-to-end through the server handler.
func TestWithReportsDBMountsReports(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
		WithReportsDB(sqlDB))

	req := httptest.NewRequest(http.MethodGet, "/reports/queue-summary", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reports/queue-summary = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Total") {
		t.Error("reports workspace did not render the queue-summary table")
	}
}

// TestWithoutWebUINoPages confirms that, absent WithWebUI, the handler serves
// only the JSON API and the web routes 404.
func TestWithoutWebUINoPages(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics")

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /config without WithWebUI status = %d, want 404", rec.Code)
	}
}

// TestWithWebUIIfEnabled confirms that WithWebUIIf(true, ...) mounts the web
// UI routes -- the path used when cfg.Server.WebUIEnabled is true in runServe.
func TestWithWebUIIfEnabled(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUIIf(true, config.Config{}, "venabled"))

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings with WithWebUIIf(true) status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "venabled") {
		t.Error("sidebar version not rendered; web UI appears not mounted")
	}
}

// TestWithWebUIIfDisabled confirms that WithWebUIIf(false, ...) is a no-op:
// the web routes return 404, identical to omitting WithWebUI entirely. This is
// the default state (cfg.Server.WebUIEnabled = false) until auth ships (#204).
func TestWithWebUIIfDisabled(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUIIf(false, config.Config{}, "vdisabled"))

	for _, path := range []string{"/config", "/reports", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// Root with no web UI: the handler has no route for GET /{$}, so it
		// falls through to 404 (explicitly, not a redirect and not some other
		// unexpected status like 405/500).
		if path == "/" {
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET / with WithWebUIIf(false) status = %d, want 404", rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with WithWebUIIf(false) status = %d, want 404", path, rec.Code)
		}
	}
}
