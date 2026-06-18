package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
)

// openReportsTestDB opens a temp-file SQLite with every migration applied, the
// same real-SQLite-no-mocks approach internal/reports and internal/cache use.
func openReportsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// newReportsUIServer mounts the UI (with the reports repo wired) on a fresh mux.
func newReportsUIServer(t *testing.T, sqlDB *sql.DB) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB))).Register(mux)
	return mux
}

// insertDone inserts one completed work_queue row with the given output_paths
// JSON and provider lane, so the Recent outcomes report has something to derive.
func insertDone(t *testing.T, sqlDB *sql.DB, title, lane, outputPaths, completedAt string) {
	t.Helper()
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, album, status, last_error,
             output_paths, completed_at, provider_lane)
         VALUES (?, ?, ?, ?, ?, 'done', '', ?, ?, ?)`,
		"Artist", title, "Artist", title, "Album", outputPaths, completedAt, lane)
	if err != nil {
		t.Fatalf("insert done work_queue: %v", err)
	}
}

// getFragment issues an htmx report-fragment request (HX-Request set) and
// returns the recorder.
func getFragment(t *testing.T, mux *http.ServeMux, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/reports/"+key, nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestReportFragmentQueueSummary runs the queue-summary report on demand and
// asserts the results table, the run timestamp, and the out-of-band rail update
// (selection highlight + timestamp) are all present in one fragment response.
func TestReportFragmentQueueSummary(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "d1", "musixmatch", `[{"outdir":"/out","filename":"d1.lrc"}]`, "2026-06-17T10:00:00Z")
	insertDone(t, sqlDB, "d2", "musixmatch", `[{"outdir":"/out","filename":"d2.lrc"}]`, "2026-06-17T11:00:00Z")
	mux := newReportsUIServer(t, sqlDB)

	rec := getFragment(t, mux, "queue-summary")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reports/queue-summary = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"Status", "Count", "Done", "Total", "Refresh"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue-summary fragment missing %q", want)
		}
	}
	// Two done rows.
	if !strings.Contains(body, ">2<") {
		t.Errorf("queue-summary should report 2 done rows; body:\n%s", body)
	}
	// The run stamps a "Last run:" line, not the "Not run yet" default.
	if !strings.Contains(body, "Last run:") {
		t.Error("fragment missing last-run timestamp")
	}
	// OOB rail re-render with the selected item highlighted.
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Error("fragment missing the out-of-band rail swap")
	}
	if n := strings.Count(body, `aria-current="page"`); n != 1 {
		t.Errorf("expected exactly one active rail item, got %d", n)
	}
	if !strings.Contains(body, `hx-get="/reports/queue-summary"`) {
		t.Error("active rail item should be queue-summary")
	}
	// No-store so a run is never cached.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	// HARD CONSTRAINT: never any auto-refresh trigger.
	if strings.Contains(body, "hx-trigger") {
		t.Error("fragment must not contain hx-trigger (no polling/SSE)")
	}
}

// TestReportFragmentRecentOutcomes confirms the derived result class, provider
// lane, and completion timestamp render for a completed track.
func TestReportFragmentRecentOutcomes(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "Song", "petitlyrics", `[{"outdir":"/out","filename":"Song.lrc"}]`, "2026-06-17T12:00:00Z")
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "recent-outcomes").Body.String()
	for _, want := range []string{"Artist", "Title", "Result", "synced", "petitlyrics"} {
		if !strings.Contains(body, want) {
			t.Errorf("recent-outcomes fragment missing %q", want)
		}
	}
}

// TestProviderEffectivenessApproxLabel asserts the hit-rate is rendered with the
// required "approximate" caveat (the underlying counter is attempt-weighted).
func TestProviderEffectivenessApproxLabel(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO provider_outcomes (lane, hits, misses) VALUES ('musixmatch', 3, 1)`); err != nil {
		t.Fatalf("insert provider_outcomes: %v", err)
	}
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "provider-effectiveness").Body.String()
	if !strings.Contains(strings.ToLower(body), "approximate") {
		t.Error("provider-effectiveness must label the hit-rate approximate")
	}
	if !strings.Contains(body, "75.0%") {
		t.Errorf("expected hit-rate 75.0%% for 3 hits / 1 miss; body:\n%s", body)
	}
}

// TestReportFragmentEmptyState confirms a report with zero rows renders a clean
// empty state, not a broken/headerless table.
func TestReportFragmentEmptyState(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "instrumental-inventory").Body.String()
	if !strings.Contains(body, "No audio-detected instrumentals.") {
		t.Errorf("empty instrumental report missing empty-state copy; body:\n%s", body)
	}
	if strings.Contains(body, "<table") {
		t.Error("empty report should not render a table element")
	}
}

// TestReportFragmentUnknownKey rejects an unknown report slug with 404 rather
// than rendering an empty workspace.
func TestReportFragmentUnknownKey(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/reports/does-not-exist", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown report key = %d, want 404", rec.Code)
	}
}

// TestReportFragmentNoRepo fails loudly (503) when the UI has no reports repo
// wired, rather than rendering an empty table that reads as "no data".
func TestReportFragmentNoRepo(t *testing.T) {
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(mux) // no WithReports

	rec := getFragment(t, mux, "queue-summary")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("report with no repo = %d, want 503", rec.Code)
	}
}

