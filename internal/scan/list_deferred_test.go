package scan_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/cache"
	"github.com/sydlexius/mxlrcgo-svc/internal/library"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"
)

func TestRepo_ListDeferred(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	repo := scan.New(sqlDB)
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "A", TrackName: "B"},
		Outdir:   "/music",
		Filename: "a.lrc",
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Enqueue the pending row (creates the work_queue + junction link), then
	// dequeue and Defer it so the linked work_queue row is in 'deferred' state.
	enq := scan.Enqueuer{Results: repo, Cache: cache.New(sqlDB), Queue: queue.NewDBQueue(sqlDB), Priority: queue.PriorityScan}
	if _, _, err := enq.EnqueuePending(ctx, lib.ID); err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("benign miss")); err != nil {
		t.Fatalf("defer: %v", err)
	}

	got, err := repo.ListDeferred(ctx, scan.Filter{})
	if err != nil {
		t.Fatalf("list deferred: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListDeferred returned %d rows; want 1 (the deferred-miss track)", len(got))
	}
	if got[0].FilePath != "/music/a.mp3" {
		t.Errorf("FilePath = %q; want /music/a.mp3", got[0].FilePath)
	}

	// A library filter that excludes the row returns nothing.
	other := lib.ID + 999
	gotOther, err := repo.ListDeferred(ctx, scan.Filter{LibraryID: &other})
	if err != nil {
		t.Fatalf("list deferred (other lib): %v", err)
	}
	if len(gotOther) != 0 {
		t.Fatalf("ListDeferred(otherLib) = %d rows; want 0", len(gotOther))
	}
}
