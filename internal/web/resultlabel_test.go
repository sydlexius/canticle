package web

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

func TestResultLabel(t *testing.T) {
	tests := []struct {
		name string
		rc   reports.ResultClass
		want string
	}{
		{"word-synced gets a hyphenated label", reports.ResultWordSynced, "word-synced"},
		{"line-synced gets a hyphenated label", reports.ResultLineSynced, "line-synced"},
		{"tier-unknown synced says so in text, not only color", reports.ResultSynced, "synced (tier unknown)"},
		{"unsynced passes through unchanged", reports.ResultUnsynced, "unsynced"},
		{"blocked renders its own word, not the unknown dash", reports.ResultBlocked, "blocked"},
		{"unknown renders a dash, not a fourth class word", reports.ResultUnknown, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resultLabel(tt.rc); got != tt.want {
				t.Errorf("resultLabel(%q) = %q, want %q", tt.rc, got, tt.want)
			}
		})
	}
}

func TestResultTierClass(t *testing.T) {
	tests := []struct {
		name string
		rc   reports.ResultClass
		want string
	}{
		{"word-synced gets the word tier class", reports.ResultWordSynced, "mx-result-tier mx-result-tier-word"},
		{"line-synced gets the line tier class", reports.ResultLineSynced, "mx-result-tier mx-result-tier-line"},
		{"plain synced (tier unknown) gets its own class, not no-badge", reports.ResultSynced, "mx-result-tier mx-result-tier-unknown"},
		{"unsynced has no tier badge", reports.ResultUnsynced, ""},
		{"instrumental has no tier badge", reports.ResultInstrumental, ""},
		{"miss has no tier badge", reports.ResultMiss, ""},
		{"rejected has no tier badge", reports.ResultRejected, ""},
		{"blocked has no tier badge", reports.ResultBlocked, ""},
		{"unknown gets the muted pill", reports.ResultUnknown, "mx-result-tier mx-result-tier-unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resultTierClass(tt.rc); got != tt.want {
				t.Errorf("resultTierClass(%q) = %q, want %q", tt.rc, got, tt.want)
			}
		})
	}
}

// TestBuildResultsTiles asserts one tile per resultBuckets entry, in order,
// each carrying its value and a non-empty tooltip, zero included (never
// omitted), and that the tiles never merge word/line/tier-unknown.
func TestBuildResultsTiles(t *testing.T) {
	b := reports.ResultsBreakdown{WordSynced: 1, LineSynced: 2, Unsynced: 3, Instrumental: 4, Blocked: 7, SyncedTierUnknown: 5, Other: 6}
	want := []struct{ label, value string }{
		{"Word-synced", "1"}, {"Line-synced", "2"}, {"Unsynced", "3"},
		{"Instrumental", "4"}, {"Blocked", "7"}, {"Tier unknown", "5"}, {"Other", "6"},
	}
	tiles := buildResultsTiles(b)
	if len(tiles) != len(want) {
		t.Fatalf("buildResultsTiles returned %d tiles, want %d", len(tiles), len(want))
	}
	for i, w := range want {
		if tiles[i].Label != w.label || tiles[i].Value != w.value {
			t.Errorf("tile %d = %q/%q, want %q/%q", i, tiles[i].Label, tiles[i].Value, w.label, w.value)
		}
		if tiles[i].Tooltip == "" {
			t.Errorf("tile %q has no tooltip", tiles[i].Label)
		}
	}
	for _, tile := range buildResultsTiles(reports.ResultsBreakdown{}) {
		if tile.Value != "0" {
			t.Errorf("zero-state tile %q value = %q, want \"0\"", tile.Label, tile.Value)
		}
	}
}

var (
	dashTileValueRE = regexp.MustCompile(`<span class="mx-dash-tile-label">([^<]+)</span>\s*<span class="mx-dash-tile-value">(\d+)</span>`)
	dashSectionRE   = regexp.MustCompile(`(?s)<h2 class="mx-dash-section-title" id="(mx-dash-[a-z]+-heading)">.*?</section>`)
)

// dashSectionTiles returns label -> value for the tiles inside the dashboard
// section whose heading id is headingID.
func dashSectionTiles(t *testing.T, body, headingID string) map[string]int {
	t.Helper()
	for _, sec := range dashSectionRE.FindAllStringSubmatch(body, -1) {
		if sec[1] != headingID {
			continue
		}
		out := map[string]int{}
		for _, m := range dashTileValueRE.FindAllStringSubmatch(sec[0], -1) {
			n, _ := strconv.Atoi(m[2])
			out[m[1]] = n
		}
		return out
	}
	t.Fatalf("dashboard section %q not found", headingID)
	return nil
}

