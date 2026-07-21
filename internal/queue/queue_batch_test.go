package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
)

// enqueueN enqueues n scan-priority rows with distinct titles and returns the
// queue. The caller sets q.batchSize via SetBatchSize as needed.
func enqueueN(t *testing.T, q *DBQueue, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := q.Enqueue(ctx, models.Inputs{
			Track: models.Track{ArtistName: "Artist", TrackName: string(rune('A' + i))},
		}, PriorityScan); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

// bufferedIDsBySeq returns the ids of currently-buffered (batch_seq IS NOT NULL)
// eligible rows in ascending batch_seq order -- the order a batched Dequeue must
// serve them in.
func bufferedIDsBySeq(t *testing.T, q *DBQueue) []int64 {
	t.Helper()
	rows, err := q.db.QueryContext(context.Background(),
		`SELECT id FROM work_queue WHERE batch_seq IS NOT NULL ORDER BY batch_seq ASC`)
	if err != nil {
		t.Fatalf("query buffered ids: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ids: %v", err)
	}
	return ids
}

// A batch drains in its stamped order, and a fresh batch is drawn only once the
// current one empties. Enqueue 2*N rows with N = batchSize; the first N claims
// are batch 1 (stable stamped order), and only then does batch 2 draw.
func TestDBQueue_BatchDrainsInStampedOrderThenRedraws(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(3)

	enqueueN(t, q, 6)

	// First claim draws batch 1 (batch_seq 1..3 over three of the six rows) and
	// claims the lowest. Capture the batch composition right after the draw.
	first, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("first dequeue: %v", err)
	}
	// Remaining batch-1 members (seq 2, 3) plus the three still-unbuffered rows.
	batch1Remaining := bufferedIDsBySeq(t, q)
	if len(batch1Remaining) != 2 {
		t.Fatalf("batch of 3 minus one claim = %d buffered; want 2", len(batch1Remaining))
	}
	// The claimed row is not among the still-buffered ones.
	for _, id := range batch1Remaining {
		if id == first.ID {
			t.Fatalf("claimed row %d still buffered", first.ID)
		}
	}

	// The next two claims drain the rest of batch 1 in stamped order, before any
	// batch-2 row is served.
	batch1IDs := map[int64]bool{first.ID: true}
	for _, want := range batch1Remaining {
		got, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("drain batch 1: %v", err)
		}
		if got.ID != want {
			t.Fatalf("batch-1 drain order = %d; want stamped %d", got.ID, want)
		}
		batch1IDs[got.ID] = true
	}

	// Batch 1 is now empty (3 claimed). The next claim triggers batch 2 over the
	// remaining three rows -- none of which were in batch 1.
	b2, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("batch 2 dequeue: %v", err)
	}
	if batch1IDs[b2.ID] {
		t.Fatalf("row %d served in both batches", b2.ID)
	}
}

// batch_size = 0 restores the per-item random path (a clean rollback): the queue
// drains the full set with no drops or duplicates, and no row is ever stamped.
func TestDBQueue_BatchSizeZeroDrainsFullSet(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(0)

	const n = 8
	enqueueN(t, q, n)

	seen := map[int64]bool{}
	for range n {
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		if seen[item.ID] {
			t.Fatalf("row %d dequeued twice", item.ID)
		}
		seen[item.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("drained %d distinct rows; want %d", len(seen), n)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dequeue after drain: err = %v; want sql.ErrNoRows", err)
	}
	// batch_size = 0 must never stamp a row.
	var stamped int
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE batch_seq IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatalf("count stamped: %v", err)
	}
	if stamped != 0 {
		t.Fatalf("batch_size=0 stamped %d rows; want 0 (per-item path)", stamped)
	}
}

