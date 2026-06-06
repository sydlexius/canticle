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

func TestDBQueue_NoResultRequeueIsDeferredButReprocessable(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	// The worker requeues a no-result via Defer (fixed cooldown). It must NOT be
	// terminal: the row stays re-dequeueable once the cooldown window elapses, so
	// the track is re-attempted later as the catalog grows.
	const cooldown = 7 * 24 * time.Hour
	deferred, err := q.Defer(ctx, claimed.ID, cooldown, errors.New("musixmatch: no results found"))
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if deferred.Status != StatusDeferred {
		t.Fatalf("status = %q; want %q (deferred, not terminal)", deferred.Status, StatusDeferred)
	}
	if deferred.MissCount != 1 {
		t.Fatalf("miss_count = %d; want 1 (Defer must increment miss_count)", deferred.MissCount)
	}

	// The cooldown is fixed: next_attempt_at is exactly now+cooldown and attempts
	// is unchanged (Defer does not ramp like geometric backoff).
	if want := now.Add(cooldown); !deferred.NextAttemptAt.Equal(want) {
		t.Fatalf("next_attempt_at = %v; want fixed %v", deferred.NextAttemptAt, want)
	}
	if deferred.Attempts != claimed.Attempts {
		t.Fatalf("attempts = %d; want unchanged %d (a fixed cooldown must not ramp)", deferred.Attempts, claimed.Attempts)
	}

	// Deferred: not eligible immediately.
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("immediate Dequeue = %v; want sql.ErrNoRows (item is deferred by cooldown)", err)
	}

	// The cooldown survives a later library scan: re-enqueuing at scan priority
	// must preserve next_attempt_at (the row stays 'failed', not reset to now),
	// so the track is not re-queried upstream on every scan.
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue (rescan): %v", err)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue after rescan = %v; want sql.ErrNoRows (cooldown must survive a scan)", err)
	}

	// Re-processable: eligible again once the cooldown elapses.
	q.now = func() time.Time { return deferred.NextAttemptAt.Add(time.Second) }
	again, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after cooldown: %v", err)
	}
	if again.ID != claimed.ID {
		t.Fatalf("re-dequeued ID = %d; want %d (a no-result must remain re-processable)", again.ID, claimed.ID)
	}
}

// TestDBQueue_WebhookEnqueueResetsDeferredCooldown proves the asymmetry
// documented on Defer: a webhook-priority Enqueue of a deferred (cooldown)
// row resets next_attempt_at to now so an explicit webhook forces an immediate
// re-check, while a scan-priority Enqueue preserves the cooldown.
func TestDBQueue_WebhookEnqueueResetsDeferredCooldown(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	// Defer the row into a future cooldown window.
	const cooldown = 7 * 24 * time.Hour
	deferred, err := q.Defer(ctx, claimed.ID, cooldown, errors.New("musixmatch: no results found"))
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if deferred.Status != StatusDeferred {
		t.Fatalf("status = %q; want %q", deferred.Status, StatusDeferred)
	}

	// Control: a scan-priority Enqueue must NOT reset the cooldown.
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue (scan): %v", err)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue after scan Enqueue = %v; want sql.ErrNoRows (scan must preserve cooldown)", err)
	}

	// A webhook-priority Enqueue resets next_attempt_at to (approximately) now,
	// so the row becomes immediately dequeueable despite the cooldown.
	refreshed, err := q.Enqueue(ctx, inputs, PriorityWebhook)
	if err != nil {
		t.Fatalf("Enqueue (webhook): %v", err)
	}
	if !refreshed.NextAttemptAt.Equal(now) {
		t.Fatalf("next_attempt_at = %v; want reset to now %v (webhook must force a re-check)", refreshed.NextAttemptAt, now)
	}
	again, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after webhook Enqueue: %v", err)
	}
	if again.ID != claimed.ID {
		t.Fatalf("re-dequeued ID = %d; want %d (webhook reset must make the same row eligible)", again.ID, claimed.ID)
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

	// Deferred rows must also be rejected; they are not reset via Retry.
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "X", TrackName: "Deferred"}}, 1); err != nil {
		t.Fatalf("Enqueue deferred candidate: %v", err)
	}
	ditem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue deferred candidate: %v", err)
	}
	if _, err := q.Defer(ctx, ditem.ID, 7*24*time.Hour, errors.New("no results")); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if _, err := q.Retry(ctx, ditem.ID); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("Retry deferred error = %v; want ErrNotRetryable", err)
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

