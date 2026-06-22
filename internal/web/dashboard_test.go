package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
)

// TestHandleDashboard_NoReports verifies that GET /dashboard returns 503 when
// no reports repo is wired, rather than panicking or rendering an empty page.
func TestHandleDashboard_NoReports(t *testing.T) {
	mux := newUIServer(config.Config{}, "dev")

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /dashboard without reports repo: status = %d, want 503", rec.Code)
	}
}

// TestHandleDashboard_WithReports verifies that GET /dashboard returns 200 and
// renders all expected sections when the reports repo is wired with real SQLite.
func TestHandleDashboard_WithReports(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Page title is present.
	if !strings.Contains(body, "Dashboard") {
		t.Error("dashboard page missing title")
	}
	// Three section headings present; Effective Configuration was removed (P1).
	for _, want := range []string{"Work Queue", "Lyrics Sources", "Recent Outcomes"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing section heading %q", want)
		}
	}
	if strings.Contains(body, "Effective Configuration") {
		t.Error("dashboard must not render Effective Configuration section (P1 removal)")
	}
	// /metrics endpoint is documented somewhere on the page.
	if !strings.Contains(body, "/metrics") {
		t.Error("dashboard missing /metrics reference")
	}
}

// TestHandleDashboard_NoCacheControl verifies the dashboard always carries
// Cache-Control: no-store so operational data is never stale-served.
func TestHandleDashboard_NoCacheControl(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestDashboardNavLink verifies the sidebar marks the Dashboard link active
// when on /dashboard, and exactly one nav item is marked active.
func TestDashboardNavLink(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/dashboard" class="mx-nav-link" aria-current="page"`) {
		t.Error("dashboard page did not mark Dashboard nav link active")
	}
	if n := strings.Count(body, `aria-current="page"`); n != 1 {
		t.Errorf("dashboard page should have exactly one active nav row, got %d", n)
	}
}

// TestHandleDashboard_QueueTiles verifies the five queue-status tiles are
// present.
func TestHandleDashboard_QueueTiles(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "song-a", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-19T10:00:00Z")
	insertDone(t, sqlDB, "song-b", "musixmatch", `[{"outdir":"/o","filename":"b.lrc"}]`, "2026-06-19T11:00:00Z")

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, label := range []string{"Pending", "Processing", "Done", "Failed", "Deferred"} {
		if !strings.Contains(body, label) {
			t.Errorf("dashboard missing queue tile label %q", label)
		}
	}
}

// TestHandleDashboard_RecentOutcomesTable verifies the recent-outcomes table
// renders seeded rows and async-nature copy.
func TestHandleDashboard_RecentOutcomesTable(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "Lyric Haul", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-19T12:00:00Z")

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Lyric Haul") {
		t.Error("dashboard recent-outcomes table missing seeded track title")
	}
	if !strings.Contains(body, "asynchronous") && !strings.Contains(body, "asynchronously") {
		t.Error("dashboard missing async-nature copy in recent outcomes section")
	}
}

// TestFormatDashboardTime covers the zero-value sentinel, the UTC-labeled path,
// and the TZ-env-applied (server-local) path.
func TestFormatDashboardTime(t *testing.T) {
	zero := time.Time{}
	display, iso, applied := formatDashboardTime(zero, nil)
	if display != "-" || iso != "" || applied {
		t.Errorf("zero: got (%q,%q,%v), want (\"-\",\"\",false)", display, iso, applied)
	}

	ts := time.Date(2026, 6, 19, 20, 55, 0, 0, time.UTC)

	display, iso, applied = formatDashboardTime(ts, nil)
	if iso != "2026-06-19T20:55:00Z" {
		t.Errorf("UTC iso: got %q, want 2026-06-19T20:55:00Z", iso)
	}
	if applied {
		t.Error("UTC path: tzApplied should be false")
	}
	if display == "" {
		t.Error("UTC path: display should not be empty")
	}

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("time zone America/Los_Angeles unavailable: %v", err)
	}
	display, iso, applied = formatDashboardTime(ts, loc)
	if iso != "2026-06-19T20:55:00Z" {
		t.Errorf("TZ path iso: got %q, want 2026-06-19T20:55:00Z", iso)
	}
	if !applied {
		t.Error("TZ path: tzApplied should be true")
	}
	if display == "" {
		t.Error("TZ path: display should not be empty")
	}
}

