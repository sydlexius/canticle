package queue

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// refusedRow is the column set DeferRefused may or may not touch.
type refusedRow struct {
	status, nextAttemptAt, lastError string
	refusedWaits, missCount          int
	attempts, priority               int
}

func readRefusedRow(t *testing.T, q *DBQueue, id int64) refusedRow {
	t.Helper()
	var r refusedRow
	if err := q.db.QueryRowContext(context.Background(),
		`SELECT status, next_attempt_at, last_error, refused_waits, miss_count, attempts, priority
         FROM work_queue WHERE id = ?`, id,
	).Scan(&r.status, &r.nextAttemptAt, &r.lastError, &r.refusedWaits, &r.missCount, &r.attempts, &r.priority); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return r
}

// newRefusedQueue returns a deterministic queue at a fixed clock with one
// claimed (processing) row carrying non-zero attempts/miss_count so an
// accidental write to either is visible.
func newRefusedQueue(t *testing.T, refusedWaits int) (*DBQueue, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.SetRandomized(false)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET refused_waits = ?, attempts = 2, miss_count = 3 WHERE id = ?`,
		refusedWaits, item.ID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	return q, item.ID, now
}

func TestDBQueue_DeferRefusedBelowCap(t *testing.T) {
	q, id, now := newRefusedQueue(t, 0)
	before := readRefusedRow(t, q, id)

	const wait = 15 * time.Minute
	deferred, err := q.DeferRefused(context.Background(), id, wait, 2, "timing refused; waiting on musixmatch")
	if err != nil || !deferred {
		t.Fatalf("DeferRefused = (%v, %v); want (true, nil)", deferred, err)
	}
	got := readRefusedRow(t, q, id)
	if got.status != StatusDeferred {
		t.Errorf("status = %q; want %q", got.status, StatusDeferred)
	}
	if got.refusedWaits != 1 {
		t.Errorf("refused_waits = %d; want 1", got.refusedWaits)
	}
	if want := formatTime(now.Add(wait)); got.nextAttemptAt != want {
		t.Errorf("next_attempt_at = %q; want %q", got.nextAttemptAt, want)
	}
	if got.lastError != "timing refused; waiting on musixmatch" {
		t.Errorf("last_error = %q; want the cause", got.lastError)
	}
	if got.missCount != before.missCount || got.attempts != before.attempts || got.priority != before.priority {
		t.Errorf("miss_count/attempts/priority = %d/%d/%d; want unchanged %d/%d/%d",
			got.missCount, got.attempts, got.priority, before.missCount, before.attempts, before.priority)
	}
}

func TestDBQueue_DeferRefusedAtCapChangesNothing(t *testing.T) {
	q, id, _ := newRefusedQueue(t, 2)
	before := readRefusedRow(t, q, id)

	deferred, err := q.DeferRefused(context.Background(), id, time.Hour, 2, "cause")
	if err != nil || deferred {
		t.Fatalf("DeferRefused at cap = (%v, %v); want (false, nil)", deferred, err)
	}
	if got := readRefusedRow(t, q, id); got != before {
		t.Errorf("row changed at cap: got %+v; want %+v", got, before)
	}
}

// The cap is inclusive: maxWaits waits are granted, never maxWaits+1.
func TestDBQueue_DeferRefusedCapIsInclusive(t *testing.T) {
	ctx := context.Background()
	q, id, _ := newRefusedQueue(t, 1)

	if deferred, err := q.DeferRefused(ctx, id, time.Minute, 2, "c"); err != nil || !deferred {
		t.Fatalf("second wait = (%v, %v); want (true, nil)", deferred, err)
	}
	// Put it back in the worker's hands, as a re-dequeue would.
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if deferred, err := q.DeferRefused(ctx, id, time.Minute, 2, "c"); err != nil || deferred {
		t.Fatalf("third wait = (%v, %v); want (false, nil)", deferred, err)
	}
	if got := readRefusedRow(t, q, id); got.refusedWaits != 2 || got.status != StatusProcessing {
		t.Errorf("after cap: refused_waits=%d status=%q; want 2, processing", got.refusedWaits, got.status)
	}
}

func TestDBQueue_DeferRefusedNonProcessingRow(t *testing.T) {
	ctx := context.Background()
	q, id, _ := newRefusedQueue(t, 0)
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'pending' WHERE id = ?`, id); err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	before := readRefusedRow(t, q, id)

	if _, err := q.DeferRefused(ctx, id, time.Minute, 5, "c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeferRefused on pending row err = %v; want sql.ErrNoRows", err)
	}
	if got := readRefusedRow(t, q, id); got != before {
		t.Errorf("row changed: got %+v; want %+v", got, before)
	}
}