// TestDBQueue_DeferSetsDeferredStatus verifies that Defer transitions a
// processing row to StatusDeferred (not StatusFailed) and increments miss_count.
func TestDBQueue_DeferSetsDeferredStatus(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if item.MissCount != 0 {
		t.Fatalf("initial miss_count = %d; want 0", item.MissCount)
	}

	deferred, err := q.Defer(ctx, item.ID, 7*24*time.Hour, errors.New("no results"))
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if deferred.Status != StatusDeferred {
		t.Fatalf("status = %q; want %q", deferred.Status, StatusDeferred)
	}
	if deferred.MissCount != 1 {
		t.Fatalf("miss_count = %d; want 1 after first Defer", deferred.MissCount)
	}
	if deferred.Attempts != item.Attempts {
		t.Fatalf("attempts = %d; want unchanged %d", deferred.Attempts, item.Attempts)
	}

	// Second Defer: re-enqueue and defer again; miss_count increments further.
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityWebhook); err != nil {
		t.Fatalf("Enqueue (force re-check): %v", err)
	}
	item2, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue 2: %v", err)
	}
	deferred2, err := q.Defer(ctx, item2.ID, 7*24*time.Hour, errors.New("still no results"))
	if err != nil {
		t.Fatalf("Defer 2: %v", err)
	}
	if deferred2.MissCount != 2 {
		t.Fatalf("miss_count = %d; want 2 after second Defer", deferred2.MissCount)
	}
}

// TestDBQueue_DeferredRowsArePickedUpByDequeue verifies that a deferred row
// becomes eligible for Dequeue once its next_attempt_at window elapses.
func TestDBQueue_DeferredRowsArePickedUpByDequeue(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	const cooldown = 7 * 24 * time.Hour
	if _, err := q.Defer(ctx, item.ID, cooldown, errors.New("no results")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	// Not yet eligible.
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue before cooldown = %v; want sql.ErrNoRows", err)
	}

	// Advance time past the cooldown.
	q.now = func() time.Time { return now.Add(cooldown + time.Second) }
	again, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after cooldown: %v", err)
	}
	if again.ID != item.ID {
		t.Fatalf("re-dequeued ID = %d; want %d", again.ID, item.ID)
	}
}

// TestDBQueue_CountByStatusIncludesDeferred verifies that CountByStatus
// reports a non-zero deferred bucket once a row has been deferred.
func TestDBQueue_CountByStatusIncludesDeferred(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "B"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, 7*24*time.Hour, errors.New("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	counts, err := q.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[StatusDeferred] != 1 {
		t.Fatalf("deferred count = %d; want 1. counts = %v", counts[StatusDeferred], counts)
	}
	if counts[StatusFailed] != 0 {
		t.Fatalf("failed count = %d; want 0 (deferred must not appear as failed)", counts[StatusFailed])
	}
}

// TestDBQueue_DeferSetsPriorityMiss verifies that Defer sets the row priority
// to PriorityMiss so deferred re-attempts sink below all fresh work.
func TestDBQueue_DeferSetsPriorityMiss(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	deferred, err := q.Defer(ctx, item.ID, 24*time.Hour, errors.New("miss"))
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if deferred.Priority != PriorityMiss {
		t.Fatalf("priority = %d; want PriorityMiss (%d)", deferred.Priority, PriorityMiss)
	}
}