// TestReportFragmentNoJSFullPage confirms a plain navigation (no HX-Request
// header) to a report URL returns the full workspace page (sidebar shell +
// selected report), so each rail link is a real destination without JS.
func TestReportFragmentNoJSFullPage(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "d1", "musixmatch", `[{"outdir":"/out","filename":"d1.lrc"}]`, "2026-06-17T10:00:00Z")
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/reports/queue-summary", nil) // no HX-Request
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain GET /reports/queue-summary = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Full page: sidebar shell present (wordmark) AND the report table rendered.
	if !strings.Contains(body, "mx-sidebar") {
		t.Error("no-JS report navigation should return the full shell, not a bare fragment")
	}
	if !strings.Contains(strings.ToLower(body), "<!doctype html>") {
		t.Error("no-JS report navigation should return a full HTML document")
	}
	if !strings.Contains(body, "Refresh") {
		t.Error("no-JS full page should still render the selected report")
	}
	// The selected report's sidebar row is marked active on the full page too.
	if n := strings.Count(body, `aria-current="page"`); n != 1 {
		t.Errorf("no-JS full page should mark exactly one sidebar row active, got %d", n)
	}
	if !strings.Contains(body, `hx-get="/reports/queue-summary" hx-target="#mx-main" hx-swap="innerHTML" hx-push-url="true" aria-current="page"`) {
		t.Error("no-JS full page should mark the queue-summary sidebar row active")
	}
}

// insertInstrumental inserts one audio-detected instrumental work_queue row
// (instrumental_result=1) with the given detect_instrumental request flag
// (pass nil for NULL), optionally linked to a scan_results file path.
func insertInstrumental(t *testing.T, sqlDB *sql.DB, title string, detect any, filePath string) {
	t.Helper()
	ctx := context.Background()
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, status, instrumental_result, detect_instrumental)
         VALUES (?, ?, ?, ?, 'done', 1, ?)`,
		"Artist", title, "Artist", title, detect)
	if err != nil {
		t.Fatalf("insert instrumental work_queue: %v", err)
	}
	if filePath == "" {
		return
	}
	wqID, _ := res.LastInsertId()
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES ('/music', 'lib') RETURNING id`).Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	var srID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, file_path) VALUES (?, ?) RETURNING id`, libID, filePath).Scan(&srID); err != nil {
		t.Fatalf("insert scan_results: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wqID, srID); err != nil {
		t.Fatalf("link scan_results: %v", err)
	}
}

// TestReportFragmentInstrumentalRows exercises the instrumental inventory with
// rows, covering the file-path join and all three detect-requested labels.
func TestReportFragmentInstrumentalRows(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertInstrumental(t, sqlDB, "Requested", 1, "/music/a.flac")
	insertInstrumental(t, sqlDB, "NotRequested", 0, "")
	insertInstrumental(t, sqlDB, "Defaulted", nil, "")
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "instrumental-inventory").Body.String()
	for _, want := range []string{
		"/music/a.flac", "requested", "not requested", "config default",
		"Requested", "NotRequested", "Defaulted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("instrumental inventory missing %q", want)
		}
	}
}

// TestRecentOutcomeNullCompletedAt covers the null-timestamp render path: a done
// row with no completed_at shows an em-dash, not a bogus epoch.
func TestRecentOutcomeNullCompletedAt(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, status, last_error, output_paths, completed_at)
         VALUES ('A', 'NoTime', 'A', 'NoTime', 'done', '', '[{"outdir":"/o","filename":"NoTime.lrc"}]', NULL)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "recent-outcomes").Body.String()
	if !strings.Contains(body, "NoTime") {
		t.Fatal("recent outcome row missing")
	}
	if !strings.Contains(body, ">-<") {
		t.Errorf("null completed_at should render as '-'; body:\n%s", body)
	}
}

// TestReportFragmentQueryError fails with 500 when the underlying query errors
// (here, a closed database), rather than rendering a partial or empty table.
func TestReportFragmentQueryError(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)
	_ = sqlDB.Close() // force every query to error

	rec := getFragment(t, mux, "queue-summary")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("report over a closed DB = %d, want 500", rec.Code)
	}
}

// TestReportSelectionHighlightMovesAcrossReports verifies the sidebar's active
// highlight follows the selected report: each run returns an out-of-band sidebar
// report-nav re-render in which exactly one row -- the selected report's -- is
// marked aria-current, so selecting a different report moves the highlight rather
// than accumulating it.
func TestReportSelectionHighlightMovesAcrossReports(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	for _, key := range []string{"queue-summary", "failure-analysis"} {
		body := getFragment(t, mux, key).Body.String()
		// The out-of-band sidebar report nav is present so the highlight updates
		// in place (not the in-content rail of the old design).
		if !strings.Contains(body, `id="mx-report-nav"`) {
			t.Errorf("%s fragment: missing the out-of-band sidebar report nav", key)
		}
		// Exactly one row is active across the whole sidebar.
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("%s fragment: expected exactly one active sidebar row, got %d", key, n)
		}
		// The active row is the selected report's row (precise attribute order as
		// templ renders it).
		wantActive := `hx-get="/reports/` + key + `" hx-target="#mx-main" hx-swap="innerHTML" hx-push-url="true" aria-current="page"`
		if !strings.Contains(body, wantActive) {
			t.Errorf("%s fragment: active sidebar row should be %s; missing %q", key, key, wantActive)
		}
	}
}

// TestBuildReportViewUnimplementedKey exercises the defensive default case: a
// reportDef whose key has no switch arm must fail fast with an error rather than
// returning an empty (misleading) view. Unreachable via the HTTP path (keys are
// validated upstream in handleReportFragment), so it is called directly.
func TestBuildReportViewUnimplementedKey(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	if _, err := ui.buildReportView(context.Background(), reportDef{key: "no-such-report"}); err == nil {
		t.Fatal("buildReportView with unknown key = nil error, want fail-fast error")
	}
}
