package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

func TestNext_EmptyQueue(t *testing.T) {
	q := NewInputsQueue()
	_, err := q.Next()
	if err == nil {
		t.Fatal("expected error from Next() on empty queue, got nil")
	}
}

func TestPop_EmptyQueue(t *testing.T) {
	q := NewInputsQueue()
	_, err := q.Pop()
	if err == nil {
		t.Fatal("expected error from Pop() on empty queue, got nil")
	}
}

func TestNext_NonEmptyQueue(t *testing.T) {
	q := NewInputsQueue()
	item := models.Inputs{
		Track:  models.Track{ArtistName: "Artist", TrackName: "Track"},
		Outdir: "out",
	}
	q.Push(item)

	got, err := q.Next()
	if err != nil {
		t.Fatalf("unexpected error from Next() on non-empty queue: %v", err)
	}
	if got.Track.ArtistName != item.Track.ArtistName || got.Track.TrackName != item.Track.TrackName {
		t.Fatalf("got %+v; want %+v", got, item)
	}
	// Queue should be unchanged (Next is non-destructive)
	if q.Len() != 1 {
		t.Fatalf("queue length should be 1 after Next(), got %d", q.Len())
	}
}

func TestPop_NonEmptyQueue(t *testing.T) {
	q := NewInputsQueue()
	item := models.Inputs{
		Track:  models.Track{ArtistName: "Artist", TrackName: "Track"},
		Outdir: "out",
	}
	q.Push(item)

	got, err := q.Pop()
	if err != nil {
		t.Fatalf("unexpected error from Pop() on non-empty queue: %v", err)
	}
	if got.Track.ArtistName != item.Track.ArtistName || got.Track.TrackName != item.Track.TrackName {
		t.Fatalf("got %+v; want %+v", got, item)
	}
	// Queue should be shortened by 1
	if q.Len() != 0 {
		t.Fatalf("queue length should be 0 after Pop(), got %d", q.Len())
	}
}

func openQueueTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func TestDBQueue_EnqueueDedupesNormalizedArtistTitle(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	first, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "  Héllo  ", TrackName: " Wörld "},
		Outdir:   "out-a",
		Filename: "a.lrc",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	second, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "hello", TrackName: "world"},
		Outdir:   "out-b",
		Filename: "b.lrc",
	}, 5)
	if err != nil {
		t.Fatalf("Enqueue duplicate: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate enqueue ID = %d; want %d", second.ID, first.ID)
	}
	if second.Priority != 5 {
		t.Fatalf("duplicate enqueue priority = %d; want 5", second.Priority)
	}

	var count int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue`).Scan(&count); err != nil {
		t.Fatalf("count work_queue: %v", err)
	}
	if count != 1 {
		t.Fatalf("work_queue rows = %d; want 1", count)
	}
}

func TestDBQueue_CountByStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	// Empty queue: no rows, so no status keys.
	counts, err := q.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus empty: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("empty counts = %v; want no entries", counts)
	}

	// Two pending rows.
	for _, name := range []string{"One", "Two"} {
		if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: name}}, PriorityScan); err != nil {
			t.Fatalf("Enqueue %s: %v", name, err)
		}
	}
	// Claim one (pending -> processing) and complete it (processing -> done).
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, claimed.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	counts, err = q.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[StatusPending] != 1 {
		t.Errorf("pending = %d; want 1", counts[StatusPending])
	}
	if counts[StatusDone] != 1 {
		t.Errorf("done = %d; want 1", counts[StatusDone])
	}
	if _, ok := counts[StatusProcessing]; ok {
		t.Errorf("processing present = %v; want absent (no processing rows)", counts[StatusProcessing])
	}
}

func TestDBQueue_DequeueClaimsHighestPriorityReadyItem(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Low", TrackName: "Ready"}}, 1); err != nil {
		t.Fatalf("Enqueue low: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "High", TrackName: "Ready"}}, 10); err != nil {
		t.Fatalf("Enqueue high: %v", err)
	}

	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if got.Inputs.Track.ArtistName != "High" {
		t.Fatalf("dequeued artist = %q; want High", got.Inputs.Track.ArtistName)
	}
	if got.Status != StatusProcessing {
		t.Fatalf("dequeued status = %q; want %q", got.Status, StatusProcessing)
	}
}

func TestDBQueue_DequeueKeepsFIFOWithinSamePriority(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	inputs := []models.Inputs{
		{Track: models.Track{ArtistName: "Artist", TrackName: "One"}},
		{Track: models.Track{ArtistName: "Artist", TrackName: "Two"}},
		{Track: models.Track{ArtistName: "Artist", TrackName: "Three"}},
	}
	for _, v := range inputs {
		if _, err := q.Enqueue(ctx, v, PriorityScan); err != nil {
			t.Fatalf("Enqueue %q: %v", v.Track.TrackName, err)
		}
	}

	first, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue first: %v", err)
	}
	second, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue second: %v", err)
	}
	third, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue third: %v", err)
	}

	if first.Inputs.Track.TrackName != "One" || second.Inputs.Track.TrackName != "Two" || third.Inputs.Track.TrackName != "Three" {
		t.Fatalf("dequeue order = %q, %q, %q; want One, Two, Three",
			first.Inputs.Track.TrackName, second.Inputs.Track.TrackName, third.Inputs.Track.TrackName)
	}
}

func TestDBQueue_DequeuePrioritizesWebhookAheadOfScanBacklog(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Scan", TrackName: "One"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue scan one: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Scan", TrackName: "Two"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue scan two: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Webhook", TrackName: "Now"}}, PriorityWebhook); err != nil {
		t.Fatalf("Enqueue webhook: %v", err)
	}

	first, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue first: %v", err)
	}
	second, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue second: %v", err)
	}
	third, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue third: %v", err)
	}

	if first.Inputs.Track.ArtistName != "Webhook" {
		t.Fatalf("first dequeue artist = %q; want Webhook", first.Inputs.Track.ArtistName)
	}
	if second.Inputs.Track.TrackName != "One" || third.Inputs.Track.TrackName != "Two" {
		t.Fatalf("scan dequeue order = %q, %q; want One, Two", second.Inputs.Track.TrackName, third.Inputs.Track.TrackName)
	}
}

func TestDBQueue_EnqueueDuplicateDoesNotRequeueProcessingItem(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "claimed-out",
		Filename: "claimed.lrc",
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	dup, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: " artist ", TrackName: "title"},
		Outdir:   "duplicate-out",
		Filename: "duplicate.lrc",
	}, 10)
	if err != nil {
		t.Fatalf("Enqueue duplicate: %v", err)
	}
	if dup.ID != claimed.ID {
		t.Fatalf("duplicate ID = %d; want %d", dup.ID, claimed.ID)
	}
	if dup.Status != StatusProcessing {
		t.Fatalf("duplicate status = %q; want %q", dup.Status, StatusProcessing)
	}
	if dup.Inputs.Outdir != "claimed-out" || dup.Inputs.Filename != "claimed.lrc" {
		t.Fatalf("duplicate payload = %q/%q; want claimed-out/claimed.lrc", dup.Inputs.Outdir, dup.Inputs.Filename)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue after processing duplicate error = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_EnqueueDuplicatePreservesFailedBackoff(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "failed-out",
		Filename: "failed.lrc",
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	failed, err := q.Fail(ctx, claimed.ID, errors.New("rate limited"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}

	dup, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: " artist ", TrackName: "title"},
		Outdir:   "duplicate-out",
		Filename: "duplicate.lrc",
	}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue duplicate: %v", err)
	}
	if dup.ID != failed.ID {
		t.Fatalf("duplicate ID = %d; want %d", dup.ID, failed.ID)
	}
	if dup.Status != StatusFailed {
		t.Fatalf("duplicate status = %q; want %q", dup.Status, StatusFailed)
	}
	if dup.Attempts != failed.Attempts {
		t.Fatalf("duplicate attempts = %d; want %d", dup.Attempts, failed.Attempts)
	}
	if !dup.NextAttemptAt.Equal(failed.NextAttemptAt) {
		t.Fatalf("duplicate next attempt = %s; want %s", dup.NextAttemptAt, failed.NextAttemptAt)
	}
	if dup.LastError != failed.LastError {
		t.Fatalf("duplicate last error = %q; want %q", dup.LastError, failed.LastError)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue during preserved backoff error = %v; want sql.ErrNoRows", err)
	}
}

// TestDBQueue_EnqueueWebhookDuplicateResetsFailedBackoff verifies a webhook
// (high-priority) duplicate of a failed row resets next_attempt_at to now so the
// work becomes immediately retry-eligible, without changing the attempt count.
func TestDBQueue_EnqueueWebhookDuplicateResetsFailedBackoff(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "failed-out",
		Filename: "failed.lrc",
	}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	failed, err := q.Fail(ctx, claimed.ID, errors.New("rate limited"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if !failed.NextAttemptAt.After(now) {
		t.Fatalf("failed next attempt = %s; want a future backoff after %s", failed.NextAttemptAt, now)
	}

	dup, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: " artist ", TrackName: "title"},
		Outdir:   "duplicate-out",
		Filename: "duplicate.lrc",
	}, PriorityWebhook)
	if err != nil {
		t.Fatalf("Enqueue webhook duplicate: %v", err)
	}
	if dup.ID != failed.ID {
		t.Fatalf("duplicate ID = %d; want %d", dup.ID, failed.ID)
	}
	if dup.Status != StatusFailed {
		t.Fatalf("duplicate status = %q; want %q (status is unchanged)", dup.Status, StatusFailed)
	}
	if dup.Attempts != failed.Attempts {
		t.Fatalf("duplicate attempts = %d; want %d (attempts preserved)", dup.Attempts, failed.Attempts)
	}
	if !dup.NextAttemptAt.Equal(now) {
		t.Fatalf("duplicate next attempt = %s; want %s (reset to now)", dup.NextAttemptAt, now)
	}
	if dup.Priority != PriorityWebhook {
		t.Fatalf("duplicate priority = %d; want %d", dup.Priority, PriorityWebhook)
	}
	claimed2, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after webhook reset = %v; want the reset row to be claimable", err)
	}
	if claimed2.ID != failed.ID {
		t.Fatalf("Dequeue claimed ID = %d; want %d", claimed2.ID, failed.ID)
	}
}

func TestDBQueue_EnqueuePersistsOutputPaths(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Artist", TrackName: "Title"},
		OutputPaths: []models.OutputPath{
			{Outdir: "out-a", Filename: "a.lrc"},
			{Outdir: "out-b", Filename: "b.lrc"},
		},
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(item.Inputs.OutputPaths) != 2 {
		t.Fatalf("enqueued output paths = %+v; want 2 paths", item.Inputs.OutputPaths)
	}

	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(got.Inputs.OutputPaths) != 2 {
		t.Fatalf("dequeued output paths = %+v; want 2 paths", got.Inputs.OutputPaths)
	}
	if got.Inputs.OutputPaths[0].Outdir != "out-a" || got.Inputs.OutputPaths[1].Filename != "b.lrc" {
		t.Fatalf("dequeued output paths = %+v; want persisted paths", got.Inputs.OutputPaths)
	}
}

func TestDBQueue_EnqueuePersistsSourcePath(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     "out",
		Filename:   "artist-title.lrc",
		SourcePath: "/music/artist-title.flac",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if item.Inputs.SourcePath != "/music/artist-title.flac" {
		t.Fatalf("enqueued source path = %q; want source path", item.Inputs.SourcePath)
	}

	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if got.Inputs.SourcePath != "/music/artist-title.flac" {
		t.Fatalf("dequeued source path = %q; want source path", got.Inputs.SourcePath)
	}
}

func insertScanResult(t *testing.T, sqlDB *sql.DB, filePath string) int64 {
	t.Helper()
	ctx := context.Background()
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?)`,
		filepath.Dir(filePath), "test")
	if err != nil {
		t.Fatalf("insert library: %v", err)
	}
	libID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library id: %v", err)
	}
	res, err = sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, status) VALUES (?, ?, 'processing')`,
		libID, filePath)
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	return id
}

func TestDBQueue_CompleteAtomicallyWritesScanResultsDone(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	scanID := insertScanResult(t, sqlDB, "/music/atomic.mp3")
	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Atomic"},
		ScanResultID: scanID,
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var queueStatus, scanStatus string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM work_queue WHERE id = ?`, item.ID,
	).Scan(&queueStatus); err != nil {
		t.Fatalf("read work_queue: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM scan_results WHERE id = ?`, scanID,
	).Scan(&scanStatus); err != nil {
		t.Fatalf("read scan_results: %v", err)
	}
	if queueStatus != "done" {
		t.Fatalf("work_queue status = %q; want done", queueStatus)
	}
	if scanStatus != "done" {
		t.Fatalf("scan_results status = %q; want done (Complete must atomically flip both ledgers)", scanStatus)
	}
}