// TestDBQueue_EnqueueScanPreservesMissPriority confirms that a scan-priority
// re-enqueue does NOT un-deprioritize a deferred miss row.
func TestDBQueue_EnqueueScanPreservesMissPriority(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, 24*time.Hour, errors.New("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	// Scan-priority re-enqueue must preserve PriorityMiss.
	refreshed, err := q.Enqueue(ctx, inputs, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue (scan): %v", err)
	}
	if refreshed.Priority != PriorityMiss {
		t.Fatalf("priority after scan Enqueue = %d; want PriorityMiss (%d) (scan must not un-deprioritize)", refreshed.Priority, PriorityMiss)
	}
}

// TestDBQueue_EnqueueWebhookRestoresPriority confirms that a webhook-priority
// re-enqueue overrides PriorityMiss (the escape hatch).
func TestDBQueue_EnqueueWebhookRestoresPriority(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, 24*time.Hour, errors.New("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	// Webhook-priority enqueue must override PriorityMiss.
	refreshed, err := q.Enqueue(ctx, inputs, PriorityWebhook)
	if err != nil {
		t.Fatalf("Enqueue (webhook): %v", err)
	}
	if refreshed.Priority != PriorityWebhook {
		t.Fatalf("priority after webhook Enqueue = %d; want PriorityWebhook (%d)", refreshed.Priority, PriorityWebhook)
	}
}

// TestDBQueue_DequeuePicksScanOverMissRow verifies that a ready PriorityScan
// item is dequeued before a ready PriorityMiss item.
func TestDBQueue_DequeuePicksScanOverMissRow(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	// Enqueue and defer a miss item first (so it has a lower ID and would win
	// on created_at if priority were equal).
	missInputs := models.Inputs{Track: models.Track{ArtistName: "Miss", TrackName: "Song"}}
	if _, err := q.Enqueue(ctx, missInputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue miss: %v", err)
	}
	missItem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue miss: %v", err)
	}
	// Defer sets PriorityMiss; advance clock so cooldown is in the past.
	if _, err := q.Defer(ctx, missItem.ID, time.Millisecond, errors.New("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	// Advance clock so the deferred row is ready.
	q.now = func() time.Time { return now.Add(time.Second) }

	// Enqueue a fresh scan-priority item (higher ID, but higher priority).
	freshInputs := models.Inputs{Track: models.Track{ArtistName: "Fresh", TrackName: "Song"}}
	if _, err := q.Enqueue(ctx, freshInputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue fresh: %v", err)
	}

	// The scan-priority item must be dequeued first despite having a higher ID.
	first, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("first Dequeue: %v", err)
	}
	if first.Inputs.Track.ArtistName != "Fresh" {
		t.Fatalf("first dequeued = %q; want Fresh (scan-priority must win over PriorityMiss)", first.Inputs.Track.ArtistName)
	}

	// The miss item is dequeued second.
	second, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("second Dequeue: %v", err)
	}
	if second.Inputs.Track.ArtistName != "Miss" {
		t.Fatalf("second dequeued = %q; want Miss", second.Inputs.Track.ArtistName)
	}
}

// TestDBQueue_RetireMiss verifies that RetireMiss sets status=done with the
// sentinel error message. When no scan_result_id is linked the call must
// succeed without error (empty junction is a no-op on scan_results).
func TestDBQueue_RetireMiss(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	retired, err := q.RetireMiss(ctx, item.ID)
	if err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}
	if retired.Status != StatusDone {
		t.Fatalf("status = %q; want %q", retired.Status, StatusDone)
	}
	if retired.LastError != missLimitReachedError {
		t.Fatalf("last_error = %q; want %q", retired.LastError, missLimitReachedError)
	}
	if retired.CompletedAt == nil {
		t.Fatal("completed_at = nil; want non-nil")
	}
	// No scan_result_id on this row; the scan_results UPDATE is a no-op but must
	// not error. The linked-row writeback is covered by
	// TestDBQueue_RetireMissWritesScanResultsDone.
}

