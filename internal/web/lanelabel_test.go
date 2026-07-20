package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/reports"
)

func TestLaneLabel(t *testing.T) {
	tests := []struct {
		name string
		lane string
		want string
	}{
		{"detector lane gets the full display name", "detector", "Instrumental Detector"},
		{"provider lanes pass through unchanged", "musixmatch", "musixmatch"},
		{"unmapped lane passes through rather than blanking", "somefuturelane", "somefuturelane"},
		{"empty lane stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := laneLabel(tt.lane); got != tt.want {
				t.Errorf("laneLabel(%q) = %q; want %q", tt.lane, got, tt.want)
			}
		})
	}
}

// TestBuildProviderTilesAppliesLaneLabel covers the call site, not just the
// helper. TestLaneLabel alone would still pass if laneLabel were dropped from
// dashboard.go, since it exercises the function in isolation; this asserts the
// display name actually reaches the rendered tile.
func TestBuildProviderTilesAppliesLaneLabel(t *testing.T) {
	tiles := buildProviderTiles([]reports.ProviderEffectiveness{
		{Lane: "detector", Hits: 3, Misses: 1, HitRate: 0.75},
		{Lane: "musixmatch", Hits: 1, Misses: 1, HitRate: 0.5},
	})
	if len(tiles) != 2 {
		t.Fatalf("buildProviderTiles returned %d tiles; want 2", len(tiles))
	}
	if tiles[0].Label != "Instrumental Detector" {
		t.Errorf("detector tile Label = %q; want %q", tiles[0].Label, "Instrumental Detector")
	}
	if tiles[1].Label != "musixmatch" {
		t.Errorf("provider tile Label = %q; want it unchanged", tiles[1].Label)
	}
}

// TestBuildRecentRowsAppliesLaneLabel covers the DASHBOARD's recent-activity
// table, which is a separate surface from the Reports-page "recent outcomes"
// report even though both build a templates.RecentOutcomeRow. The first pass at
// #539 fixed only the Reports-page path and left this one rendering the raw
// "detector" string.
func TestBuildRecentRowsAppliesLaneLabel(t *testing.T) {
	rows := buildRecentRows([]reports.RecentOutcome{
		{Artist: "A", Title: "T", ProviderLane: "detector"},
		{Artist: "B", Title: "U", ProviderLane: "musixmatch"},
	}, time.UTC)
	if len(rows) != 2 {
		t.Fatalf("buildRecentRows returned %d rows; want 2", len(rows))
	}
	if rows[0].Lane != "Instrumental Detector" {
		t.Errorf("detector row Lane = %q; want %q", rows[0].Lane, "Instrumental Detector")
	}
	if rows[1].Lane != "musixmatch" {
		t.Errorf("provider row Lane = %q; want it unchanged", rows[1].Lane)
	}
}

// TestReportFragmentsShowLaneDisplayName covers the two buildReportView call
// sites end-to-end, through the real render path. Without these, reverting
// either laneLabel() call in ui.go would pass the whole suite silently: the
// helper-level tests above exercise the function in isolation, and
// TestBuildProviderTilesAppliesLaneLabel covers only the dashboard tiles.
func TestReportFragmentsShowLaneDisplayName(t *testing.T) {
	t.Run("recent outcomes", func(t *testing.T) {
		sqlDB := openReportsTestDB(t)
		insertDone(t, sqlDB, "Song", "detector", `[{"outdir":"/out","filename":"Song.txt"}]`, "2026-06-17T12:00:00Z")
		body := getFragment(t, newReportsUIServer(t, sqlDB), "recent-outcomes").Body.String()
		if !strings.Contains(body, "Instrumental Detector") {
			t.Errorf("recent-outcomes should render the display name; body:\n%s", body)
		}
	})

	t.Run("provider effectiveness", func(t *testing.T) {
		sqlDB := openReportsTestDB(t)
		insertDone(t, sqlDB, "Song", "detector", `[{"outdir":"/out","filename":"Song.txt"}]`, "2026-06-17T12:00:00Z")
		if _, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (1, 'detector', 1, ?)`,
			"2026-06-17T12:00:00Z"); err != nil {
			t.Fatalf("insert lane_attempts: %v", err)
		}
		body := getFragment(t, newReportsUIServer(t, sqlDB), "provider-effectiveness").Body.String()
		if !strings.Contains(body, "Instrumental Detector") {
			t.Errorf("provider-effectiveness should render the display name; body:\n%s", body)
		}
	})
}

// TestLaneLabelDoesNotChangePersistedValue pins the invariant that #539 renames
// only the DISPLAY name. The stored lane string is the primary key of
// provider_outcomes and the value written to work_queue.provider_lane, so if it
// ever changes, one lane's history splits across two keys and any query keyed on
// the old string silently returns zero instead of erroring. This test fails
// loudly if a future refactor moves the stored value.
func TestLaneLabelDoesNotChangePersistedValue(t *testing.T) {
	if detectorbackfill.LaneName != "detector" {
		t.Fatalf("persisted detector lane string = %q; want %q -- changing it splits "+
			"provider_outcomes history and zeroes existing queries", detectorbackfill.LaneName, "detector")
	}
	if got := laneLabel(detectorbackfill.LaneName); got == detectorbackfill.LaneName {
		t.Errorf("laneLabel(%q) returned the raw persisted value; the UI would render "+
			"the stored string instead of a display name", detectorbackfill.LaneName)
	}
}