func TestDBQueue_CompleteWithoutScanResultIDLeavesLedgerUntouched(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	// Webhook-style enqueue with no originating scan_result should still
	// complete cleanly without touching scan_results.
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Artist", TrackName: "Adhoc"},
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM work_queue WHERE id = ?`, item.ID,
	).Scan(&status); err != nil {
		t.Fatalf("read work_queue: %v", err)
	}
	if status != "done" {
		t.Fatalf("work_queue status = %q; want done", status)
	}
}

func TestDBQueue_CompleteWritesBackAllLinkedScanResults(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	scanID1 := insertScanResult(t, sqlDB, "/music/lib-a/dup.mp3")
	scanID2 := insertScanResult(t, sqlDB, "/music/lib-b/dup.mp3")
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	// Two scan_results with identical normalized artist/title collapse into one
	// work_queue row. Both links must survive so Complete can flip both rows.
	first, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Dup"},
		ScanResultID: scanID1,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	second, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: " artist ", TrackName: "dup"},
		ScanResultID: scanID2,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected dedupe to single work_queue row; got ids %d and %d", first.ID, second.ID)
	}

	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, first.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for _, id := range []int64{scanID1, scanID2} {
		var status string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status FROM scan_results WHERE id = ?`, id,
		).Scan(&status); err != nil {
			t.Fatalf("read scan_results %d: %v", id, err)
		}
		if status != "done" {
			t.Fatalf("scan_results %d status = %q; want done (Complete must flip every linked row)", id, status)
		}
	}
}

