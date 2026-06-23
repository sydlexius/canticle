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

// TestBuildQueueChart verifies the queue doughnut series uses the fixed
// status-label order (kept in sync with the chart-init color map) and the
// corresponding counts, excluding Total.
func TestBuildQueueChart(t *testing.T) {
	c := buildQueueChart(reports.QueueSummary{
		Pending: 1, Processing: 2, Done: 3, Failed: 4, Deferred: 5, Total: 15,
	})
	wantLabels := []string{"Pending", "Processing", "Done", "Failed", "Deferred"}
	if len(c.Labels) != len(wantLabels) {
		t.Fatalf("Labels len = %d, want %d", len(c.Labels), len(wantLabels))
	}
	for i, l := range wantLabels {
		if c.Labels[i] != l {
			t.Errorf("Labels[%d] = %q, want %q", i, c.Labels[i], l)
		}
	}
	wantValues := []float64{1, 2, 3, 4, 5}
	for i, v := range wantValues {
		if c.Values[i] != v {
			t.Errorf("Values[%d] = %v, want %v", i, c.Values[i], v)
		}
	}
	if !c.HasData() {
		t.Error("HasData() = false, want true for non-zero counts")
	}
	// An empty queue must report no data so the chart is omitted.
	if buildQueueChart(reports.QueueSummary{}).HasData() {
		t.Error("HasData() = true for all-zero queue, want false")
	}
}

// TestHandleDashboard_Charts verifies that when queue and provider data exist,
// the dashboard renders both chart canvases with their data attributes and loads
// the vendored Chart.js + init scripts from /static (CSP-safe, no CDN/inline).
func TestHandleDashboard_Charts(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "song-a", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-19T10:00:00Z")
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
	wantFragments := []string{
		`id="mx-dash-queue-chart"`,
		`data-chart-type="doughnut"`,
		`/static/js/chart.umd.min.js`,
		`/static/js/chart-init.js`,
	}
	for _, frag := range wantFragments {
		if !strings.Contains(body, frag) {
			t.Errorf("dashboard charts: missing %q", frag)
		}
	}
	// The standalone provider hit-rate bar chart was replaced by inline mini bars
	// (#318); its canvas must no longer be rendered.
	if strings.Contains(body, `id="mx-dash-provider-chart"`) {
		t.Error("dashboard charts: standalone provider bar-chart canvas must be removed (replaced by inline mini bars)")
	}
	// The per-provider hit rate now renders as an inline mini bar inside its tile,
	// carrying the percent in a data-hit-rate attribute (3 hits / 4 attempts = 75%).
	if !strings.Contains(body, `class="mx-dash-tile-bar-fill"`) || !strings.Contains(body, `data-hit-rate="75"`) {
		t.Error("dashboard: provider tile missing inline hit-rate mini bar (data-hit-rate=\"75\")")
	}
	// The queue series labels must be serialized into the work-queue canvas's
	// data-chart-labels JSON attribute, not merely appear somewhere in the body
	// (a loose Contains would also match the stat-tile label text). templ
	// HTML-escapes the JSON quotes to &#34; inside the attribute value.
	const wantQueueLabelsAttr = `data-chart-labels="[&#34;Pending&#34;,&#34;Processing&#34;,&#34;Done&#34;,&#34;Failed&#34;,&#34;Deferred&#34;]"`
	if !strings.Contains(body, wantQueueLabelsAttr) {
		t.Errorf("dashboard charts: work-queue canvas missing serialized labels attribute %q", wantQueueLabelsAttr)
	}
	// The provider name no longer lives in a chart canvas; it renders as a tile
	// whose inline mini hit-rate bar is asserted above. Confirm "musixmatch"
	// appears as a tile label so the provider series is represented somewhere.
	if !strings.Contains(body, "musixmatch") {
		t.Error("dashboard: provider tile label \"musixmatch\" missing")
	}
	// No inline chart code: chart logic must be external (CSP script-src 'self').
	if strings.Contains(body, "new Chart(") {
		t.Error("dashboard charts: inline Chart() call found; init must be in the static .js file")
	}
}

// TestHandleDashboard_ChartsOmittedWhenEmpty verifies that with an empty DB the
// chart canvases are not rendered (HasData false), so no blank chart appears.
func TestHandleDashboard_ChartsOmittedWhenEmpty(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `id="mx-dash-queue-chart"`) {
		t.Error("empty dashboard must omit the queue chart canvas")
	}
	if strings.Contains(body, `id="mx-dash-provider-chart"`) {
		t.Error("empty dashboard must omit the provider chart canvas")
	}
}

// TestBuildProviderTiles verifies provider rows map to tiles carrying the hit
// count/attempts value and an inline mini hit-rate bar (#318) whose percent
// matches the displayed sub-label.
func TestBuildProviderTiles(t *testing.T) {
	tiles := buildProviderTiles([]reports.ProviderEffectiveness{
		{Lane: "musixmatch", Hits: 3, Misses: 1, HitRate: 0.75},
		{Lane: "petitlyrics", Hits: 0, Misses: 2, HitRate: 0},
	})
	if len(tiles) != 2 {
		t.Fatalf("buildProviderTiles len = %d, want 2", len(tiles))
	}
	mx := tiles[0]
	if mx.Label != "musixmatch" || mx.Value != "3/4" || mx.Sub != "75%" {
		t.Errorf("musixmatch tile = %+v, want label=musixmatch value=3/4 sub=75%%", mx)
	}
	if !mx.ShowBar || mx.BarPct != "75" || mx.BarLabel != "Hit rate 75%" {
		t.Errorf("musixmatch bar = {ShowBar:%v BarPct:%q BarLabel:%q}, want {true \"75\" \"Hit rate 75%%\"}", mx.ShowBar, mx.BarPct, mx.BarLabel)
	}
	pl := tiles[1]
	if pl.BarPct != "0" || !pl.ShowBar {
		t.Errorf("petitlyrics bar = {ShowBar:%v BarPct:%q}, want {true \"0\"}", pl.ShowBar, pl.BarPct)
	}
}

// TestHandleDashboard_NoCacheTile verifies the lyrics-cache hit-rate tile is no
// longer surfaced on the dashboard (removed per maintainer: cache hit rate is
// not worth surfacing here). Even with provider data present, no Cache tile
// renders. Cache stats remain available via /metrics, a separate path.
func TestHandleDashboard_NoCacheTile(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	// Seed provider data so the Lyrics Sources row renders real tiles.
	for i, hit := range []int64{1, 1, 1, 0} {
		if _, err := sqlDB.ExecContext(t.Context(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, 'musixmatch', ?, '2026-06-18T00:00:00Z')`,
			int64(i+1), hit); err != nil {
			t.Fatalf("insert lane_attempts: %v", err)
		}
	}
	mux := newReportsUIServer(t, sqlDB)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "musixmatch") {
		t.Fatal("dashboard missing provider tile; test precondition not met")
	}
	if strings.Contains(body, ">Cache<") {
		t.Error("dashboard must not render the lyrics-cache tile (removed)")
	}
}