// TestHandleDashboard_TZEnvTimestamp verifies that when the TZ env var is set,
// completed-at times carry data-tz-applied="1" and not data-tz="pending".
func TestHandleDashboard_TZEnvTimestamp(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "TZ Track", "musixmatch", `[{"outdir":"/o","filename":"tz.lrc"}]`, "2026-06-19T20:55:00Z")

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// When TZ env is honored server-side, the <time> element carries data-tz-applied="1".
	// Note: the page's JS snippet also contains the literal string `data-tz="pending"`
	// as a CSS selector, so only test for the positive attribute, not the absence of
	// the pending selector string.
	if !strings.Contains(body, `data-tz-applied="1"`) {
		t.Error("TZ env set: expected data-tz-applied=\"1\" on completed-at <time> element")
	}
}

// TestHandleDashboard_ProviderTiles verifies that provider lane tiles render when
// lane_attempts data exists (covers the ProviderTiles loop body in buildDashboardView).
func TestHandleDashboard_ProviderTiles(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	// Insert 3 hits + 1 miss for musixmatch to produce provider effectiveness data.
	for i, hit := range []int64{1, 1, 1, 0} {
		if _, err := sqlDB.ExecContext(t.Context(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, 'musixmatch', ?, '2026-06-18T00:00:00Z')`,
			int64(i+1), hit); err != nil {
			t.Fatalf("insert lane_attempts: %v", err)
		}
	}
	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "musixmatch") {
		t.Error("dashboard missing provider lane tile for musixmatch")
	}
}

// TestHandleDashboard_AsyncCopy verifies copy throughout the page makes the
// async/queued processing model explicit: no on-demand search language.
func TestHandleDashboard_AsyncCopy(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	// Must mention async processing somewhere.
	if !strings.Contains(body, "asynchronous") && !strings.Contains(body, "asynchronously") {
		t.Error("dashboard missing async-processing copy; must be explicit about queued model")
	}
	// Must not contain a search box.
	if strings.Contains(body, `type="search"`) || strings.Contains(body, `<input`) && strings.Contains(body, "search") {
		t.Error("dashboard must not contain a search box")
	}
}

// fakeCacheStats is a test CacheStatsProvider returning fixed hit/lookup counts.
type fakeCacheStats struct{ hits, lookups int64 }

func (f fakeCacheStats) CacheStats() (hits, lookups int64) { return f.hits, f.lookups }

// TestBuildCacheTiles verifies the cache tile value/rate formatting, including
// the lookups==0 guard that must render 0% rather than dividing by zero.
func TestBuildCacheTiles(t *testing.T) {
	tests := []struct {
		name               string
		hits, lookups      int64
		wantValue, wantSub string
	}{
		{"no lookups guard", 0, 0, "0/0", "0%"},
		{"all hits", 5, 5, "5/5", "100%"},
		{"three of four", 3, 4, "3/4", "75%"},
		{"rounds to nearest", 1, 3, "1/3", "33%"},
		{"zero hits with lookups", 0, 4, "0/4", "0%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tiles := buildCacheTiles(tc.hits, tc.lookups)
			if len(tiles) != 1 {
				t.Fatalf("buildCacheTiles len = %d, want 1", len(tiles))
			}
			got := tiles[0]
			if got.Label != "Cache" {
				t.Errorf("Label = %q, want %q", got.Label, "Cache")
			}
			if got.Value != tc.wantValue {
				t.Errorf("Value = %q, want %q", got.Value, tc.wantValue)
			}
			if got.Sub != tc.wantSub {
				t.Errorf("Sub = %q, want %q", got.Sub, tc.wantSub)
			}
		})
	}
}

// TestHandleDashboard_CacheTile verifies that when a cache stats source is
// attached the dashboard renders the Cache tile with its hit-rate, and omits it
// otherwise.
func TestHandleDashboard_CacheTile(t *testing.T) {
	sqlDB := openReportsTestDB(t)

	// With cache stats attached: tile and rate appear.
	muxWith := http.NewServeMux()
	uiWith := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	uiWith.AttachCacheStats(fakeCacheStats{hits: 3, lookups: 4})
	uiWith.Register(muxWith)

	recWith := httptest.NewRecorder()
	muxWith.ServeHTTP(recWith, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if recWith.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", recWith.Code)
	}
	bodyWith := recWith.Body.String()
	if !strings.Contains(bodyWith, ">Cache<") {
		t.Error("dashboard with cache stats missing Cache tile label")
	}
	if !strings.Contains(bodyWith, "3/4") || !strings.Contains(bodyWith, "75%") {
		t.Errorf("dashboard with cache stats missing 3/4 / 75%% values")
	}

	// Without cache stats: no Cache tile.
	muxWithout := newReportsUIServer(t, sqlDB)
	recWithout := httptest.NewRecorder()
	muxWithout.ServeHTTP(recWithout, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if strings.Contains(recWithout.Body.String(), ">Cache<") {
		t.Error("dashboard without cache stats must omit the Cache tile")
	}
}
