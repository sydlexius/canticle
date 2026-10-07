package reports_test

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
)

// TestBuildSourceTrendGap (#1302): a day with no attempts has a nil hit rate
// (a gap), never 0 percent; a day with only misses is a real 0.
func TestBuildSourceTrendGap(t *testing.T) {
	to := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	rows := []reports.SourceEventCount{
		{Day: "2026-10-03", Lane: "musixmatch", Event: "hit", Count: 3},
		{Day: "2026-10-03", Lane: "musixmatch", Event: "miss", Count: 1},
		{Day: "2026-10-05", Lane: "musixmatch", Event: "miss", Count: 2},
		{Day: "2026-10-05", Lane: "musixmatch", Event: "line", Count: 4},
		{Day: "2026-10-04", Lane: "petitlyrics", Event: "hit", Count: 9},
	}
	got := reports.BuildSourceTrend(rows, "musixmatch", to, 3)
	if !got.HasHistory || len(got.Days) != 3 {
		t.Fatalf("history=%v days=%d", got.HasHistory, len(got.Days))
	}
	if got.Days[0].Day != "2026-10-03" || got.Days[2].Day != "2026-10-05" {
		t.Fatalf("days = %v", got.Days)
	}
	if hr := got.Days[0].HitRate; hr == nil || *hr != 75 {
		t.Errorf("day 1 hit rate = %v, want 75", hr)
	}
	if hr := got.Days[1].HitRate; hr != nil {
		t.Errorf("no-attempt day hit rate = %v, want nil (gap), not 0", *hr)
	}
	if hr := got.Days[2].HitRate; hr == nil || *hr != 0 {
		t.Errorf("all-miss day hit rate = %v, want 0", hr)
	}
	if got.Days[2].Line != 4 {
		t.Errorf("line = %d, want 4", got.Days[2].Line)
	}
	if none := reports.BuildSourceTrend(rows, "innertube", to, 3); none.HasHistory {
		t.Error("lane with no rows must have no history")
	}
}

func TestRepoSourceTrend(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, r := range [][4]any{
		{"2026-10-05", "musixmatch", "hit", 2},
		{"2026-07-01", "musixmatch", "hit", 5}, // outside the 90-day window
		{"2026-08-01", "petitlyrics", "miss", 1},
	} {
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO source_event_daily(day, lane, event, count) VALUES(?, ?, ?, ?)`, r[0], r[1], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	repo := reports.New(sqlDB)
	tr, err := repo.SourceTrend(ctx, "musixmatch", now, 7)
	if err != nil || !tr.HasHistory || len(tr.Days) != 7 || tr.Days[6].Hits != 2 {
		t.Fatalf("trend = %+v, %v", tr, err)
	}
	// History is judged over 90 days: petitlyrics has rows, none in a 7-day view.
	tr, err = repo.SourceTrend(ctx, "petitlyrics", now, 7)
	if err != nil || !tr.HasHistory || tr.Days[0].HitRate != nil {
		t.Fatalf("petitlyrics trend = %+v, %v", tr, err)
	}
	if tr, _ = repo.SourceTrend(ctx, "innertube", now, 7); tr.HasHistory {
		t.Error("innertube must have no history")
	}
}
