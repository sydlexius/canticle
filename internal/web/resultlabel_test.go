package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		{"unknown passes through unchanged", reports.ResultUnknown, "unknown"},
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
		{"unknown has no tier badge", reports.ResultUnknown, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resultTierClass(tt.rc); got != tt.want {
				t.Errorf("resultTierClass(%q) = %q, want %q", tt.rc, got, tt.want)
			}
		})
	}
}

// TestBuildSyncTierTilesNeverMerges asserts the three dashboard tiles are
// always separate and always all three present (#627 AC: "Dashboard counters
// do not merge the two tiers into one number"), including the zero case so a
// fresh install with no synced rows yet still shows three explicit zeros
// rather than omitting the section.
func TestBuildSyncTierTilesNeverMerges(t *testing.T) {
	tiles := buildSyncTierTiles(reports.SyncTierCounts{WordSynced: 5, LineSynced: 3, Unknown: 2})
	if len(tiles) != 3 {
		t.Fatalf("buildSyncTierTiles returned %d tiles, want 3", len(tiles))
	}
	want := map[string]string{
		"Word-synced":           "5",
		"Line-synced":           "3",
		"Synced (tier unknown)": "2",
	}
	for _, tile := range tiles {
		wantVal, ok := want[tile.Label]
		if !ok {
			t.Errorf("unexpected tile label %q", tile.Label)
			continue
		}
		if tile.Value != wantVal {
			t.Errorf("tile %q value = %q, want %q", tile.Label, tile.Value, wantVal)
		}
	}

	zero := buildSyncTierTiles(reports.SyncTierCounts{})
	if len(zero) != 3 {
		t.Fatalf("buildSyncTierTiles on zero counts returned %d tiles, want 3 (never omitted)", len(zero))
	}
	for _, tile := range zero {
		if tile.Value != "0" {
			t.Errorf("zero-state tile %q value = %q, want \"0\"", tile.Label, tile.Value)
		}
	}
}

// TestDashboardShowsSyncTierTiles renders the real dashboard page (#627) over a
// database seeded with one word-synced, one line-synced, and one
// tier-unknown (legacy) row, and asserts the three counts render as SEPARATE
// tiles rather than a single merged "Synced" number. This is the AC-level
// check: it fails if a future change re-merges the tiers even though the unit
// tests on buildSyncTierTiles still pass in isolation.
func TestDashboardShowsSyncTierTiles(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDoneWithWordTiming(t, sqlDB, "WS", pathsJSONWeb("ws.lrc"), "2026-06-17T10:00:00Z", "served")
	insertDoneWithWordTiming(t, sqlDB, "LS", pathsJSONWeb("ls.lrc"), "2026-06-17T11:00:00Z", "absent")
	insertDoneWithWordTiming(t, sqlDB, "TU", pathsJSONWeb("tu.lrc"), "2026-06-17T12:00:00Z", "")
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Sync Tier") {
		t.Fatal("dashboard missing the Sync Tier section heading")
	}
	for _, want := range []string{"Word-synced", "Line-synced", "Synced (tier unknown)"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing sync-tier tile label %q; body:\n%s", want, body)
		}
	}
	// Each tier tile shows its own count of 1, not a merged 3 -- this is the
	// AC's "never merged into one number". A merged implementation would render
	// a single ">3<" tile value instead of three ">1<"s.
	if strings.Count(body, `<span class="mx-dash-tile-value">1</span>`) < 3 {
		t.Errorf("expected at least 3 tiles showing value 1 (one per tier); body:\n%s", body)
	}

	// Recent Outcomes carries the tier badges too. Asserted on the BADGE
	// element, not a bare substring: "word-synced"/"line-synced" also appear in
	// the Sync Tier section's own title="" attribute (see dashSyncTierTiles'
	// tooltip), so a bare Contains would pass even if resultLabel stopped
	// emitting the hyphenated form in the Recent Outcomes table itself
	// (mutation-checked: renaming resultLabel's output to "word_synced" /
	// "line_synced" reddens only this assertion, not a bare-substring one).
	for _, want := range []string{">word-synced</span>", ">line-synced</span>", "mx-result-tier-word", "mx-result-tier-line", "mx-result-tier-unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard Recent Outcomes missing %q; body:\n%s", want, body)
		}
	}
}

// TestReportsRecentOutcomesShowsSyncTiers is the Reports-workspace equivalent
// of TestDashboardShowsSyncTierTiles: the same three rows, rendered through
// the /reports/recent-outcomes fragment instead of the dashboard, since the
// two surfaces share the RecentOutcomeRow view model but render from
// independent call sites (ui.go's buildReportView vs dashboard.go's
// buildRecentRows) that could drift.
func TestReportsRecentOutcomesShowsSyncTiers(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDoneWithWordTiming(t, sqlDB, "WS", pathsJSONWeb("ws.lrc"), "2026-06-17T10:00:00Z", "served")
	insertDoneWithWordTiming(t, sqlDB, "LS", pathsJSONWeb("ls.lrc"), "2026-06-17T11:00:00Z", "absent")
	insertDoneWithWordTiming(t, sqlDB, "TU", pathsJSONWeb("tu.lrc"), "2026-06-17T12:00:00Z", "")
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
