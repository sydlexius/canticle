package reports_test

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

func TestInFlightLiveClaimAndOrphan(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	// The updated_at trigger would restamp the seeded orphan time to now.
	if _, err := sqlDB.ExecContext(ctx, `DROP TRIGGER update_work_queue_updated_at`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	// An orphan: processing since long ago (pre-migration shape, no claimed_at,
	// so ClaimedAt falls back to updated_at).
	orphan := insertWorkItem(t, sqlDB, workItem{artist: "OA", title: "orphan", album: "OAlb", status: "processing"})
	setWorkItemColumn(t, sqlDB, orphan, "updated_at", true, "2026-01-01T00:00:00Z")
	// A live claim made through the real claim path, plus rows that must not appear.
	live := insertWorkItem(t, sqlDB, workItem{artist: "LA", title: "live", album: "LAlb", status: "pending"})
	insertWorkItem(t, sqlDB, workItem{artist: "X", title: "waiting", status: "pending"})
	insertWorkItem(t, sqlDB, workItem{artist: "X", title: "finished", status: "done"})
	setWorkItemColumn(t, sqlDB, live, "priority", true, 10) // claimed first

	before := time.Now().UTC().Add(-2 * time.Second)
	item, err := queue.NewDBQueue(sqlDB).Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if item.ID != live {
		t.Fatalf("claimed id %d, want %d", item.ID, live)
	}

	got, err := reports.New(sqlDB).InFlight(ctx)
	if err != nil {
		t.Fatalf("InFlight: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d in-flight rows, want 2 (live + orphan): %+v", len(got), got)
	}
	if got[0].ID != orphan || got[0].Artist != "OA" || got[0].Album != "OAlb" {
		t.Errorf("first row = %+v, want the orphan (oldest claim first)", got[0])
	}
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !got[0].ClaimedAt.Equal(want) {
		t.Errorf("orphan ClaimedAt = %v, want fallback to updated_at %v", got[0].ClaimedAt, want)
	}
	if got[1].ID != live || got[1].Title != "live" {
		t.Errorf("second row = %+v, want the live claim", got[1])
	}
	if got[1].ClaimedAt.Before(before) || got[1].ClaimedAt.After(time.Now().UTC().Add(2*time.Second)) {
		t.Errorf("live ClaimedAt = %v, want about now", got[1].ClaimedAt)
	}
}

// A completion stamp written while the row is still processing restamps
// updated_at via the trigger; ClaimedAt must not move with it.
func TestInFlightClaimedAtSurvivesStamps(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "t", status: "pending"})
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	var stamped *string
	if err := sqlDB.QueryRowContext(ctx, `SELECT claimed_at FROM work_queue WHERE id = ?`, id).Scan(&stamped); err != nil {
		t.Fatalf("read claimed_at: %v", err)
	}
	if stamped == nil || *stamped == "" {
		t.Fatal("claim did not stamp claimed_at")
	}
	// Backdate the claim, then stamp: the trigger bumps updated_at to now.
	setWorkItemColumn(t, sqlDB, id, "claimed_at", true, "2026-02-02T03:04:05Z")
	if err := q.SetOutcomeType(ctx, id, "synced"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got, err := reports.New(sqlDB).InFlight(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("InFlight = %+v, %v", got, err)
	}
	if want := time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC); !got[0].ClaimedAt.Equal(want) {
		t.Errorf("ClaimedAt = %v, want %v (must not follow updated_at)", got[0].ClaimedAt, want)
	}
}

func TestInFlightEmptyAndUnparsable(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	got, err := repo.InFlight(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "t", status: "processing"})
	setWorkItemColumn(t, sqlDB, id, "claimed_at", true, "garbage")
	got, err = repo.InFlight(ctx)
	if err != nil || len(got) != 1 || !got[0].ClaimedAt.IsZero() {
		t.Fatalf("unparsable = %+v, %v; want one row with zero ClaimedAt", got, err)
	}
}
