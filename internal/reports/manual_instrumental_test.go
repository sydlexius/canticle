package reports_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// #1405: a hand-marked instrumental is Finished (no sweep will ever upgrade
// it), its bucket and recent-outcome rows carry the flag, and its lane renders
// as Manual. A provider-written instrumental stays SettledUpgradable.
func TestManualInstrumentalReportsAsFinishedAndFlagged(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	marked := insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "marked", status: "done", outcomeType: "instrumental",
		completedAt: "2026-08-16T05:00:00Z", providerLane: queue.ManualLane,
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "detected", status: "done", outcomeType: "instrumental",
		completedAt: "2026-08-16T04:00:00Z", providerLane: "detector",
	})
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET manual_instrumental_at = '2026-08-16T05:00:00Z' WHERE id = ?`, marked); err != nil {
		t.Fatal(err)
	}

	sum, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Finished != 1 || sum.SettledUpgradable != 1 {
		t.Errorf("Finished=%d SettledUpgradable=%d, want 1/1", sum.Finished, sum.SettledUpgradable)
	}

	for _, b := range []reports.Bucket{reports.BucketFinished, reports.BucketSettled} {
		rows, err := repo.ListBucket(ctx, b, 0, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("bucket %s: rows=%d err=%v, want 1", b, len(rows), err)
		}
		if want := b == reports.BucketFinished; rows[0].ManualInstrumental != want {
			t.Errorf("bucket %s: ManualInstrumental = %v, want %v", b, rows[0].ManualInstrumental, want)
		}
	}

	recent, err := repo.RecentOutcomes(ctx, 10)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent: %d rows, err %v", len(recent), err)
	}
	for _, o := range recent {
		if want := o.Title == "marked"; o.ManualInstrumental != want {
			t.Errorf("recent %q: ManualInstrumental = %v, want %v", o.Title, o.ManualInstrumental, want)
		}
	}
	if got := reports.LaneLabel(queue.ManualLane); got != "Manual" {
		t.Errorf("LaneLabel(manual) = %q, want Manual", got)
	}
}
