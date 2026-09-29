package web

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// seedFinishedSplitRows seeds 2 word-synced (finished) and 3 settled-but-
// upgradable done rows (line-synced, untiered synced, unsynced), so a counter
// wired to Done (5), to the wrong half, or to Total cannot pass.
func seedFinishedSplitRows(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	insertDoneWithSyncTier(t, sqlDB, "w1", `[{"outdir":"/o","filename":"w1.lrc"}]`, "2026-06-19T10:00:00Z", "word")
	insertDoneWithSyncTier(t, sqlDB, "w2", `[{"outdir":"/o","filename":"w2.lrc"}]`, "2026-06-19T10:01:00Z", "word")
	insertDoneWithSyncTier(t, sqlDB, "l1", `[{"outdir":"/o","filename":"l1.lrc"}]`, "2026-06-19T10:02:00Z", "line")
	insertDoneWithSyncTier(t, sqlDB, "n1", `[{"outdir":"/o","filename":"n1.lrc"}]`, "2026-06-19T10:03:00Z", "")
	insertDone(t, sqlDB, "u1", "musixmatch", `[{"outdir":"/o","filename":"u1.txt"}]`, "2026-06-19T10:04:00Z")
}

// TestHandleDashboard_FinishedSplitTiles asserts the Work Queue row splits
// Done into Finished (word-synced, terminal) and Settled (upgradable) per the
// #553 decision, each tile showing its OWN count.
func TestHandleDashboard_FinishedSplitTiles(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedFinishedSplitRows(t, sqlDB)
	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for label, want := range map[string]string{"Finished": "2", "Settled (upgradable)": "3"} {
		tile := regexp.MustCompile(`<span class="mx-dash-tile-label">` + regexp.QuoteMeta(label) + `</span>\s*<span class="mx-dash-tile-value">(\d+)</span>`)
		m := tile.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("dashboard missing %q tile", label)
			continue
		}
		if m[1] != want {
			t.Errorf("%q tile = %s, want %s", label, m[1], want)
		}
	}
	// The bare Done tile is gone: keeping it beside its two halves would
	// double-count every settled row in one tile row.
	if regexp.MustCompile(`<span class="mx-dash-tile-label">Done</span>`).MatchString(body) {
		t.Error("dashboard still renders a Done tile beside Finished/Settled; the row would double-count")
	}
}

// TestReportFragmentQueueSummaryFinishedSplit asserts the Reports queue-summary
// table carries the same split, so the two surfaces agree.
func TestReportFragmentQueueSummaryFinishedSplit(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedFinishedSplitRows(t, sqlDB)
	mux := newReportsUIServer(t, sqlDB)
	body := getFragment(t, mux, "queue-summary").Body.String()
	for label, want := range map[string]string{"Finished": "2", "Settled (upgradable)": "3", "Total": "5"} {
		row := regexp.MustCompile(`<td>` + regexp.QuoteMeta(label) + `</td>\s*<td class="mx-cell-mono">(\d+)</td>`)
		m := row.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("queue-summary fragment missing %q row; body:\n%s", label, body)
			continue
		}
		if m[1] != want {
			t.Errorf("%q row = %s, want %s", label, m[1], want)
		}
	}
}

// TestBuildQueueTilesSumToTotal: the tile row is one status axis, so its
// values must sum to Total with no row counted twice.
func TestBuildQueueTilesSumToTotal(t *testing.T) {
	qs := reports.QueueSummary{Pending: 1, Processing: 2, Done: 7, Failed: 4, Deferred: 5, Unavailable: 6, Total: 25, Finished: 3, SettledUpgradable: 4}
	var sum int64
	for _, tile := range buildQueueTiles(qs) {
		var v int64
		for _, c := range tile.Value {
			v = v*10 + int64(c-'0')
		}
		sum += v
	}
	if sum != qs.Total {
		t.Errorf("queue tiles sum to %d, want Total %d", sum, qs.Total)
	}
}