func TestDBQueue_CompleteResetsRefusedWaits(t *testing.T) {
	q, id, _ := newRefusedQueue(t, 2)
	if err := q.Complete(context.Background(), id); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readRefusedRow(t, q, id); got.refusedWaits != 0 {
		t.Errorf("refused_waits after Complete = %d; want 0 (fresh budget if reopened)", got.refusedWaits)
	}
}

// A waiting row leaves the ready set, so other work is served while it waits
// (no head-of-line starvation), and it returns once next_attempt_at passes. A
// scan-priority re-enqueue of the same track must not cut the wait short.
func TestDBQueue_DeferRefusedRowWaitsWithoutBlockingOthers(t *testing.T) {
	ctx := context.Background()
	q, id, now := newRefusedQueue(t, 0)
	other, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Other", TrackName: "Song"}}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue other: %v", err)
	}

	const wait = 30 * time.Minute
	if deferred, err := q.DeferRefused(ctx, id, wait, 3, "c"); err != nil || !deferred {
		t.Fatalf("DeferRefused = (%v, %v)", deferred, err)
	}
	waiting := readRefusedRow(t, q, id)

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if got := readRefusedRow(t, q, id); got.status != StatusDeferred || got.nextAttemptAt != waiting.nextAttemptAt {
		t.Errorf("after scan re-enqueue: status=%q next=%q; want deferred, %q", got.status, got.nextAttemptAt, waiting.nextAttemptAt)
	}

	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue while waiting: %v", err)
	}
	if claimed.ID != other.ID {
		t.Fatalf("dequeued %d while row %d waits; want the other row %d", claimed.ID, id, other.ID)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second Dequeue before wait elapses = %v; want sql.ErrNoRows", err)
	}

	q.now = func() time.Time { return now.Add(wait) }
	again, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue after wait: %v", err)
	}
	if again.ID != id {
		t.Fatalf("dequeued %d after wait; want %d", again.ID, id)
	}
}

// Every path that reopens a settled row resets the wait budget, not only the
// queue's own settles. prune's retireUnresolvable can settle a row straight to
// 'done' from 'deferred' mid-wait, so a spent refused_waits survives on the row;
// each instrumental reopen must clear it or the reopened row starts with less
// than a full budget.
func TestDBQueue_InstrumentalReopenPathsResetRefusedWaits(t *testing.T) {
	cases := []struct {
		name   string
		reopen func(ctx context.Context, q *DBQueue, id int64) error
	}{
		{"ResetInstrumental", func(ctx context.Context, q *DBQueue, id int64) error {
			_, err := q.ResetInstrumental(ctx, id)
			return err
		}},
		{"UnsettleInstrumental", func(ctx context.Context, q *DBQueue, id int64) error {
			ok, err := q.UnsettleInstrumental(ctx, id)
			if err == nil && !ok {
				return errors.New("UnsettleInstrumental reverted nothing")
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q, id, _ := newRefusedQueue(t, 0)
			// A settled instrumental row carrying a spent budget, the shape
			// retireUnresolvable leaves behind when it settles a waiting row.
			if _, err := q.db.ExecContext(ctx,
				`UPDATE work_queue SET status = 'done', instrumental_result = 1, refused_waits = 3 WHERE id = ?`,
				id); err != nil {
				t.Fatalf("seed settled row: %v", err)
			}
			if err := tc.reopen(ctx, q, id); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := readRefusedRow(t, q, id)
			if got.status != "deferred" {
				t.Fatalf("status after %s = %q; want deferred (the reopen did not run)", tc.name, got.status)
			}
			if got.refusedWaits != 0 {
				t.Errorf("refused_waits after %s = %d; want 0 (a reopened row gets a fresh wait budget)", tc.name, got.refusedWaits)
			}
		})
	}
}
