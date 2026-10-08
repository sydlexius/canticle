package queue

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// ReleaseUntil (#1430): a processing row goes back to its prior status, leaves
// the ready set until the wait is over, and is charged nothing.
func TestDBQueue_ReleaseUntil(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }

	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, item.ID, errors.New("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	now = now.Add(48 * time.Hour) // past any backoff: claimable again
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("re-Dequeue: %v", err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET miss_count = 3, refused_waits = 2 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	type counters struct {
		attempts, miss, waits, priority int
		lastError                       string
	}
	read := func() (st, next string, c counters) {
		t.Helper()
		if err := q.db.QueryRowContext(ctx,
			`SELECT status, next_attempt_at, attempts, miss_count, refused_waits, priority, last_error FROM work_queue WHERE id = ?`, item.ID,
		).Scan(&st, &next, &c.attempts, &c.miss, &c.waits, &c.priority, &c.lastError); err != nil {
			t.Fatal(err)
		}
		return st, next, c
	}
	_, _, before := read()

	const wait = 5 * time.Minute
	if err := q.ReleaseUntil(ctx, item.ID, wait); err != nil {
		t.Fatalf("ReleaseUntil: %v", err)
	}
	st, next, after := read()
	if st != StatusFailed {
		t.Errorf("status = %q, want %q (the prior status restored)", st, StatusFailed)
	}
	if want := formatTime(now.Add(wait)); next != want {
		t.Errorf("next_attempt_at = %q, want %q", next, want)
	}
	if after != before {
		t.Errorf("counters/last_error changed: %+v -> %+v; nothing may be charged", before, after)
	}
	// Not ready during the wait, ready after it.
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue during the wait = %v, want sql.ErrNoRows", err)
	}
	now = now.Add(wait + time.Second)
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue after the wait: %v", err)
	}

	// A row that is no longer processing is an error and is left as it was.
	if err := q.Release(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	_, nextBefore, _ := read()
	if err := q.ReleaseUntil(ctx, item.ID, time.Hour); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ReleaseUntil on a non-processing row = %v, want sql.ErrNoRows", err)
	}
	if _, nextAfter, _ := read(); nextAfter != nextBefore {
		t.Errorf("next_attempt_at moved on a refused release: %q -> %q", nextBefore, nextAfter)
	}
}

// ReleaseUntil restores the row's own prior status and charges nothing, for
// every status a claim can come from (#1430). Each case claims a row, forces
// the prior status and non-zero counters, then asserts the status and every
// counter come back untouched.
func TestDBQueue_ReleaseUntil_EachPriorStatus(t *testing.T) {
	cases := []struct {
		name      string
		prev      string
		lastError string
	}{
		{name: "pending", prev: StatusPending, lastError: ""},
		{name: "deferred", prev: StatusDeferred, lastError: "no lyrics found"},
		{name: "failed", prev: StatusFailed, lastError: "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q := NewDBQueue(openQueueTestDB(t))
			now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
			q.now = func() time.Time { return now }

			item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, PriorityScan)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if _, err := q.Dequeue(ctx); err != nil {
				t.Fatalf("Dequeue: %v", err)
			}
			if _, err := q.db.ExecContext(ctx,
				`UPDATE work_queue SET prev_status = ?, attempts = 4, miss_count = 3, refused_waits = 2, last_error = ? WHERE id = ?`,
				tc.prev, tc.lastError, item.ID); err != nil {
				t.Fatal(err)
			}
			type counters struct {
				attempts, miss, waits, priority int
				lastError                       string
			}
			read := func() (st, next string, c counters) {
				t.Helper()
				if err := q.db.QueryRowContext(ctx,
					`SELECT status, next_attempt_at, attempts, miss_count, refused_waits, priority, last_error FROM work_queue WHERE id = ?`, item.ID,
				).Scan(&st, &next, &c.attempts, &c.miss, &c.waits, &c.priority, &c.lastError); err != nil {
					t.Fatal(err)
				}
				return st, next, c
			}
			_, _, before := read()

			const wait = 5 * time.Minute
			if err := q.ReleaseUntil(ctx, item.ID, wait); err != nil {
				t.Fatalf("ReleaseUntil: %v", err)
			}
			st, next, after := read()
			if st != tc.prev {
				t.Errorf("status = %q, want %q (the prior status restored)", st, tc.prev)
			}
			if want := formatTime(now.Add(wait)); next != want {
				t.Errorf("next_attempt_at = %q, want %q", next, want)
			}
			if after != before {
				t.Errorf("counters/last_error changed: %+v -> %+v; nothing may be charged", before, after)
			}
		})
	}
}
