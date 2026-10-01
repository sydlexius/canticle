package reports_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// TestRecentOutcomesUnrecordedBlanksLaneAndExplains pins #654: a row whose
// outcome was never recorded (ResultUnknown) shows NO provider lane, and its
// Detail coalesces stored detail > timing verdict > the legacy literal.
// last_error is never surfaced (#1167 tracks hardening the normalizer first). Rows with a recorded outcome keep their lane, and a miss
// does not echo its own sentinel as a reason. A row with a recorded timing
// verdict (categorical, remediated mis_synced) KEEPS its lane.
func TestRecentOutcomesUnrecordedBlanksLaneAndExplains(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "legacy-stale-lane", status: "done",
		completedAt: "2026-08-16T05:00:00Z", providerLane: "musixmatch",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "legacy-whitespace-error", status: "done",
		completedAt: "2026-08-16T04:50:00Z", lastError: "   ",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "legacy-error", status: "done",
		completedAt: "2026-08-16T04:40:00Z", providerLane: "petitlyrics",
		lastError: `output dir "/data/library/private": permission denied`,
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "quarantined", status: "done",
		completedAt: "2026-08-16T04:30:00Z", providerLane: "musixmatch",
		timingOutcome: "categorical", lastError: "ignored in favor of timing verdict",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "remediated", status: "done",
		completedAt: "2026-08-16T04:25:00Z", providerLane: "petitlyrics",
		timingOutcome: "mis_synced",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "synced", status: "done",
		completedAt: "2026-08-16T04:20:00Z", providerLane: "musixmatch",
		outcomeType: "synced", lastError: "stale error on a recorded outcome",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "exhausted-miss", status: "unavailable",
		completedAt: "2026-08-16T04:10:00Z", providerLane: "musixmatch",
		lastError: "miss limit reached",
	})

	got, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	byTitle := make(map[string]reports.RecentOutcome, len(got))
	for _, o := range got {
		byTitle[o.Title] = o
	}
	for _, tc := range []struct {
		title      string
		wantResult reports.ResultClass
		wantLane   string
		wantDetail string
	}{
		{"legacy-stale-lane", reports.ResultUnknown, "", reports.LegacyNoOutcomeDetail},
		{"legacy-whitespace-error", reports.ResultUnknown, "", reports.LegacyNoOutcomeDetail},
		{"legacy-error", reports.ResultUnknown, "", reports.LegacyNoOutcomeDetail},
		{"quarantined", reports.ResultUnknown, "musixmatch", "timing refused: categorical"},
		{"remediated", reports.ResultUnknown, "petitlyrics", "timing refused: mis_synced"},
		{"synced", reports.ResultSynced, "musixmatch", ""},
		{"exhausted-miss", reports.ResultMiss, "musixmatch", ""},
	} {
		o, ok := byTitle[tc.title]
		if !ok {
			t.Fatalf("row %q missing from results", tc.title)
		}
		if strings.Contains(o.Detail, "/data/library") || strings.Contains(o.Detail, "permission denied") {
			t.Errorf("%s: Detail %q leaks last_error", tc.title, o.Detail)
		}
		if o.Result != tc.wantResult || o.ProviderLane != tc.wantLane || o.Detail != tc.wantDetail {
			t.Errorf("%s: got result=%q lane=%q detail=%q; want result=%q lane=%q detail=%q",
				tc.title, o.Result, o.ProviderLane, o.Detail, tc.wantResult, tc.wantLane, tc.wantDetail)
		}
	}
}