func TestDBQueue_EnqueuePersistsScanResultID(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	scanID := insertScanResult(t, sqlDB, "/music/a.mp3")
	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	enq, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Title"},
		ScanResultID: scanID,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if enq.Inputs.ScanResultID != scanID {
		t.Fatalf("enqueued ScanResultID = %d; want %d", enq.Inputs.ScanResultID, scanID)
	}

	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if got.Inputs.ScanResultID != scanID {
		t.Fatalf("dequeued ScanResultID = %d; want %d", got.Inputs.ScanResultID, scanID)
	}
}

func TestDBQueue_EnqueuePreservesScanResultIDOnDuplicate(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	scanID := insertScanResult(t, sqlDB, "/music/a.mp3")
	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Title"},
		ScanResultID: scanID,
	}, 1); err != nil {
		t.Fatalf("Enqueue initial: %v", err)
	}
	// Webhook re-enqueue without an originating scan_result must not erase the link.
	dup, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Artist", TrackName: "Title"},
	}, 5)
	if err != nil {
		t.Fatalf("Enqueue duplicate: %v", err)
	}
	if dup.Inputs.ScanResultID != scanID {
		t.Fatalf("duplicate ScanResultID = %d; want %d preserved", dup.Inputs.ScanResultID, scanID)
	}
}

func TestDBQueue_CleanupRemovesRetryableDuplicate(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}
	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	removed, err := q.Cleanup(ctx, models.Inputs{Track: models.Track{ArtistName: " artist ", TrackName: "title"}})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d; want 1", removed)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue after cleanup error = %v; want sql.ErrNoRows", err)
	}

	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("Enqueue failed case: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue failed case: %v", err)
	}
	if _, err := q.Fail(ctx, item.ID, errors.New("retryable")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	removed, err = q.Cleanup(ctx, models.Inputs{Track: models.Track{ArtistName: " artist ", TrackName: "title"}})
	if err != nil {
		t.Fatalf("Cleanup failed row: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed failed rows = %d; want 1", removed)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue after failed cleanup error = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_CleanupPreservesProcessingAndDone(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}
	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	removed, err := q.Cleanup(ctx, inputs)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d; want 0 for processing item", removed)
	}

	if err := q.Complete(ctx, 1); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	removed, err = q.Cleanup(ctx, inputs)
	if err != nil {
		t.Fatalf("Cleanup done item: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d; want 0 for done item", removed)
	}
}

func TestDBQueue_CompleteRequiresProcessingStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Complete pending error = %v; want sql.ErrNoRows", err)
	}

	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete processing: %v", err)
	}
	if err := q.Complete(ctx, item.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Complete done error = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_DequeueSkipsBackoffUntilReady(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, item.ID, errors.New("temporary failure")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue during backoff error = %v; want sql.ErrNoRows", err)
	}

	q.now = func() time.Time { return now.Add(time.Minute) }
	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after backoff: %v", err)
	}
	if got.ID != item.ID {
		t.Fatalf("dequeued ID = %d; want %d", got.ID, item.ID)
	}
}

func TestDBQueue_FailUsesGeometricBackoff(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.baseBackoff = time.Second
	q.maxBackoff = time.Hour
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue first: %v", err)
	}
	failed, err := q.Fail(ctx, item.ID, errors.New("first"))
	if err != nil {
		t.Fatalf("Fail first: %v", err)
	}
	if failed.Attempts != 1 || !failed.NextAttemptAt.Equal(now.Add(time.Second)) {
		t.Fatalf("first failure attempts/next = %d/%s; want 1/%s", failed.Attempts, failed.NextAttemptAt, now.Add(time.Second))
	}

	q.now = func() time.Time { return now.Add(time.Second) }
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue second: %v", err)
	}
	failed, err = q.Fail(ctx, item.ID, errors.New("second"))
	if err != nil {
		t.Fatalf("Fail second: %v", err)
	}
	wantNext := now.Add(3 * time.Second)
	if failed.Attempts != 2 || !failed.NextAttemptAt.Equal(wantNext) {
		t.Fatalf("second failure attempts/next = %d/%s; want 2/%s", failed.Attempts, failed.NextAttemptAt, wantNext)
	}
}