// batch_seq is advisory: a buffered row that has since fallen out of eligibility
// (here, completed by another path) is skipped, not served, because the claim
// re-applies the full eligibility predicate.
func TestDBQueue_BufferedButIneligibleRowIsSkipped(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(10)

	enqueueN(t, q, 3)
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("prime batch: %v", err)
	}
	buffered := bufferedIDsBySeq(t, q)
	if len(buffered) != 2 {
		t.Fatalf("buffered = %d; want 2", len(buffered))
	}

	// The lowest-batch_seq buffered row completes via another path (still carrying
	// its batch_seq stamp). The next claim must skip it and serve the other row.
	stale := buffered[0]
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET status = 'done' WHERE id = ?`, stale); err != nil {
		t.Fatalf("mark stale done: %v", err)
	}

	next, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue after stale: %v", err)
	}
	if next.ID == stale {
		t.Fatalf("served the ineligible (done) buffered row %d; want it skipped", stale)
	}
	if next.ID != buffered[1] {
		t.Fatalf("served %d; want the other buffered row %d", next.ID, buffered[1])
	}
}

// Anti-fingerprinting is retained at batch granularity: two independent draws
// over the same pool differ in composition/order (randomness is not lost, only
// moved from per-item to per-batch). Guards against a draw that accidentally
// became deterministic (e.g. losing the RANDOM() term).
func TestDBQueue_BatchDrawsAreRandomized(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(5)

	// A pool much larger than the batch, so a random 5-of-30 draw almost surely
	// differs between two independent draws.
	enqueueN(t, q, 30)

	drawOrder := func() []int64 {
		if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET batch_seq = NULL`); err != nil {
			t.Fatalf("clear batch_seq: %v", err)
		}
		if _, err := q.db.ExecContext(ctx, refillBufferSQL, 0, "2026-04-27T12:00:00Z", 5); err != nil {
			t.Fatalf("draw: %v", err)
		}
		return bufferedIDsBySeq(t, q)
	}

	a := drawOrder()
	b := drawOrder()
	if len(a) != 5 || len(b) != 5 {
		t.Fatalf("draw sizes = %d, %d; want 5, 5", len(a), len(b))
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatalf("two draws produced identical order %v; randomness not retained", a)
	}
}

// TestDBQueue_BufferRefillsEachDequeue verifies the buffer is topped back up to
// batch_size on every Dequeue (not only when it drains to empty), so the #572
// "Up next" panel never goes blank while eligible work exists (#587). With a pool
// far larger than the batch, the buffered count stays at batch_size-1 after each
// successive Dequeue (batch_size drawn, one just claimed). Discriminates the fix:
// the old drain-then-redraw behavior would count down 4,3,2,1,0 before redrawing.
func TestDBQueue_BufferRefillsEachDequeue(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(5)

	enqueueN(t, q, 30) // pool much larger than the batch so refill always succeeds

	for i := range 6 {
		if _, err := q.Dequeue(ctx); err != nil {
			t.Fatalf("dequeue %d: %v", i, err)
		}
		if got := len(bufferedIDsBySeq(t, q)); got != 4 {
			t.Fatalf("after dequeue %d: buffered = %d; want 4 (batch_size-1, rolling refill)", i, got)
		}
	}
}

// TestDBQueue_BatchedDequeueEmptyPool: with batching on and nothing eligible, the
// refill draws nothing and the claim returns sql.ErrNoRows (not a silent empty
// item).
func TestDBQueue_BatchedDequeueEmptyPool(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.SetBatchSize(5)

	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue on empty batched queue: err = %v; want sql.ErrNoRows", err)
	}
}

// TestDBQueue_RefillNoOpWhenBufferAtCapacity: when the buffered count already
// meets (or exceeds) batch_size, the refill is a no-op. Exercised by shrinking
// batch_size below the current buffer -- the guard must skip the draw rather than
// pass a negative LIMIT, which SQLite treats as "no limit" and would drain the
// whole pool into the buffer.
func TestDBQueue_RefillNoOpWhenBufferAtCapacity(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(5)
	enqueueN(t, q, 30)

	if _, err := q.Dequeue(ctx); err != nil { // refill(5) + claim -> 4 buffered
		t.Fatalf("prime: %v", err)
	}
	// Shrink below the current buffered count (4): the next refill's limit is
	// negative, so it must no-op instead of drawing the rest of the pool.
	q.SetBatchSize(3)
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue after shrink: %v", err)
	}
	if got := len(bufferedIDsBySeq(t, q)); got != 3 {
		t.Fatalf("buffered after shrink+claim = %d; want 3 (no refill, one claimed from 4)", got)
	}
}

// TestDBQueue_BatchedDequeueSurfacesRefillError: a failure in the buffer refill
// must surface, not be swallowed into a silent empty claim. Seed eligible rows
// through a writable handle, then reopen the same file read-only so BeginTx and
// the census SELECT succeed but the refill UPDATE is rejected (query_only).
func TestDBQueue_BatchedDequeueSurfacesRefillError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")

	w, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open writable: %v", err)
	}
	enqueueN(t, NewDBQueue(w), 5)
	if err := w.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}

	ro, err := db.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	q := NewDBQueue(ro)
	q.SetBatchSize(5)

	if _, err := q.Dequeue(ctx); err == nil {
		t.Fatal("Dequeue with a read-only DB (refill UPDATE rejected) returned nil error; want a surfaced failure")
	}
}