// TestDashboardResultsSumToDone renders the real dashboard (#599) over a mixed
// fixture holding every Results bucket, including a prune-retired row that
// still carries stale word-synced data, and asserts (1) each Results tile
// carries its own count, (2) the Results tiles sum to Finished + Settled
// (upgradable) in the Work Queue, (3) no Instrumental tile remains in the Work
// Queue and (4) the tiles carry their Go-defined tooltips.
func TestDashboardResultsSumToDone(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDoneWithSyncTier(t, sqlDB, "WS", pathsJSONWeb("ws.lrc"), "2026-06-17T10:00:00Z", "word")
	insertDoneWithSyncTier(t, sqlDB, "LS", pathsJSONWeb("ls.lrc"), "2026-06-17T11:00:00Z", "line")
	insertDoneWithSyncTier(t, sqlDB, "TU", pathsJSONWeb("tu.lrc"), "2026-06-17T12:00:00Z", "")
	insertDone(t, sqlDB, "UN", "musixmatch", pathsJSONWeb("un.txt"), "2026-06-17T13:00:00Z")
	insertInstrumental(t, sqlDB, "IN", nil, "")
	insertDone(t, sqlDB, "RJ", "musixmatch", pathsJSONWeb("rj.txt"), "2026-06-17T14:00:00Z")
	insertDoneWithSyncTier(t, sqlDB, "PR", pathsJSONWeb("pr.lrc"), "2026-06-17T15:00:00Z", "word")
	for _, stmt := range []struct{ q, title string }{
		{`UPDATE work_queue SET outcome_type = 'instrumental' WHERE title = ?`, "IN"},
		{`UPDATE work_queue SET outcome_type = 'rejected' WHERE title = ?`, "RJ"},
		{`UPDATE work_queue SET last_error = '` + queue.UnresolvableGoneError + `' WHERE title = ?`, "PR"},
	} {
		if _, err := sqlDB.ExecContext(context.Background(), stmt.q, stmt.title); err != nil {
			t.Fatalf("seed %s: %v", stmt.title, err)
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

	results := dashSectionTiles(t, body, "mx-dash-results-heading")
	wantResults := map[string]int{
		"Word-synced": 1, "Line-synced": 1, "Unsynced": 1,
		"Instrumental": 1, "Tier unknown": 1, "Other": 2,
	}
	for label, want := range wantResults {
		if got, ok := results[label]; !ok || got != want {
			t.Errorf("Results tile %q = %d (present %v), want %d", label, got, ok, want)
		}
	}
	queueTiles := dashSectionTiles(t, body, "mx-dash-queue-heading")
	if _, ok := queueTiles["Instrumental"]; ok {
		t.Error("Work Queue must not carry an Instrumental tile (statuses only)")
	}
	sum := 0
	for _, n := range results {
		sum += n
	}
	if done := queueTiles["Finished"] + queueTiles["Settled (upgradable)"]; sum != done {
		t.Errorf("Results tiles sum to %d, Work Queue Finished + Settled = %d; they must match", sum, done)
	}
	for _, rb := range resultBuckets {
		if !strings.Contains(body, `title="`+html.EscapeString(rb.Tooltip)+`"`) {
			t.Errorf("dashboard missing tooltip for Results tile %q", rb.Label)
		}
	}
	// Recent Outcomes carries the tier badges too (#627); asserted on the badge
	// element so a bare tooltip substring cannot satisfy it.
	for _, want := range []string{">word-synced</span>", ">line-synced</span>", "mx-result-tier-word", "mx-result-tier-line", "mx-result-tier-unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard Recent Outcomes missing %q", want)
		}
	}
}

// TestReportsRecentOutcomesShowsSyncTiers is the Reports-workspace equivalent
// of the dashboard's Recent Outcomes tier badges: the same three rows, rendered through
// the /reports/recent-outcomes fragment instead of the dashboard, since the
// two surfaces share the RecentOutcomeRow view model but render from
// independent call sites (ui.go's buildReportView vs dashboard.go's
// buildRecentRows) that could drift.
func TestReportsRecentOutcomesShowsSyncTiers(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDoneWithSyncTier(t, sqlDB, "WS", pathsJSONWeb("ws.lrc"), "2026-06-17T10:00:00Z", "word")
	insertDoneWithSyncTier(t, sqlDB, "LS", pathsJSONWeb("ls.lrc"), "2026-06-17T11:00:00Z", "line")
	insertDoneWithSyncTier(t, sqlDB, "TU", pathsJSONWeb("tu.lrc"), "2026-06-17T12:00:00Z", "")
	mux := newReportsUIServer(t, sqlDB)

	body := getFragment(t, mux, "recent-outcomes").Body.String()
	for _, want := range []string{"word-synced", "line-synced", "mx-result-tier-word", "mx-result-tier-line", "mx-result-tier-unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("recent-outcomes fragment missing %q; body:\n%s", want, body)
		}
	}
}

// pathsJSONWeb builds an output_paths JSON array with one entry, matching the
// internal/reports test helper of the same shape (unexported there, so this
// package needs its own copy).
func pathsJSONWeb(filename string) string {
	return `[{"outdir":"/out","filename":"` + filename + `"}]`
}

// TestLineSyncedTooltipMakesNoSourceClaim pins the #1201 review fix: the worker
// stamps the line tier under word_sync_mode=off, for non-word-capable lanes and
// for cache-served results, so the tooltip may only describe the file on disk.
func TestLineSyncedTooltipMakesNoSourceClaim(t *testing.T) {
	for _, tile := range buildResultsTiles(reports.ResultsBreakdown{}) {
		if tile.Label != "Line-synced" {
			continue
		}
		if strings.Contains(tile.Tooltip, "every word-capable source") {
			t.Errorf("Line-synced tooltip claims all word sources were checked: %q", tile.Tooltip)
		}
		if !strings.Contains(tile.Tooltip, "no word timing") {
			t.Errorf("Line-synced tooltip must state the file has no word timing: %q", tile.Tooltip)
		}
		return
	}
	t.Fatal("no Line-synced tile")
}