func TestDBQueue_FailRequiresProcessingStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Fail(ctx, item.ID, errors.New("not claimed")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Fail pending error = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_ReleaseReturnsItemToPendingWithoutFailure(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	dequeued, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if dequeued.ID != item.ID {
		t.Fatalf("Dequeue id = %d; want %d", dequeued.ID, item.ID)
	}

	if err := q.Release(ctx, item.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	var status string
	var attempts int
	var nextAttempt string
	var lastError string
	if err := q.db.QueryRowContext(ctx,
		`SELECT status, attempts, next_attempt_at, last_error FROM work_queue WHERE id = ?`,
		item.ID,
	).Scan(&status, &attempts, &nextAttempt, &lastError); err != nil {
		t.Fatalf("query released row: %v", err)
	}
	if status != StatusPending {
		t.Fatalf("status = %q; want %q (release must restore pending)", status, StatusPending)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d; want 0 (release must not count as a failure)", attempts)
	}
	if lastError != "" {
		t.Fatalf("last_error = %q; want empty after release", lastError)
	}
	requeued, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after release: %v", err)
	}
	if requeued.ID != item.ID {
		t.Fatalf("re-dequeued id = %d; want %d (released item must be eligible again)", requeued.ID, item.ID)
	}
}

func TestDBQueue_ReleaseRequiresProcessingStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Release(ctx, item.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Release on pending row = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_ListFiltersByStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Pending"}}, 1); err != nil {
		t.Fatalf("Enqueue pending: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "B", TrackName: "Failing"}}, 1); err != nil {
		t.Fatalf("Enqueue failing: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, claimed.ID, errors.New("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	items, err := q.List(ctx, ListFilter{Status: StatusFailed})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 1 || items[0].ID != claimed.ID {
		t.Fatalf("List(failed) = %+v; want one row with id %d", items, claimed.ID)
	}

	items, err = q.List(ctx, ListFilter{Status: StatusPending})
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List(pending) returned %d; want 1", len(items))
	}

	items, err = q.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List no filter: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("List(no filter) = %d rows; want 2", len(items))
	}
}

func TestDBQueue_ListHonorsLimit(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	for i := 0; i < 5; i++ {
		if _, err := q.Enqueue(ctx, models.Inputs{
			Track: models.Track{ArtistName: "Artist", TrackName: fmt.Sprintf("Track%d", i)},
		}, 1); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	items, err := q.List(ctx, ListFilter{Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("List(limit=3) returned %d rows; want 3", len(items))
	}
}

func TestDBQueue_RetryResetsFailedRow(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	failed, err := q.Fail(ctx, claimed.ID, errors.New("rate limited"))
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if failed.Attempts != 1 || failed.LastError == "" {
		t.Fatalf("pre-retry attempts=%d last_error=%q; want attempts>0, non-empty error", failed.Attempts, failed.LastError)
	}

	q.now = func() time.Time { return now.Add(time.Hour) }
	retried, err := q.Retry(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.Status != StatusPending {
		t.Fatalf("retried status = %q; want pending", retried.Status)
	}
	if retried.Attempts != 0 {
		t.Fatalf("retried attempts = %d; want 0", retried.Attempts)
	}
	if retried.LastError != "" {
		t.Fatalf("retried last_error = %q; want empty", retried.LastError)
	}
	if !retried.NextAttemptAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("retried next_attempt_at = %s; want %s", retried.NextAttemptAt, now.Add(time.Hour))
	}
}

func TestDBQueue_RetryRejectsNonFailedStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	pending, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Pending"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Retry(ctx, pending.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("Retry pending error = %v; want ErrNotRetryable", err)
	}

	processing, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Retry(ctx, processing.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("Retry processing error = %v; want ErrNotRetryable", err)
	}

	if err := q.Complete(ctx, processing.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := q.Retry(ctx, processing.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("Retry done error = %v; want ErrNotRetryable", err)
	}

	if _, err := q.Retry(ctx, 9999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Retry missing id error = %v; want sql.ErrNoRows", err)
	}
}

func TestDBQueue_ClearDoneRemovesOnlyDoneRows(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	// pending (will stay)
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Pending"}}, 1); err != nil {
		t.Fatalf("Enqueue pending: %v", err)
	}
	// done
	doneItem, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "B", TrackName: "Done"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue done: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil { // claims pending first by FIFO; need to claim 'Done' instead
		t.Fatalf("Dequeue: %v", err)
	}
	// Above claimed the pending row. Claim again to get the done one.
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue 2: %v", err)
	}
	if err := q.Complete(ctx, doneItem.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	count, err := q.CountDone(ctx)
	if err != nil {
		t.Fatalf("CountDone: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountDone = %d; want 1", count)
	}

	deleted, err := q.ClearDone(ctx)
	if err != nil {
		t.Fatalf("ClearDone: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("ClearDone deleted = %d; want 1", deleted)
	}

	// The other (non-done) rows must still exist.
	items, err := q.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("ClearDone removed non-done rows; remaining = 0")
	}
	for _, it := range items {
		if it.Status == StatusDone {
			t.Fatalf("ClearDone left a done row: %+v", it)
		}
	}
}

func TestDBQueue_CompleteMarksDone(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var status string
	var completedAt string
	if err := q.db.QueryRowContext(ctx,
		`SELECT status, completed_at FROM work_queue WHERE id = ?`,
		item.ID,
	).Scan(&status, &completedAt); err != nil {
		t.Fatalf("query completed row: %v", err)
	}
	if status != StatusDone {
		t.Fatalf("status = %q; want %q", status, StatusDone)
	}
	if completedAt != formatTime(now) {
		t.Fatalf("completed_at = %q; want %q", completedAt, formatTime(now))
	}
}

func TestDBQueue_RetryResetsLinkedScanResults(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	scanID1 := insertScanResult(t, sqlDB, "/music/lib-a/song.mp3")
	scanID2 := insertScanResult(t, sqlDB, "/music/lib-b/song.mp3")

	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	// Two scan_results with the same normalized key collapse into one queue row.
	first, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Song"},
		ScanResultID: scanID1,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Song"},
		ScanResultID: scanID2,
	}, 1); err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, first.ID, errors.New("rate limited")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	// EnqueuePending in scan flips both scan_results to 'processing' on enqueue.
	// Verify that's the starting state we're testing the reset against.
	for _, id := range []int64{scanID1, scanID2} {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE scan_results SET status = 'processing' WHERE id = ?`, id,
		); err != nil {
			t.Fatalf("seed processing on scan %d: %v", id, err)
		}
	}

	if _, err := q.Retry(ctx, first.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	for _, id := range []int64{scanID1, scanID2} {
		var status string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status FROM scan_results WHERE id = ?`, id,
		).Scan(&status); err != nil {
			t.Fatalf("read scan_results %d: %v", id, err)
		}
		if status != StatusPending {
			t.Fatalf("scan_results %d status = %q; want %q (Retry must reset every linked processing row)", id, status, StatusPending)
		}
	}
}