// TestDBQueue_RetireMissWritesScanResultsDone verifies that RetireMiss
// writes status='done' to every linked scan_results row (mirroring Complete's
// writeback) so the scan layer does not strand the track in 'processing'.
func TestDBQueue_RetireMissWritesScanResultsDone(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }

	// Insert a library and scan_result row to link.
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES ('/music', 'test') RETURNING id`,
	).Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	var srID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
         VALUES (?, 'Artist', 'Title', '/music/song.flac', 'out', 'song.lrc', 'pending')
         RETURNING id`,
		libID,
	).Scan(&srID); err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}

	inputs := models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:       "out",
		Filename:     "song.lrc",
		ScanResultID: srID,
	}
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	if _, err := q.RetireMiss(ctx, item.ID); err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}

	// scan_results must be 'done' so the track is not stranded as 'processing'.
	var srStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, srID).Scan(&srStatus); err != nil {
		t.Fatalf("scan scan_result status: %v", err)
	}
	if srStatus != "done" {
		t.Fatalf("scan_result status = %q; want %q (RetireMiss must write back done to unblock the scan layer)", srStatus, "done")
	}
}

// TestPriorityConstantsSQLParity is a compile-time guard asserting that
// PriorityMiss and PriorityWebhook match the SQL literals hardcoded in Defer
// (priority = -100) and Enqueue (priority >= 10). The SQL driver cannot bind Go
// constants, so a future rename or value change here would silently desync with
// those queries; this test makes such a drift a build failure instead.
func TestPriorityConstantsSQLParity(t *testing.T) {
	if PriorityMiss != -100 {
		t.Fatalf("PriorityMiss = %d; SQL literal in Defer is -100 -- update the SQL or this constant", PriorityMiss)
	}
	if PriorityWebhook != 10 {
		t.Fatalf("PriorityWebhook = %d; SQL literal in Enqueue is 10 -- update the SQL or this constant", PriorityWebhook)
	}
}

// TestDBQueue_RetireMissNoRowsWhenNotProcessing verifies that RetireMiss
// returns sql.ErrNoRows when the row is not in processing status.
func TestDBQueue_RetireMissNoRowsWhenNotProcessing(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	// Complete the item first (now it's done, not processing).
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// RetireMiss on a non-processing row must return sql.ErrNoRows.
	_, err = q.RetireMiss(ctx, item.ID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RetireMiss on non-processing row = %v; want sql.ErrNoRows", err)
	}
}

// insertLibraryAndScanResult inserts a library row and a scan_results row linked
// to it. Returns the library ID and scan_result ID.
func insertLibraryAndScanResult(t *testing.T, sqlDB *sql.DB, libPath, filePath string) (libID int64, srID int64) {
	t.Helper()
	ctx := context.Background()
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`, libPath, libPath,
	).Scan(&libID); err != nil {
		t.Fatalf("insert library %s: %v", libPath, err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
         VALUES (?, 'Artist', 'Title', ?, 'out', 'song.lrc', 'done')
         RETURNING id`,
		libID, filePath,
	).Scan(&srID); err != nil {
		t.Fatalf("insert scan_result %s: %v", filePath, err)
	}
	return libID, srID
}

// makeRetiredRow enqueues an item linked to srID, dequeues it, and calls
// RetireMiss so the work_queue row lands in status='done' with the sentinel
// last_error. Returns the work_queue ID.
func makeRetiredRow(t *testing.T, ctx context.Context, q *DBQueue, srID int64, artistSuffix string) int64 {
	t.Helper()
	inputs := models.Inputs{
		Track:        models.Track{ArtistName: "Artist" + artistSuffix, TrackName: "Title" + artistSuffix},
		Outdir:       "out",
		Filename:     "song.lrc",
		ScanResultID: srID,
	}
	if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.RetireMiss(ctx, item.ID); err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}
	return item.ID
}

