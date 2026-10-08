package reports_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// TestUpNextCarriesIDAndManualFlag pins #1434: each Up Next row carries its
// work_queue id and the manual-instrumental flag, while the order (batch_seq)
// and the limit are unchanged. Synthetic fixtures.
func TestUpNextCarriesIDAndManualFlag(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Inserted out of batch order so a wrong ORDER BY (id) would show.
	c := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "third", status: "pending", batchSeq: 3})
	a := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "first", status: "failed", batchSeq: 1})
	b := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "second", status: "deferred", batchSeq: 2})
	setWorkItemColumn(t, sqlDB, b, "manual_instrumental_at", true, "2026-01-01T00:00:00Z")
	// A claimed row is never listed (status predicate), so it can show no action.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "claimed", status: "processing", batchSeq: 4})

	got, err := repo.UpNext(ctx, 10)
	if err != nil {
		t.Fatalf("UpNext: %v", err)
	}
	want := []struct {
		id     int64
		title  string
		manual bool
	}{{a, "first", false}, {b, "second", true}, {c, "third", false}}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Title != w.title || got[i].ManualInstrumental != w.manual {
			t.Errorf("row %d = id %d %q manual=%v, want id %d %q manual=%v",
				i, got[i].ID, got[i].Title, got[i].ManualInstrumental, w.id, w.title, w.manual)
		}
	}
	top, err := repo.UpNext(ctx, 2)
	if err != nil || len(top) != 2 || top[0].ID != a || top[1].ID != b {
		t.Errorf("limit 2 = %+v (err %v), want the first two in batch order", top, err)
	}
}

// TestNeedsAttentionCarriesIDAndManualFlag pins #1434 for the Needs attention
// rows: the id was already selected, and the manual flag now rides along without
// changing membership or order (failed before deferred, newest first).
func TestNeedsAttentionCarriesIDAndManualFlag(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	f := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "f", status: "failed", lastError: "boom", attempts: 1})
	d := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1})
	setWorkItemColumn(t, sqlDB, d, "manual_instrumental_at", true, "2026-01-01T00:00:00Z")
	// A claimed row is not in the failed/deferred population.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "p", status: "processing"})

	got, err := repo.NeedsAttention(ctx, 10)
	if err != nil {
		t.Fatalf("NeedsAttention: %v", err)
	}
	if len(got) != 2 || got[0].ID != f || got[0].ManualInstrumental || got[1].ID != d || !got[1].ManualInstrumental {
		t.Errorf("rows = %+v, want failed %d unmarked then deferred %d marked", got, f, d)
	}
	one, err := repo.NeedsAttention(ctx, 1)
	if err != nil || len(one) != 1 || one[0].ID != f {
		t.Errorf("limit 1 = %+v (err %v), want the failed row", one, err)
	}
}