func addLibrary(t *testing.T, sqlDB *sql.DB, name, path string) int64 {
	t.Helper()
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO libraries (path, name) VALUES (?, ?)`, path, name)
	if err != nil {
		t.Fatalf("insert library %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("library %q id: %v", name, err)
	}
	return id
}

func addScanResultIn(t *testing.T, sqlDB *sql.DB, libraryID int64, filePath, outdir, filename string) int64 {
	t.Helper()
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO scan_results (library_id, file_path, outdir, filename, status) VALUES (?, ?, ?, ?, 'pending')`,
		libraryID, filePath, outdir, filename)
	if err != nil {
		t.Fatalf("insert scan_result %s: %v", filePath, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id %s: %v", filePath, err)
	}
	return id
}

func linkScanResult(t *testing.T, sqlDB *sql.DB, workQueueID, scanResultID int64) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		workQueueID, scanResultID); err != nil {
		t.Fatalf("link work_queue %d -> scan_result %d: %v", workQueueID, scanResultID, err)
	}
}

func TestDBQueue_CancelByLibrary_DeletesSingleLibraryRow(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	srA := addScanResultIn(t, sqlDB, libA, "/music/a/1.mp3", "/music/a", "track.lrc")

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Solo"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "track.lrc"}},
		ScanResultID: srA,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	deleted, updated, err := q.CancelByLibrary(ctx, libA)
	if err != nil {
		t.Fatalf("CancelByLibrary: %v", err)
	}
	if deleted != 1 || updated != 0 {
		t.Fatalf("CancelByLibrary = (deleted=%d, updated=%d); want (1, 0)", deleted, updated)
	}

	var count int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE id = ?`, item.ID,
	).Scan(&count); err != nil {
		t.Fatalf("count work_queue: %v", err)
	}
	if count != 0 {
		t.Fatalf("work_queue row %d still present after CancelByLibrary", item.ID)
	}
}

func TestDBQueue_CancelByLibrary_UpdatesSharedRowAndLeavesOtherLibraryUntouched(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	libB := addLibrary(t, sqlDB, "B", "/music/b")

	// Shared row: same artist/title, present in both libraries.
	srA := addScanResultIn(t, sqlDB, libA, "/music/a/shared.mp3", "/music/a", "shared.lrc")
	srB := addScanResultIn(t, sqlDB, libB, "/music/b/shared.mp3", "/music/b", "shared.lrc")
	shared, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Both", TrackName: "Shared"},
		OutputPaths: []models.OutputPath{
			{Outdir: "/music/a", Filename: "shared.lrc"},
			{Outdir: "/music/b", Filename: "shared.lrc"},
		},
		ScanResultID: srA,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue shared: %v", err)
	}
	linkScanResult(t, sqlDB, shared.ID, srB)

	// Library Y only: must remain untouched.
	srBOnly := addScanResultIn(t, sqlDB, libB, "/music/b/only.mp3", "/music/b", "only.lrc")
	bOnly, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Just", TrackName: "B"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/b", Filename: "only.lrc"}},
		ScanResultID: srBOnly,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue bOnly: %v", err)
	}

	deleted, updated, err := q.CancelByLibrary(ctx, libA)
	if err != nil {
		t.Fatalf("CancelByLibrary: %v", err)
	}
	if deleted != 0 || updated != 1 {
		t.Fatalf("CancelByLibrary = (deleted=%d, updated=%d); want (0, 1)", deleted, updated)
	}

	// Shared row still present, output_paths shrunk to library B's entry only.
	var rawPaths string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT output_paths FROM work_queue WHERE id = ?`, shared.ID,
	).Scan(&rawPaths); err != nil {
		t.Fatalf("read shared output_paths: %v", err)
	}
	var paths []models.OutputPath
	if err := json.Unmarshal([]byte(rawPaths), &paths); err != nil {
		t.Fatalf("unmarshal shared output_paths %q: %v", rawPaths, err)
	}
	if len(paths) != 1 || paths[0].Outdir != "/music/b" || paths[0].Filename != "shared.lrc" {
		t.Fatalf("shared output_paths = %+v; want one /music/b entry", paths)
	}

	// Library B's standalone row is untouched.
	var bOnlyPaths string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT output_paths FROM work_queue WHERE id = ?`, bOnly.ID,
	).Scan(&bOnlyPaths); err != nil {
		t.Fatalf("read bOnly output_paths: %v", err)
	}
	if bOnlyPaths == "" {
		t.Fatalf("bOnly output_paths empty; want preserved")
	}
}

func TestDBQueue_CancelByLibrary_LeavesSharedRowWhenAllPathsBelongToOtherLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	libB := addLibrary(t, sqlDB, "B", "/music/b")

	// The row is a cancel candidate because it links to library A, but every
	// output_path it carries also belongs to library B's scan_result. The
	// keep-set therefore retains all paths and the row needs no change: this
	// exercises the defensive "filtered == paths" branch in cancelByLibrary.
	srA := addScanResultIn(t, sqlDB, libA, "/music/a/shared.mp3", "/music/a", "shared.lrc")
	srB := addScanResultIn(t, sqlDB, libB, "/music/b/shared.mp3", "/music/b", "shared.lrc")
	row, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Both", TrackName: "Shared"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/b", Filename: "shared.lrc"}},
		ScanResultID: srA,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	linkScanResult(t, sqlDB, row.ID, srB)

	deleted, updated, err := q.CancelByLibrary(ctx, libA)
	if err != nil {
		t.Fatalf("CancelByLibrary: %v", err)
	}
	if deleted != 0 || updated != 0 {
		t.Fatalf("CancelByLibrary = (deleted=%d, updated=%d); want (0, 0) when no paths change", deleted, updated)
	}

	// Row preserved with its single library-B path intact.
	var rawPaths string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT output_paths FROM work_queue WHERE id = ?`, row.ID,
	).Scan(&rawPaths); err != nil {
		t.Fatalf("read output_paths: %v", err)
	}
	var paths []models.OutputPath
	if err := json.Unmarshal([]byte(rawPaths), &paths); err != nil {
		t.Fatalf("unmarshal output_paths %q: %v", rawPaths, err)
	}
	if len(paths) != 1 || paths[0].Outdir != "/music/b" || paths[0].Filename != "shared.lrc" {
		t.Fatalf("output_paths = %+v; want one /music/b entry unchanged", paths)
	}
}