// TestDBQueue_RecheckDeferred verifies that RecheckDeferred resets
// next_attempt_at to now for deferred rows and leaves status/priority/miss_count
// and providers_version unchanged. Non-deferred rows are not touched.
func TestDBQueue_RecheckDeferred(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	// Enqueue, dequeue, defer with a far-future cooldown.
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Artist", TrackName: "Title"},
	}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	deferred, err := q.Defer(ctx, claimed.ID, 7*24*time.Hour, fmt.Errorf("no results"))
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if deferred.Status != StatusDeferred {
		t.Fatalf("status = %q; want deferred", deferred.Status)
	}
	savedMissCount := deferred.MissCount
	savedPriority := deferred.Priority
	savedProvidersVersion := deferred.ProvidersVersion

	// Also enqueue a non-deferred pending row to verify it is NOT touched.
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Other", TrackName: "Title"},
	}, PriorityScan); err != nil {
		t.Fatalf("Enqueue other: %v", err)
	}

	// Advance clock to confirm the revived next_attempt_at == new now.
	later := now.Add(time.Hour)
	q.now = func() time.Time { return later }

	n, err := q.RecheckDeferred(ctx, nil)
	if err != nil {
		t.Fatalf("RecheckDeferred: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecheckDeferred count = %d; want 1", n)
	}

	// Verify the deferred row's next_attempt_at was reset; other fields unchanged.
	var status string
	var priority, missCount, providersVersion int
	var nextAttemptAt string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, priority, miss_count, providers_version, next_attempt_at
         FROM work_queue WHERE id = ?`, deferred.ID,
	).Scan(&status, &priority, &missCount, &providersVersion, &nextAttemptAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != StatusDeferred {
		t.Fatalf("status = %q; want deferred (RecheckDeferred must not change status)", status)
	}
	if priority != savedPriority {
		t.Fatalf("priority = %d; want %d (unchanged)", priority, savedPriority)
	}
	if missCount != savedMissCount {
		t.Fatalf("miss_count = %d; want %d (unchanged)", missCount, savedMissCount)
	}
	if providersVersion != savedProvidersVersion {
		t.Fatalf("providers_version = %d; want %d (unchanged)", providersVersion, savedProvidersVersion)
	}
	wantTime := formatTime(later)
	if nextAttemptAt != wantTime {
		t.Fatalf("next_attempt_at = %q; want %q (reset to now)", nextAttemptAt, wantTime)
	}

	// The pending row must be untouched -- still status='pending' with a
	// next_attempt_at of the original 'now', not 'later'.
	var pendingStatus string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM work_queue WHERE artist = 'Other'`,
	).Scan(&pendingStatus); err != nil {
		t.Fatalf("read pending row: %v", err)
	}
	if pendingStatus != StatusPending {
		t.Fatalf("pending row status = %q; want pending", pendingStatus)
	}
}

// TestDBQueue_CountRecheckDeferred verifies that CountRecheckDeferred matches
// the rows affected by RecheckDeferred.
func TestDBQueue_CountRecheckDeferred(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	// No deferred rows yet.
	count, err := q.CountRecheckDeferred(ctx, nil)
	if err != nil {
		t.Fatalf("CountRecheckDeferred empty: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d; want 0", count)
	}

	// Add two deferred rows and confirm count = 2.
	for _, name := range []string{"Alpha", "Beta"} {
		if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: name, TrackName: "Song"}}, PriorityScan); err != nil {
			t.Fatalf("Enqueue %s: %v", name, err)
		}
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("Dequeue %s: %v", name, err)
		}
		if _, err := q.Defer(ctx, item.ID, time.Hour, fmt.Errorf("miss")); err != nil {
			t.Fatalf("Defer %s: %v", name, err)
		}
	}

	count, err = q.CountRecheckDeferred(ctx, nil)
	if err != nil {
		t.Fatalf("CountRecheckDeferred: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d; want 2", count)
	}

	n, err := q.RecheckDeferred(ctx, nil)
	if err != nil {
		t.Fatalf("RecheckDeferred: %v", err)
	}
	if n != count {
		t.Fatalf("RecheckDeferred = %d; CountRecheckDeferred = %d; must agree", n, count)
	}
}

