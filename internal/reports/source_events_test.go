package reports_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
)

// TestSourceEvents (#1301): the range is inclusive at both ends, ordered by
// day, lane, event, and excludes days outside it.
func TestSourceEvents(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	for _, r := range [][4]any{
		{"2026-10-05", "petitlyrics", "line", 2},
		{"2026-10-05", "musixmatch", "word", 1},
		{"2026-10-05", "musixmatch", "hit", 3},
		{"2026-10-07", "musixmatch", "miss", 4},
		{"2026-10-08", "musixmatch", "hit", 9},
		{"2026-10-04", "musixmatch", "hit", 9},
	} {
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO source_event_daily(day, lane, event, count) VALUES(?, ?, ?, ?)`, r[0], r[1], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	repo := reports.New(sqlDB)
	got, err := repo.SourceEvents(ctx, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []reports.SourceEventCount{
		{"2026-10-05", "musixmatch", "hit", 3},
		{"2026-10-05", "musixmatch", "word", 1},
		{"2026-10-05", "petitlyrics", "line", 2},
		{"2026-10-07", "musixmatch", "miss", 4},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if none, err := repo.SourceEvents(ctx, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)); err != nil || len(none) != 0 {
		t.Errorf("inverted range = %v, %v; want empty", none, err)
	}
}