func TestDBQueue_CancelByLibrary_ErrorsOnCorruptOutputPaths(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	srA := addScanResultIn(t, sqlDB, libA, "/music/a/x.mp3", "/music/a", "x.lrc")
	row, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "X"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "x.lrc"}},
		ScanResultID: srA,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Corrupt the persisted output_paths so cancelByLibrary's json.Unmarshal of
	// the candidate row fails and the error is surfaced rather than swallowed.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET output_paths = ? WHERE id = ?`, "{not-valid-json", row.ID); err != nil {
		t.Fatalf("corrupt output_paths: %v", err)
	}
	if _, _, err := q.CancelByLibrary(ctx, libA); err == nil {
		t.Fatalf("CancelByLibrary: want error on corrupt output_paths, got nil")
	}
}

func TestDBQueue_CancelByLibrary_ErrorsWhenDBClosed(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := q.CancelByLibrary(ctx, 1); err == nil {
		t.Fatalf("CancelByLibrary on closed DB: want error, got nil")
	}
}

func TestDBQueue_CountCancelByLibrary_ErrorsWhenDBClosed(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := q.CountCancelByLibrary(ctx, 1); err == nil {
		t.Fatalf("CountCancelByLibrary on closed DB: want error, got nil")
	}
}

func TestDBQueue_CancelByLibrary_ErrorsWhenJunctionMissing(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	libA := addLibrary(t, sqlDB, "A", "/music/a")
	// Drop the junction table so the candidate-selection query fails inside
	// the cancel transaction; the error must propagate to the caller rather
	// than be swallowed.
	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE work_queue_scan_results`); err != nil {
		t.Fatalf("drop junction table: %v", err)
	}
	if _, _, err := q.CancelByLibrary(ctx, libA); err == nil {
		t.Fatalf("CancelByLibrary with missing junction table: want error, got nil")
	}
}

func TestDBQueue_CancelByLibrary_LeavesProcessingAndDoneRowsUntouched(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	srProc := addScanResultIn(t, sqlDB, libA, "/music/a/proc.mp3", "/music/a", "proc.lrc")
	srDone := addScanResultIn(t, sqlDB, libA, "/music/a/done.mp3", "/music/a", "done.lrc")

	proc, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Processing"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "proc.lrc"}},
		ScanResultID: srProc,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue proc: %v", err)
	}
	done, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Done"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "done.lrc"}},
		ScanResultID: srDone,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue done: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET status = 'processing' WHERE id = ?`, proc.ID); err != nil {
		t.Fatalf("force processing: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET status = 'done' WHERE id = ?`, done.ID); err != nil {
		t.Fatalf("force done: %v", err)
	}

	deleted, updated, err := q.CancelByLibrary(ctx, libA)
	if err != nil {
		t.Fatalf("CancelByLibrary: %v", err)
	}
	if deleted != 0 || updated != 0 {
		t.Fatalf("CancelByLibrary = (deleted=%d, updated=%d); want (0, 0) for processing+done", deleted, updated)
	}

	for _, id := range []int64{proc.ID, done.ID} {
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_queue WHERE id = ?`, id,
		).Scan(&count); err != nil {
			t.Fatalf("count work_queue %d: %v", id, err)
		}
		if count != 1 {
			t.Fatalf("work_queue %d count = %d; want 1 (processing/done must not be deleted)", id, count)
		}
	}
}

func TestDBQueue_CountCancelByLibrary_ProjectsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	libB := addLibrary(t, sqlDB, "B", "/music/b")

	srSolo := addScanResultIn(t, sqlDB, libA, "/music/a/1.mp3", "/music/a", "1.lrc")
	solo, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Solo", TrackName: "A"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "1.lrc"}},
		ScanResultID: srSolo,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue solo: %v", err)
	}

	srA2 := addScanResultIn(t, sqlDB, libA, "/music/a/2.mp3", "/music/a", "2.lrc")
	srB2 := addScanResultIn(t, sqlDB, libB, "/music/b/2.mp3", "/music/b", "2.lrc")
	shared, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Shared", TrackName: "B"},
		OutputPaths: []models.OutputPath{
			{Outdir: "/music/a", Filename: "2.lrc"},
			{Outdir: "/music/b", Filename: "2.lrc"},
		},
		ScanResultID: srA2,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue shared: %v", err)
	}
	linkScanResult(t, sqlDB, shared.ID, srB2)

	deleted, updated, err := q.CountCancelByLibrary(ctx, libA)
	if err != nil {
		t.Fatalf("CountCancelByLibrary: %v", err)
	}
	if deleted != 1 || updated != 1 {
		t.Fatalf("CountCancelByLibrary = (deleted=%d, updated=%d); want (1, 1)", deleted, updated)
	}

	// Both rows must still exist with original output_paths unchanged.
	for _, id := range []int64{solo.ID, shared.ID} {
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_queue WHERE id = ?`, id,
		).Scan(&count); err != nil {
			t.Fatalf("count work_queue %d: %v", id, err)
		}
		if count != 1 {
			t.Fatalf("work_queue %d count = %d; want 1 (dry-run must not delete)", id, count)
		}
	}
	var sharedPaths string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT output_paths FROM work_queue WHERE id = ?`, shared.ID,
	).Scan(&sharedPaths); err != nil {
		t.Fatalf("read shared output_paths: %v", err)
	}
	var paths []models.OutputPath
	if err := json.Unmarshal([]byte(sharedPaths), &paths); err != nil {
		t.Fatalf("unmarshal shared output_paths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("shared output_paths after dry-run = %+v; want 2 (unchanged)", paths)
	}
}

func TestDBQueue_CancelByLibraryTx_RunsInExternalTransaction(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA := addLibrary(t, sqlDB, "A", "/music/a")
	srA := addScanResultIn(t, sqlDB, libA, "/music/a/1.mp3", "/music/a", "track.lrc")

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Solo"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music/a", Filename: "track.lrc"}},
		ScanResultID: srA,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	deleted, updated, err := q.CancelByLibraryTx(ctx, tx, libA)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("CancelByLibraryTx: %v", err)
	}
	if deleted != 1 || updated != 0 {
		_ = tx.Rollback()
		t.Fatalf("CancelByLibraryTx = (deleted=%d, updated=%d); want (1, 0)", deleted, updated)
	}

	// Before commit, the row must still be visible on a separate read because
	// the change is uncommitted. Verify rollback restores it.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var preCommit int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE id = ?`, item.ID,
	).Scan(&preCommit); err != nil {
		t.Fatalf("post-rollback count: %v", err)
	}
	if preCommit != 1 {
		t.Fatalf("work_queue row %d count after rollback = %d; want 1", item.ID, preCommit)
	}

	// Now commit a second invocation and verify the row is gone.
	tx2, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 2: %v", err)
	}
	if _, _, err := q.CancelByLibraryTx(ctx, tx2, libA); err != nil {
		_ = tx2.Rollback()
		t.Fatalf("CancelByLibraryTx 2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var postCommit int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE id = ?`, item.ID,
	).Scan(&postCommit); err != nil {
		t.Fatalf("post-commit count: %v", err)
	}
	if postCommit != 0 {
		t.Fatalf("work_queue row %d count after commit = %d; want 0", item.ID, postCommit)
	}
}