// TestDBQueue_RecheckRetired verifies that RecheckRetired revives
// status='done'+sentinel rows to status='deferred' and resets their linked
// scan_results from 'done' to 'pending'. Non-retired done rows must be
// untouched. miss_count and providers_version must not change.
func TestDBQueue_RecheckRetired(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	_, srID := insertLibraryAndScanResult(t, sqlDB, "/music", "/music/song.flac")
	wqID := makeRetiredRow(t, ctx, q, srID, "")

	// A 'done' row with a DIFFERENT last_error must NOT be revived.
	var otherWqID int64
	{
		if _, err := q.Enqueue(ctx, models.Inputs{
			Track: models.Track{ArtistName: "Other", TrackName: "Done"},
		}, PriorityScan); err != nil {
			t.Fatalf("Enqueue other: %v", err)
		}
		other, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("Dequeue other: %v", err)
		}
		if err := q.Complete(ctx, other.ID); err != nil {
			t.Fatalf("Complete other: %v", err)
		}
		otherWqID = other.ID
	}

	// Capture pre-revival miss_count and providers_version.
	var preMissCount, preProvidersVersion int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT miss_count, providers_version FROM work_queue WHERE id = ?`, wqID,
	).Scan(&preMissCount, &preProvidersVersion); err != nil {
		t.Fatalf("read pre-revival: %v", err)
	}

	n, err := q.RecheckRetired(ctx, nil)
	if err != nil {
		t.Fatalf("RecheckRetired: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecheckRetired count = %d; want 1", n)
	}

	// Retired row: must be 'deferred', priority=-100, last_error='', completed_at=NULL.
	var status, lastError string
	var priority, missCount, providersVersion int
	var completedAt sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, priority, last_error, miss_count, providers_version, completed_at
         FROM work_queue WHERE id = ?`, wqID,
	).Scan(&status, &priority, &lastError, &missCount, &providersVersion, &completedAt); err != nil {
		t.Fatalf("read revived row: %v", err)
	}
	if status != StatusDeferred {
		t.Fatalf("status = %q; want deferred", status)
	}
	if priority != PriorityMiss {
		t.Fatalf("priority = %d; want %d (PriorityMiss)", priority, PriorityMiss)
	}
	if lastError != "" {
		t.Fatalf("last_error = %q; want ''", lastError)
	}
	if completedAt.Valid {
		t.Fatalf("completed_at = %q; want NULL", completedAt.String)
	}
	if missCount != preMissCount {
		t.Fatalf("miss_count = %d; want %d (unchanged)", missCount, preMissCount)
	}
	if providersVersion != preProvidersVersion {
		t.Fatalf("providers_version = %d; want %d (unchanged)", providersVersion, preProvidersVersion)
	}

	// Linked scan_result must be 'pending'.
	var srStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, srID).Scan(&srStatus); err != nil {
		t.Fatalf("read scan_result: %v", err)
	}
	if srStatus != "pending" {
		t.Fatalf("scan_result status = %q; want pending (RecheckRetired must revive the scan layer)", srStatus)
	}

	// Non-retired done row must be untouched.
	var otherStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, otherWqID).Scan(&otherStatus); err != nil {
		t.Fatalf("read other row: %v", err)
	}
	if otherStatus != StatusDone {
		t.Fatalf("non-retired row status = %q; want done (must not be touched)", otherStatus)
	}
}

