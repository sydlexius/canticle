package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/aligner"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/web"
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

// TestWithLaneHealthReachesDashboard pins the production wiring of the lane
// health seam onto the web UI (#488): NewHandler must hand WithLaneHealth's
// source to the dashboard, or the tiles silently lose their status line.
func TestWithLaneHealthReachesDashboard(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
		WithReportsDB(sqlDB),
		WithLaneHealth(func() []orchestrator.LaneState {
			return []orchestrator.LaneState{{Provider: "musixmatch", State: orchestrator.LaneStateHalfOpen}}
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `mx-dash-tile-status-probing">Probing<`) {
		t.Error("dashboard missing the lane status line; WithLaneHealth did not reach the web UI")
	}
}

// ctxAligner's AlignFile returns, noting it on ended, once its context ends.
type ctxAligner struct{ ended chan struct{} }

func (ctxAligner) Health(context.Context) error { return nil }
func (a ctxAligner) AlignFile(ctx context.Context, _ io.Reader, _ []string) (aligner.Result, error) {
	<-ctx.Done()
	a.ended <- struct{}{}
	return aligner.Result{}, ctx.Err()
}

// TestCloseEndsAutoAlignmentRuns pins Close's call of the web UI's CloseAuto,
// its only production caller: Close returns with a started run's context ended.
func TestCloseEndsAutoAlignmentRuns(t *testing.T) {
	ok := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	ok(err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	audio := filepath.Join(t.TempDir(), "song.flac")
	lrc := strings.TrimSuffix(audio, ".flac") + ".lrc"
	ok(os.WriteFile(audio, []byte("invented audio"), 0o600))
	ok(os.WriteFile(lrc, []byte("[00:01.00]one\n[00:05.00]two\n"), 0o600))
	fi, err := os.Stat(lrc)
	ok(err)
	_, err = sqlDB.ExecContext(context.Background(), `INSERT INTO libraries (path, name) VALUES (?, 'lib')`, filepath.Dir(audio))
	ok(err)
	_, err = sqlDB.ExecContext(context.Background(), `INSERT INTO work_queue (artist, title, artist_key, title_key, album,
		status, source_path, outcome_type, sync_tier) VALUES ('A', 'T', 'a', 't', 'Al', 'done', ?, 'synced', 'line')`, audio)
	ok(err)
	fake := ctxAligner{ended: make(chan struct{}, 1)}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithWebUI(config.Config{}, "vtest"), WithReportsDB(sqlDB), WithAutoAligner(fake, 1))
	token := strings.Repeat("ab", 32)
	req := httptest.NewRequest(http.MethodPost, "/preview/1/auto", strings.NewReader(fmt.Sprintf("csrf_token=%s&mtime=%d", token, fi.ModTime().UnixNano())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: web.CSRFCookieName, Value: token})
	rec := httptest.NewRecorder()
	if h.ServeHTTP(rec, req); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body)
	}
	if h.Close(); len(fake.ended) != 1 {
		t.Fatal("Close returned with the Auto alignment run's context still live")
	}
}