// A webhook-priority Enqueue preempts a partially drained batch: it clears the
// buffer, so the next Dequeue redraws a batch that includes the webhook row,
// which then wins on priority DESC. Without the buffer-clear the batched claim
// would keep serving the pre-drawn scan rows and the webhook would wait out the
// whole batch. Worst-case added latency is one dequeue (#571).
func TestDBQueue_WebhookEnqueuePreemptsBatch(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(10)

	enqueueN(t, q, 5)

	// Draw the batch and claim one, so a partially drained buffer of scan rows
	// exists.
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("prime batch: %v", err)
	}
	if got := len(bufferedIDsBySeq(t, q)); got != 4 {
		t.Fatalf("buffered scan rows = %d; want 4 (active buffer)", got)
	}

	// A webhook arrives mid-batch.
	webhook, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Webhook", TrackName: "Urgent"},
	}, PriorityWebhook)
	if err != nil {
		t.Fatalf("enqueue webhook: %v", err)
	}

	// The very next claim must be the webhook, ahead of the pre-drawn scan rows.
	next, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue after webhook: %v", err)
	}
	if next.ID != webhook.ID {
		t.Fatalf("next claim ID = %d (%q); want webhook ID %d -- webhook did not preempt the batch",
			next.ID, next.Inputs.Track.TrackName, webhook.ID)
	}
}

// The draw must keep the top-N rows by priority, not an arbitrary N. With a pool
// one larger than batch_size, LIMIT drops exactly one row; the highest-priority
// (webhook) row must never be the one dropped. Without a top-level ORDER BY rn on
// the ranked subquery, SQLite's LIMIT keeps an undefined subset, so the webhook
// could be excluded from the batch and wait behind scan rows -- a priority
// inversion (#571 review). Assert across several independent draws, since the
// bug is order-dependent and would otherwise pass by luck.
func TestDBQueue_DrawKeepsHighestPriorityUnderLimit(t *testing.T) {
	ctx := context.Background()
	for attempt := range 8 {
		q := NewDBQueue(openQueueTestDB(t))
		q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
		q.SetBatchSize(5)

		// Five scan rows plus one webhook = pool of 6, one larger than the batch,
		// so the draw's LIMIT 5 must drop one row -- never the webhook.
		enqueueN(t, q, 5)
		webhook, err := q.Enqueue(ctx, models.Inputs{
			Track: models.Track{ArtistName: "Webhook", TrackName: "Urgent"},
		}, PriorityWebhook)
		if err != nil {
			t.Fatalf("attempt %d: enqueue webhook: %v", attempt, err)
		}

		first, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("attempt %d: dequeue: %v", attempt, err)
		}
		if first.ID != webhook.ID {
			t.Fatalf("attempt %d: first claim ID = %d (%q); want webhook %d -- highest-priority row dropped from the batch",
				attempt, first.ID, first.Inputs.Track.TrackName, webhook.ID)
		}
	}
}

// The batched Dequeue surfaces a DB failure rather than swallowing it: a closed
// DB fails BeginTx, and the error must propagate (not a silent empty claim).
func TestDBQueue_BatchedDequeueSurfacesDBError(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.SetBatchSize(10)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := q.Dequeue(ctx)
	if err == nil {
		t.Fatal("Dequeue on a closed DB returned nil error; want a surfaced failure")
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue on a closed DB returned ErrNoRows; want a real error, got %v", err)
	}
}

// A batch, once drawn, keeps its order across a restart: the remaining rows
// drain in their stamped batch_seq order with no redraw and no stranded state.
// This is the main reason the buffer lives in the DB rather than worker memory
// (#571). Discriminates against the pre-#571 per-claim RANDOM() path, which
// stamps no batch_seq and re-randomizes on every dequeue.
func TestDBQueue_BatchOrderSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	q.SetBatchSize(10)

	enqueueN(t, q, 6)

	// First Dequeue draws the batch (stamping batch_seq = 1..6) and claims the
	// lowest. The remaining five stay buffered in their stamped order.
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("first dequeue: %v", err)
	}
	want := bufferedIDsBySeq(t, q)
	if len(want) != 5 {
		t.Fatalf("buffered after one claim = %d rows; want 5 (batch of 6 minus the claimed row)", len(want))
	}

	// A fresh queue over the same DB (a restart) must continue the same batch,
	// not redraw. Serve order must equal the preserved batch_seq order.
	q2 := NewDBQueue(sqlDB)
	q2.now = q.now
	q2.SetBatchSize(10)
	var got []int64
	for range want {
		item, err := q2.Dequeue(ctx)
		if err != nil {
			t.Fatalf("post-restart dequeue: %v", err)
		}
		got = append(got, item.ID)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("post-restart serve order = %v; want preserved batch_seq order %v", got, want)
		}
	}

	// Pool is now empty.
	if _, err := q2.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dequeue after drain: err = %v; want sql.ErrNoRows", err)
	}
}