// TestDBQueue_CountRecheckRetired verifies that CountRecheckRetired matches the
// rows affected by RecheckRetired.
func TestDBQueue_CountRecheckRetired(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	// No retired rows yet.
	count, err := q.CountRecheckRetired(ctx, nil)
	if err != nil {
		t.Fatalf("CountRecheckRetired empty: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d; want 0", count)
	}

	_, srID1 := insertLibraryAndScanResult(t, sqlDB, "/lib1", "/lib1/a.mp3")
	_, srID2 := insertLibraryAndScanResult(t, sqlDB, "/lib2", "/lib2/b.mp3")
	makeRetiredRow(t, ctx, q, srID1, "A")
	makeRetiredRow(t, ctx, q, srID2, "B")

	count, err = q.CountRecheckRetired(ctx, nil)
	if err != nil {
		t.Fatalf("CountRecheckRetired: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d; want 2", count)
	}

	n, err := q.RecheckRetired(ctx, nil)
	if err != nil {
		t.Fatalf("RecheckRetired: %v", err)
	}
	if n != count {
		t.Fatalf("RecheckRetired = %d; CountRecheckRetired = %d; must agree", n, count)
	}
}

// TestDBQueue_RecheckLibraryScoping verifies that passing a non-nil libraryID
// scopes the recheck to only rows linked to that library. Rows in a different
// library must not be touched.
func TestDBQueue_RecheckLibraryScoping(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	libID1, srID1 := insertLibraryAndScanResult(t, sqlDB, "/libA", "/libA/a.mp3")
	_, srID2 := insertLibraryAndScanResult(t, sqlDB, "/libB", "/libB/b.mp3")

	wqID1 := makeRetiredRow(t, ctx, q, srID1, "X")
	wqID2 := makeRetiredRow(t, ctx, q, srID2, "Y")

	// Recheck only libID1.
	n, err := q.RecheckRetired(ctx, &libID1)
	if err != nil {
		t.Fatalf("RecheckRetired scoped: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecheckRetired scoped = %d; want 1 (only libA)", n)
	}

	// wqID1 must be revived; wqID2 must remain 'done' with sentinel.
	var s1, s2 string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, wqID1).Scan(&s1); err != nil {
		t.Fatalf("read wqID1: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, wqID2).Scan(&s2); err != nil {
		t.Fatalf("read wqID2: %v", err)
	}
	if s1 != StatusDeferred {
		t.Fatalf("wqID1 status = %q; want deferred (in target library)", s1)
	}
	if s2 != StatusDone {
		t.Fatalf("wqID2 status = %q; want done (different library, must be untouched)", s2)
	}

	// scan_result for srID2 must also remain 'done'.
	var srStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, srID2).Scan(&srStatus); err != nil {
		t.Fatalf("read srID2: %v", err)
	}
	if srStatus != "done" {
		t.Fatalf("srID2 status = %q; want done (different library)", srStatus)
	}

	// RecheckDeferred scoping: use fresh libraries and scan_results so the
	// deferred rows are not confused with the revived wqID1 (which is already
	// status='deferred' after RecheckRetired). Fresh libraries ensure each
	// deferred row belongs to exactly one library.
	libIDC, srIDC := insertLibraryAndScanResult(t, sqlDB, "/libC", "/libC/c.mp3")
	_, srIDD := insertLibraryAndScanResult(t, sqlDB, "/libD", "/libD/d.mp3")
	for i, srID := range []int64{srIDC, srIDD} {
		if _, err := q.Enqueue(ctx, models.Inputs{
			Track:        models.Track{ArtistName: fmt.Sprintf("Def%d", i), TrackName: "ScopedSong"},
			ScanResultID: srID,
		}, PriorityScan); err != nil {
			t.Fatalf("Enqueue deferred %d: %v", i, err)
		}
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("Dequeue deferred %d: %v", i, err)
		}
		if _, err := q.Defer(ctx, item.ID, time.Hour, fmt.Errorf("miss")); err != nil {
			t.Fatalf("Defer %d: %v", i, err)
		}
	}

	nDeferred, err := q.RecheckDeferred(ctx, &libIDC)
	if err != nil {
		t.Fatalf("RecheckDeferred scoped: %v", err)
	}
	if nDeferred != 1 {
		t.Fatalf("RecheckDeferred scoped = %d; want 1 (only libC)", nDeferred)
	}
}

