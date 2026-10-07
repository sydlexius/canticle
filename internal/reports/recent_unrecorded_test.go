package reports_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// TestRecentOutcomesUnrecordedBlanksLaneAndExplains pins #654: a row whose
// outcome was never recorded (ResultUnknown) shows NO provider lane, and its
// Detail coalesces stored detail > timing verdict > failsig-normalized
// last_error > the legacy literal (#654 AC4: a last_error that explains the
// row's state is reachable from the UI, and only ever normalized). A blank or
// whitespace-only last_error explains nothing and falls through to the
// literal. Rows with a recorded outcome keep their lane, and a miss
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
	// prune.retireUnresolvable's shape: settled done, no outcome, the sentinel
	// as last_error.
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "prune-retired", status: "done",
		completedAt: "2026-08-16T04:35:00Z", providerLane: "musixmatch",
		lastError: queue.UnresolvableGoneError,
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
	// A prune retirement is bookkeeping, not a fetch outcome (#740): absent.
	if _, ok := byTitle["prune-retired"]; ok {
		t.Error("prune-retired row listed in Recent outcomes; a retirement is not a fetch outcome")
	}
	for _, tc := range []struct {
		title      string
		wantResult reports.ResultClass
		wantLane   string
		wantDetail string
	}{
		{"legacy-stale-lane", reports.ResultUnknown, "", reports.LegacyNoOutcomeDetail},
		{"legacy-whitespace-error", reports.ResultUnknown, "", reports.LegacyNoOutcomeDetail},
		{"legacy-error", reports.ResultUnknown, "", `output dir "<path>": permission denied`},
		{"quarantined", reports.ResultUnknown, "musixmatch", "timing refused: categorical"},
		{"remediated", reports.ResultUnknown, "petitlyrics", "timing refused: mis_synced"},
		{"synced", reports.ResultSynced, "musixmatch", ""},
		{"exhausted-miss", reports.ResultMiss, "musixmatch", ""},
	} {
		o, ok := byTitle[tc.title]
		if !ok {
			t.Fatalf("row %q missing from results", tc.title)
		}
		if strings.Contains(o.Detail, "/data/library") || strings.Contains(o.Detail, "ignored in favor") ||
			strings.Contains(o.Detail, "stale error") {
			t.Errorf("%s: Detail %q leaks raw or out-ranked last_error", tc.title, o.Detail)
		}
		if o.Result != tc.wantResult || o.ProviderLane != tc.wantLane || o.Detail != tc.wantDetail {
			t.Errorf("%s: got result=%q lane=%q detail=%q; want result=%q lane=%q detail=%q",
				tc.title, o.Result, o.ProviderLane, o.Detail, tc.wantResult, tc.wantLane, tc.wantDetail)
		}
	}
}

// TestRecentOutcomesExcludesRetirementsBeforeTheLimit pins #740's operator
// harm, which an absence check alone does not: a sweep stamps completed_at on
// every row it retires, so a batch of retirements is the NEWEST thing in the
// table and, if filtered only after the limit, would push every genuine fetch
// outcome out of the window. Both entry points share recentWhere; each is
// asserted so a future split cannot drop the exclusion from one. The retired
// row keeps a stale word-synced outcome, as retireUnresolvable leaves it.
func TestRecentOutcomesExcludesRetirementsBeforeTheLimit(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "fetched", status: "done",
		completedAt: "2026-08-16T04:00:00Z", outcomeType: "synced", syncTier: "line",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "retired", status: "done",
		completedAt: "2026-08-16T05:00:00Z", outcomeType: "synced", syncTier: "word",
		lastError: queue.UnresolvableGoneError,
	})

	plain, err := repo.RecentOutcomes(ctx, 1)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	sorted, err := repo.RecentOutcomesSorted(ctx, 1, reports.RecentOutcomesSpec.Default)
	if err != nil {
		t.Fatalf("RecentOutcomesSorted: %v", err)
	}
	for name, got := range map[string][]reports.RecentOutcome{"RecentOutcomes": plain, "RecentOutcomesSorted": sorted} {
		if len(got) != 1 || got[0].Title != "fetched" {
			titles := make([]string, len(got))
			for i, o := range got {
				titles[i] = o.Title
			}
			t.Errorf("%s(limit 1) = %v, want [fetched]: a newer prune retirement must not occupy the window", name, titles)
		}
	}
}