// TestDBQueue_RecheckRetired_SharedRow verifies that a deduped work_queue row
// linked to scan_results in two libraries is revived under --library X without
// resetting the OTHER library's scan_result. Regression test for the
// cross-library writeback leak: reviving the shared row is correct (one fetch
// serves every linked library), but the scan_results writeback must stay scoped
// to the target library so the non-target library's terminal state is preserved.
func TestDBQueue_RecheckRetired_SharedRow(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	libA, srA := insertLibraryAndScanResult(t, sqlDB, "/libA", "/libA/shared.mp3")
	_, srB := insertLibraryAndScanResult(t, sqlDB, "/libB", "/libB/shared.mp3")

	// Enqueue the same track (same artist/title) twice with different scan
	// results so both collapse onto one deduped work_queue row, linked to both
	// libraries via the work_queue_scan_results junction.
	track := models.Track{ArtistName: "Shared", TrackName: "Track"}
	first, err := q.Enqueue(ctx, models.Inputs{Track: track, Outdir: "out", Filename: "song.lrc", ScanResultID: srA}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	second, err := q.Enqueue(ctx, models.Inputs{Track: track, Outdir: "out", Filename: "song.lrc", ScanResultID: srB}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected dedup onto one row; got %d and %d", first.ID, second.ID)
	}
	wqID := first.ID

	// Drive the shared row to retired.
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if item.ID != wqID {
		t.Fatalf("dequeued %d; want shared row %d", item.ID, wqID)
	}
	if _, err := q.RetireMiss(ctx, wqID); err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}

	// Recheck only library A.
	n, err := q.RecheckRetired(ctx, &libA)
	if err != nil {
		t.Fatalf("RecheckRetired scoped: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecheckRetired scoped = %d; want 1 (shared row belongs to libA)", n)
	}

	// The shared work_queue row must be revived (deferred).
	var wqStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, wqID).Scan(&wqStatus); err != nil {
		t.Fatalf("read shared row: %v", err)
	}
	if wqStatus != StatusDeferred {
		t.Fatalf("shared row status = %q; want deferred", wqStatus)
	}

	// Library A's scan_result must be reset to pending.
	var aStatus, bStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, srA).Scan(&aStatus); err != nil {
		t.Fatalf("read srA: %v", err)
	}
	if aStatus != "pending" {
		t.Fatalf("srA status = %q; want pending (target library)", aStatus)
	}

	// Library B's scan_result must remain done -- no cross-library leak.
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, srB).Scan(&bStatus); err != nil {
		t.Fatalf("read srB: %v", err)
	}
	if bStatus != "done" {
		t.Fatalf("srB status = %q; want done (non-target library must be untouched)", bStatus)
	}
}

// TestDBQueue_RecheckRetired_NoRetiredRows verifies RecheckRetired is a clean
// no-op (0, nil) when no rows match the retired sentinel.
func TestDBQueue_RecheckRetired_NoRetiredRows(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }
	n, err := q.RecheckRetired(ctx, nil)
	if err != nil {
		t.Fatalf("RecheckRetired empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("RecheckRetired empty = %d; want 0", n)
	}
}

// TestDBQueue_RecheckClosedDB verifies the recheck methods return a wrapped
// error (rather than panicking) when the underlying handle is closed.
func TestDBQueue_RecheckClosedDB(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_ = sqlDB.Close()
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	if _, err := q.RecheckDeferred(ctx, nil); err == nil {
		t.Fatal("RecheckDeferred on closed db: want error, got nil")
	}
	if _, err := q.CountRecheckDeferred(ctx, nil); err == nil {
		t.Fatal("CountRecheckDeferred on closed db: want error, got nil")
	}
	if _, err := q.RecheckRetired(ctx, nil); err == nil {
		t.Fatal("RecheckRetired on closed db: want error, got nil")
	}
	if _, err := q.CountRecheckRetired(ctx, nil); err == nil {
		t.Fatal("CountRecheckRetired on closed db: want error, got nil")
	}
}
