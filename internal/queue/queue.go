package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/backoff"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
)

const (
	// StatusPending marks queued work ready to be processed.
	StatusPending = "pending"
	// StatusProcessing marks queued work currently being processed.
	StatusProcessing = "processing"
	// StatusDone marks queued work completed successfully.
	StatusDone = "done"
	// StatusFailed marks queued work that failed and may be retried after backoff.
	StatusFailed = "failed"
	// StatusDeferred marks queued work that produced a benign no-result miss and
	// has been rescheduled after a fixed cooldown. Unlike StatusFailed, a deferred
	// row does not count against the geometric retry budget: Defer leaves attempts
	// unchanged so each re-check waits the same fixed window. Deferred rows are
	// eligible for Dequeue once their next_attempt_at has elapsed, and they are
	// preserved by scan-priority Enqueue so bulk library scans cannot un-defer
	// them. A webhook-priority Enqueue resets next_attempt_at to now, providing
	// an intentional force-recheck escape hatch.
	StatusDeferred = "deferred"
)

const timeFormat = time.RFC3339

// ErrNotRetryable is returned by Retry when the targeted work item is not in
// the failed state and therefore cannot be safely reset (e.g. processing,
// done, or deferred). Deferred rows must use the Enqueue webhook-priority path
// to force an immediate re-check; Retry is intentionally not wired for them.
// This avoids racing the worker on rows it currently owns.
var ErrNotRetryable = errors.New("queue: work item is not in failed status")

// InputsQueue is a FIFO queue for processing work items.
type InputsQueue struct {
	Queue []models.Inputs
}

// NewInputsQueue creates an empty InputsQueue.
func NewInputsQueue() *InputsQueue {
	return &InputsQueue{}
}

// Next returns the front item without removing it, or an error if the queue is empty.
func (q *InputsQueue) Next() (models.Inputs, error) {
	if q.Empty() {
		return models.Inputs{}, fmt.Errorf("queue is empty")
	}
	return q.Queue[0], nil
}

// Pop removes and returns the front item, or an error if the queue is empty.
func (q *InputsQueue) Pop() (models.Inputs, error) {
	if q.Empty() {
		return models.Inputs{}, fmt.Errorf("queue is empty")
	}
	tmp := q.Queue[0]
	q.Queue[0] = models.Inputs{} // clear reference to avoid memory leak
	q.Queue = q.Queue[1:]
	return tmp, nil
}

// Push adds an item to the back of the queue.
func (q *InputsQueue) Push(i models.Inputs) {
	q.Queue = append(q.Queue, i)
}

// Len returns the number of items in the queue.
func (q *InputsQueue) Len() int {
	return len(q.Queue)
}

// Empty returns true if the queue has no items.
func (q *InputsQueue) Empty() bool {
	return len(q.Queue) == 0
}

// WorkItem represents a persisted queue row.
type WorkItem struct {
	ID        int64
	Inputs    models.Inputs
	Status    string
	Priority  int
	Attempts  int
	MissCount int
	// ProvidersVersion is a reserved seam for the future multi-source re-check
	// sweep (issue #103, slice 103d). No code path writes it yet, so it is
	// always 0 today; do not read a real provider generation from it.
	ProvidersVersion int
	NextAttemptAt    time.Time
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
}

// DBQueue is a SQLite-backed queue for durable lyrics work.
type DBQueue struct {
	db          *sql.DB
	baseBackoff time.Duration
	maxBackoff  time.Duration
	now         func() time.Time
	// randomized shuffles the dequeue order within each priority tier to remove
	// the strictly-alphabetical request fingerprint. On by default; flip to false
	// (via SetRandomized) to restore deterministic created_at/id ordering. Also
	// doubles as the test seam for deterministic ordering assertions.
	randomized bool
	// providersVersion is the current providers generation stamped onto new
	// work_queue rows by Enqueue. 0 means "not configured" and preserves
	// backward compatibility with call sites that do not supply a generation.
	// Set via SetProvidersVersion from the commands layer after config is loaded.
	providersVersion int
}

// NewDBQueue returns a durable queue backed by db.
func NewDBQueue(db *sql.DB) *DBQueue {
	return &DBQueue{
		db:          db,
		baseBackoff: backoff.DefaultBase,
		maxBackoff:  backoff.DefaultMax,
		now:         time.Now,
		randomized:  true,
	}
}

// SetRandomized toggles whether Dequeue shuffles within a priority tier. It lets
// callers apply the configured queue.randomize / MXLRC_QUEUE_RANDOMIZE setting
// without changing the NewDBQueue call sites.
func (q *DBQueue) SetRandomized(b bool) {
	q.randomized = b
}

// SetProvidersVersion configures the providers generation stamped onto new
// work_queue rows by Enqueue. The generation is computed by providers.Generation
// from the current active provider set and changes when providers are added or
// removed. A value of 0 (the default) preserves backward compatibility.
func (q *DBQueue) SetProvidersVersion(v int) {
	q.providersVersion = v
}

// Enqueue atomically inserts a new work item or refreshes an existing retryable
// item with the same normalized artist/title key. When the item carries a
// scan_result_id, the link is also recorded in work_queue_scan_results so a
// later Complete writeback can flip every collapsed scan_results row, not just
// the first one observed.
//
// Priority update semantics on conflict:
//   - A webhook-priority (>= PriorityWebhook) enqueue always overrides the
//     stored priority so an explicit webhook can always preempt a deferred miss.
//   - A scan-priority (< PriorityWebhook) enqueue preserves the stored priority
//     when the row is deferred so that PriorityMiss deprioritization survives
//     bulk library scans and the deferred row stays behind fresh work.
//   - For all other states (pending, failed) the higher of the two priorities wins.
func (q *DBQueue) Enqueue(ctx context.Context, inputs models.Inputs, priority int) (WorkItem, error) {
	now := formatTime(q.now())
	outputPaths, err := marshalOutputPaths(inputs)
	if err != nil {
		return WorkItem{}, err
	}
	// High-priority (webhook) duplicates of a failed row reset next_attempt_at to
	// now so the work becomes immediately retry-eligible rather than waiting out
	// the previous backoff. Scan-priority duplicates preserve the existing backoff
	// so rate-limit protection stays in effect for bulk scan traffic. The worker's
	// global circuit breaker still guards against actual upstream rate limits.
	refreshFailedBackoff := 0
	if priority >= PriorityWebhook {
		refreshFailedBackoff = 1
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`INSERT INTO work_queue (
             artist, title, album, album_artist, artist_key, title_key, outdir, filename, source_path, output_paths, scan_result_id, status, priority, providers_version, next_attempt_at
         )
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(artist_key, title_key) DO UPDATE SET
             artist = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.artist
                 ELSE excluded.artist
             END,
             title = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.title
                 ELSE excluded.title
             END,
             album = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.album
                 ELSE excluded.album
             END,
             album_artist = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.album_artist
                 ELSE excluded.album_artist
             END,
             outdir = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.outdir
                 ELSE excluded.outdir
             END,
             filename = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.filename
                 ELSE excluded.filename
             END,
             source_path = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.source_path
                 ELSE excluded.source_path
             END,
             output_paths = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.output_paths
                 ELSE excluded.output_paths
             END,
             scan_result_id = COALESCE(work_queue.scan_result_id, excluded.scan_result_id),
             priority = CASE
                 WHEN excluded.priority >= 10 THEN excluded.priority           -- PriorityWebhook always wins
                 WHEN work_queue.status = 'deferred' THEN work_queue.priority  -- preserve miss deprioritization
                 ELSE max(work_queue.priority, excluded.priority)
             END,
             status = CASE
                 WHEN work_queue.status IN ('done', 'processing', 'failed', 'deferred') THEN work_queue.status
                 ELSE 'pending'
             END,
             next_attempt_at = CASE
                 WHEN work_queue.status IN ('done', 'processing') THEN work_queue.next_attempt_at
                 WHEN work_queue.status = 'failed' AND ? = 1 THEN excluded.next_attempt_at
                 WHEN work_queue.status = 'failed' THEN work_queue.next_attempt_at
                 WHEN work_queue.status = 'deferred' AND ? = 1 THEN excluded.next_attempt_at
                 WHEN work_queue.status = 'deferred' THEN work_queue.next_attempt_at
                 ELSE excluded.next_attempt_at
             END,
             last_error = CASE
                 WHEN work_queue.status IN ('done', 'processing', 'failed', 'deferred') THEN work_queue.last_error
                 ELSE ''
             END,
             completed_at = CASE
                 WHEN work_queue.status = 'done' THEN work_queue.completed_at
                 ELSE NULL
             END
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		inputs.Track.ArtistName,
		inputs.Track.TrackName,
		inputs.Track.AlbumName,
		inputs.Track.AlbumArtist,
		normalize.NormalizeKey(inputs.Track.ArtistName),
		normalize.NormalizeKey(inputs.Track.TrackName),
		inputs.Outdir,
		inputs.Filename,
		inputs.SourcePath,
		outputPaths,
		nullableID(inputs.ScanResultID),
		StatusPending,
		priority,
		q.providersVersion,
		now,
		refreshFailedBackoff,
		refreshFailedBackoff,
	)
	item, err := scanWorkItem(row)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: enqueue: %w", err)
	}
	if inputs.ScanResultID > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id)
             VALUES (?, ?)`,
			item.ID, inputs.ScanResultID,
		); err != nil {
			return WorkItem{}, fmt.Errorf("queue: link scan_result %d: %w", inputs.ScanResultID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkItem{}, fmt.Errorf("queue: commit enqueue tx: %w", err)
	}
	return item, nil
}

// dequeueRandomizedSQL claims the next ready item, shuffling within a priority
// tier (anti-scraping fingerprint). The ORDER BY lives inside the subquery, so
// each variant is a complete, standalone statement (no concatenation, no
// interpolation -> no gosec concern).
const dequeueRandomizedSQL = `UPDATE work_queue
         SET status = 'processing'
         WHERE id = (
             SELECT id
             FROM work_queue
             WHERE status IN ('pending', 'failed', 'deferred')
               AND next_attempt_at <= ?
             ORDER BY priority DESC, RANDOM()
             LIMIT 1
         )
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`

// dequeueDeterministicSQL claims the next ready item in stable FIFO order within
// a priority tier (created_at, then id).
const dequeueDeterministicSQL = `UPDATE work_queue
         SET status = 'processing'
         WHERE id = (
             SELECT id
             FROM work_queue
             WHERE status IN ('pending', 'failed', 'deferred')
               AND next_attempt_at <= ?
             ORDER BY priority DESC, created_at ASC, id ASC
             LIMIT 1
         )
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`

// Dequeue atomically claims the next ready item and marks it processing.
func (q *DBQueue) Dequeue(ctx context.Context) (WorkItem, error) {
	now := formatTime(q.now())
	query := dequeueDeterministicSQL
	if q.randomized {
		query = dequeueRandomizedSQL
	}
	row := q.db.QueryRowContext(ctx, query, now)
	item, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItem{}, sql.ErrNoRows
	}
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: dequeue: %w", err)
	}
	return item, nil
}

// Complete marks a processing item done. Every scan_results row linked through
// work_queue_scan_results is flipped to 'done' inside the same transaction, so
// a successful Complete guarantees the work_queue row and all originating
// scan_results agree. Crash or partial-write between the updates is impossible:
// SQLite either commits the whole transaction or rolls back.
func (q *DBQueue) Complete(ctx context.Context, id int64) error {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queue: begin complete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET status = 'done',
             completed_at = ?,
             last_error = ''
         WHERE id = ?
           AND status = 'processing'`,
		now,
		id,
	)
	if err != nil {
		return fmt.Errorf("queue: complete: %w", err)
	}
	if err := requireAffected(res, "queue: complete"); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'done'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status != 'done'`,
		id,
	); err != nil {
		return fmt.Errorf("queue: complete scan_results writeback: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: commit complete tx: %w", err)
	}
	return nil
}

// Release returns a processing item to the pending pool without recording a
// failure. Used when the worker dequeued the item but cannot process it for a
// reason that should not count against the row's retry budget (e.g. the
// global rate-limit circuit breaker tripped). Attempts and next_attempt_at
// are left untouched so the row is immediately eligible for the next dequeue.
func (q *DBQueue) Release(ctx context.Context, id int64) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET status = 'pending',
             last_error = ''
         WHERE id = ?
           AND status = 'processing'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("queue: release: %w", err)
	}
	return requireAffected(res, "queue: release")
}

// Cleanup removes retryable queued work for the same normalized artist/title.
// Processing and completed rows are preserved to avoid racing active workers or
// losing history for work that has already finished.
func (q *DBQueue) Cleanup(ctx context.Context, inputs models.Inputs) (int64, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM work_queue
         WHERE artist_key = ?
           AND title_key = ?
           AND status IN ('pending', 'failed', 'deferred')`,
		normalize.NormalizeKey(inputs.Track.ArtistName),
		normalize.NormalizeKey(inputs.Track.TrackName),
	)
	if err != nil {
		return 0, fmt.Errorf("queue: cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: cleanup rows affected: %w", err)
	}
	return n, nil
}

// Fail records a failed attempt and schedules the item after geometric backoff.
func (q *DBQueue) Fail(ctx context.Context, id int64, cause error) (WorkItem, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: begin fail tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var attempts int
	if err := tx.QueryRowContext(ctx,
		`SELECT attempts FROM work_queue WHERE id = ? AND status = 'processing'`,
		id,
	).Scan(&attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkItem{}, sql.ErrNoRows
		}
		return WorkItem{}, fmt.Errorf("queue: load attempts: %w", err)
	}

	nextAttempts := attempts + 1
	nextAttemptAt := formatTime(q.now().Add(backoff.Geometric(nextAttempts, q.baseBackoff, q.maxBackoff)))
	lastError := ""
	if cause != nil {
		lastError = cause.Error()
	}
	row := tx.QueryRowContext(ctx,
		`UPDATE work_queue
         SET status = 'failed',
             attempts = ?,
             next_attempt_at = ?,
             last_error = ?
         WHERE id = ?
           AND status = 'processing'
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		nextAttempts,
		nextAttemptAt,
		lastError,
		id,
	)
	item, err := scanWorkItem(row)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: fail: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkItem{}, fmt.Errorf("queue: commit fail tx: %w", err)
	}
	return item, nil
}

// Defer reschedules a processing item after a fixed retryAfter delay without
// counting the attempt against the retry budget. Unlike Fail, attempts is left
// unchanged so the delay does not ramp into geometric backoff: every Defer of
// the same row waits the same fixed window. miss_count is incremented so the
// number of benign misses on a row is observable and available to future
// escalation logic. The row is moved to the distinct 'deferred' state (not
// 'pending' or 'failed'): 'deferred' is its own queryable status, so benign
// misses do not pollute the failures view, and Enqueue preserves next_attempt_at
// for 'deferred' rows just as it does for 'failed' ones, so the cooldown survives
// a later library scan rather than being un-deferred on every scan. The reset is
// asymmetric, matching Enqueue's refreshFailedBackoff logic: a scan-priority
// Enqueue preserves the deferred next_attempt_at (the cooldown survives bulk
// scans), but a webhook-priority Enqueue (priority >= PriorityWebhook) resets
// next_attempt_at to now, so an explicit webhook can force an immediate re-check
// despite the cooldown. Used by the worker for benign misses (no matching track,
// or a match with no usable lyrics).
func (q *DBQueue) Defer(ctx context.Context, id int64, retryAfter time.Duration, cause error) (WorkItem, error) {
	nextAttemptAt := formatTime(q.now().Add(retryAfter))
	lastError := ""
	if cause != nil {
		lastError = cause.Error()
	}
	row := q.db.QueryRowContext(ctx,
		// priority is set to PriorityMiss (-100) so that re-attempts from the
		// escalating miss cadence sink below all fresh work in the dequeue
		// ORDER BY priority DESC sort. The literal -100 is used here rather than
		// a Go constant because the SQL driver does not accept named Go values.
		`UPDATE work_queue
         SET status = 'deferred',
             miss_count = miss_count + 1,
             priority = -100,
             next_attempt_at = ?,
             last_error = ?
         WHERE id = ?
           AND status = 'processing'
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		nextAttemptAt,
		lastError,
		id,
	)
	item, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItem{}, sql.ErrNoRows
	}
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: defer: %w", err)
	}
	return item, nil
}

// missLimitReachedError is the last_error value RetireMiss writes when a benign
// miss is retired after exhausting max_miss_attempts. Defined once so the SQL
// bind, the tests, and any log/inspection of the sentinel cannot drift.
const missLimitReachedError = "miss limit reached"

// RetireMiss permanently closes a processing row that has exceeded the
// configured miss-attempt cap. It runs a transaction that mirrors Complete's
// scan_results writeback: work_queue is set to status='done' with sentinel
// last_error "miss limit reached", and every linked scan_results row is also
// set to status='done' so the scan layer does not strand the track in
// 'processing' forever. Unlike Complete (which signals a successful lyrics
// fetch), the last_error clearly marks this as a miss-limit terminal; the
// distinction is visible in the last_error field, not in the status column.
//
// Retirement is terminal under current providers. A future multi-source sweep
// (issue #103, slice 103d) could revive a retired track by resetting its
// scan_results row back to 'pending', but no such mechanism exists today.
//
// The guard AND status = 'processing' ensures only the worker that currently
// holds the row can retire it; sql.ErrNoRows is returned if the row moved on
// (a benign lost race).
func (q *DBQueue) RetireMiss(ctx context.Context, id int64) (WorkItem, error) {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: begin retire miss tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`UPDATE work_queue
         SET status = 'done',
             completed_at = ?,
             last_error = ?
         WHERE id = ?
           AND status = 'processing'
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		now,
		missLimitReachedError,
		id,
	)
	item, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItem{}, sql.ErrNoRows
	}
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: retire miss: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'done'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status != 'done'`,
		id,
	); err != nil {
		return WorkItem{}, fmt.Errorf("queue: retire miss scan_results writeback: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return WorkItem{}, fmt.Errorf("queue: commit retire miss tx: %w", err)
	}
	return item, nil
}

// recheckLibraryClause returns the SQL predicate fragment and extra args for
// scoping a recheck operation to a single library. When libraryID is nil the
// clause is empty (all libraries). The subquery joins through
// work_queue_scan_results -> scan_results so a work_queue row is considered
// "in" a library when it has at least one linked scan_result belonging to that
// library.
func recheckLibraryClause(libraryID *int64) (clause string, args []any) {
	if libraryID == nil {
		return "", nil
	}
	return " AND id IN (SELECT wqsr.work_queue_id FROM work_queue_scan_results wqsr" +
		" JOIN scan_results sr ON sr.id = wqsr.scan_result_id WHERE sr.library_id = ?)", []any{*libraryID}
}

// RecheckDeferred resets next_attempt_at to now for all rows currently in the
// 'deferred' (benign-miss cooldown) state. The worker's deferred sweep will
// pick them up on the next tick. status, priority, miss_count, and
// providers_version are left unchanged.
//
// Unlike RecheckRetired, this deliberately does not touch the linked
// scan_results: a deferred work_queue row is non-terminal and remains the
// active driver for the track, so re-arming next_attempt_at is sufficient for
// the worker to re-process it (which then updates scan_results as usual). Only
// retired rows (status='done') need their scan layer revived.
//
// When libraryID is non-nil only rows linked to that library are revived.
// Returns the number of rows affected.
func (q *DBQueue) RecheckDeferred(ctx context.Context, libraryID *int64) (int64, error) {
	now := formatTime(q.now())
	libClause, libArgs := recheckLibraryClause(libraryID)
	args := append([]any{now}, libArgs...)
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET next_attempt_at = ? WHERE status = 'deferred'`+libClause, //nolint:gosec // G202: libClause is a hardcoded constant from recheckLibraryClause, never user input
		args...,
	)
	if err != nil {
		return 0, fmt.Errorf("queue: recheck deferred: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: recheck deferred rows affected: %w", err)
	}
	return n, nil
}

// CountRecheckDeferred returns the number of 'deferred' rows that
// RecheckDeferred would revive, without writing. Intended for dry-run output.
func (q *DBQueue) CountRecheckDeferred(ctx context.Context, libraryID *int64) (int64, error) {
	libClause, libArgs := recheckLibraryClause(libraryID)
	args := append([]any(nil), libArgs...)
	var count int64
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE status = 'deferred'`+libClause, //nolint:gosec // G202: libClause is a hardcoded constant from recheckLibraryClause, never user input
		args...,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("queue: count recheck deferred: %w", err)
	}
	return count, nil
}

// RecheckRetired revives work_queue rows that were permanently retired after
// hitting the miss-attempt cap. A retired row is identified by
// status='done' AND last_error = missLimitReachedError (the sentinel written by
// RetireMiss). Revival reverses RetireMiss's terminal writeback:
//   - work_queue: status='deferred', priority=-100, next_attempt_at=now,
//     last_error=”, completed_at=NULL. miss_count and providers_version are
//     left unchanged.
//   - scan_results: rows linked via work_queue_scan_results whose status='done'
//     are reset to 'pending' so the scan layer does not strand the track.
//
// When libraryID is non-nil the scan_results writeback is additionally scoped to
// that library. A deduped work_queue row can link to scan_results in several
// libraries (the row is collapsed on artist_key/title_key), so reviving a row
// shared between libraries X and Y under `--library X` must not flip Y's
// scan_results back to 'pending'. Reviving the shared work_queue row itself is
// correct (re-fetching the deduped track once serves every linked library), and
// Y's scan_result stays 'done' until the row reprocesses, at which point the
// completion writeback re-confirms it.
//
// The scan_results guard is WHERE status='done' (not a literal mirror of
// RetireMiss, which writes 'done' WHERE status != 'done'). This is intentional:
// RetireMiss flips every linked scan_result to 'done', so at revival time they
// are all 'done'; the guard reverts exactly those rows and avoids clobbering a
// scan_result a future code path may legitimately leave in another state.
//
// Both mutations run in one transaction. When libraryID is non-nil only rows
// linked to that library are revived.
//
// Enqueue dedup safety: after revival the work_queue row is status='deferred'.
// A subsequent scan-priority Enqueue for the same artist/title will hit the
// ON CONFLICT(artist_key, title_key) path and preserve the 'deferred' status
// (the upsert keeps work_queue.status for deferred/done/processing rows), so
// no duplicate work_queue row is created alongside the revived one.
func (q *DBQueue) RecheckRetired(ctx context.Context, libraryID *int64) (int64, error) {
	now := formatTime(q.now())
	libClause, libArgs := recheckLibraryClause(libraryID)

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("queue: begin recheck retired tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Collect the IDs of retired rows we are about to revive so the
	// scan_results writeback can target exactly those rows.
	selectArgs := append([]any{missLimitReachedError}, libArgs...)
	idRows, err := tx.QueryContext(ctx,
		`SELECT id FROM work_queue WHERE status = 'done' AND last_error = ?`+libClause, //nolint:gosec // G202: libClause is a hardcoded constant from recheckLibraryClause, never user input
		selectArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("queue: recheck retired select ids: %w", err)
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			_ = idRows.Close()
			return 0, fmt.Errorf("queue: recheck retired scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := idRows.Err(); err != nil {
		_ = idRows.Close()
		return 0, fmt.Errorf("queue: recheck retired id rows: %w", err)
	}
	_ = idRows.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	// Revive the work_queue rows. The query string is built from hardcoded SQL
	// fragments only; libClause comes from recheckLibraryClause which returns a
	// fixed constant (never user input), so the concatenation is safe.
	const retireUpdateBase = `UPDATE work_queue
         SET status = 'deferred',
             priority = -100,
             next_attempt_at = ?,
             last_error = '',
             completed_at = NULL
         WHERE status = 'done' AND last_error = ?`
	updateArgs := append([]any{now, missLimitReachedError}, libArgs...)
	res, err := tx.ExecContext(ctx,
		retireUpdateBase+libClause, //nolint:gosec // G202: libClause is a fixed constant from recheckLibraryClause, not user input
		updateArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("queue: recheck retired update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: recheck retired rows affected: %w", err)
	}

	// Reset the linked scan_results rows from 'done' back to 'pending'.
	// Target only the rows linked to the work_queue IDs we just revived, via
	// the work_queue_scan_results junction. This mirrors RetireMiss's writeback.
	// When scoped to a library, restrict the writeback to that library's
	// scan_results so a shared (deduped) row does not strand-revive another
	// library's terminal scan state. The libClause-bound subquery is reused via
	// a fixed AND library_id = ? predicate (never user-built SQL).
	writebackQuery := `UPDATE scan_results
             SET status = 'pending'
             WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
               AND status = 'done'`
	if libraryID != nil {
		writebackQuery += ` AND library_id = ?`
	}
	for _, id := range ids {
		writebackArgs := []any{id}
		if libraryID != nil {
			writebackArgs = append(writebackArgs, *libraryID)
		}
		if _, err := tx.ExecContext(ctx, writebackQuery, writebackArgs...); err != nil {
			return 0, fmt.Errorf("queue: recheck retired scan_results writeback for row %d: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("queue: commit recheck retired tx: %w", err)
	}
	return n, nil
}

// CountRecheckRetired returns the number of retired rows that RecheckRetired
// would revive, without writing. Intended for dry-run output.
func (q *DBQueue) CountRecheckRetired(ctx context.Context, libraryID *int64) (int64, error) {
	libClause, libArgs := recheckLibraryClause(libraryID)
	args := append([]any{missLimitReachedError}, libArgs...)
	var count int64
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE status = 'done' AND last_error = ?`+libClause, //nolint:gosec // G202: libClause is a hardcoded constant from recheckLibraryClause, never user input
		args...,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("queue: count recheck retired: %w", err)
	}
	return count, nil
}

// ListFilter narrows the rows returned by List.
type ListFilter struct {
	// Status optionally restricts results to a single status value (e.g.
	// "pending", "processing", "failed", "done"). Empty means no filter.
	Status string
	// Limit caps the number of rows returned. Zero or negative means no cap.
	Limit int
}

// List returns work items ordered by priority desc, created_at asc, id asc,
// optionally filtered by status and capped by limit.
func (q *DBQueue) List(ctx context.Context, filter ListFilter) (items []WorkItem, retErr error) {
	const baseQuery = `SELECT id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                       miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id
                       FROM work_queue`
	const orderClause = ` ORDER BY priority DESC, created_at ASC, id ASC`
	const limitClause = ` LIMIT ?`
	var args []any
	var query string
	switch {
	case filter.Status != "" && filter.Limit > 0:
		query = baseQuery + ` WHERE status = ?` + orderClause + limitClause
		args = append(args, filter.Status, filter.Limit)
	case filter.Status != "":
		query = baseQuery + ` WHERE status = ?` + orderClause
		args = append(args, filter.Status)
	case filter.Limit > 0:
		query = baseQuery + orderClause + limitClause
		args = append(args, filter.Limit)
	default:
		query = baseQuery + orderClause
	}

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: list: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close list rows: %w", err)
		}
	}()

	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list scan: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list rows: %w", err)
	}
	return items, nil
}

// Retry resets a failed work item back to pending so the worker picks it up on
// the next dequeue. attempts is reset to zero, last_error is cleared, and
// next_attempt_at is set to now. Retry returns ErrNotRetryable when the row's
// current status is not "failed", which avoids racing the worker on
// processing rows or undoing a successful completion.
func (q *DBQueue) Retry(ctx context.Context, id int64) (WorkItem, error) {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: begin retry tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`UPDATE work_queue
         SET status = 'pending',
             attempts = 0,
             next_attempt_at = ?,
             last_error = ''
         WHERE id = ?
           AND status = 'failed'
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		now,
		id,
	)
	item, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Either the row does not exist, or its status is not 'failed'. The
		// caller cannot tell those apart here; existence is checked separately
		// so the user gets a clear error message either way.
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM work_queue WHERE id = ?`, id,
		).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return WorkItem{}, sql.ErrNoRows
			}
			return WorkItem{}, fmt.Errorf("queue: retry existence check: %w", err)
		}
		return WorkItem{}, ErrNotRetryable
	}
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: retry: %w", err)
	}
	// Reset every linked scan_results row so `scan results` reflects the
	// retried state. Skip rows already in 'pending' or 'done' so we never
	// overwrite a terminal outcome.
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'pending'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status = 'processing'`,
		id,
	); err != nil {
		return WorkItem{}, fmt.Errorf("queue: retry scan_results reset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkItem{}, fmt.Errorf("queue: commit retry tx: %w", err)
	}
	return item, nil
}

// ClearDone removes every work_queue row whose status is "done" and returns
// the number of rows deleted.
func (q *DBQueue) ClearDone(ctx context.Context) (int64, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM work_queue WHERE status = 'done'`,
	)
	if err != nil {
		return 0, fmt.Errorf("queue: clear done: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: clear done rows affected: %w", err)
	}
	return n, nil
}

// CountDone returns the number of work_queue rows whose status is "done".
// It is useful for reporting what ClearDone would delete without actually
// deleting anything.
func (q *DBQueue) CountDone(ctx context.Context) (int64, error) {
	var n int64
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE status = 'done'`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count done: %w", err)
	}
	return n, nil
}

// CountByStatus returns the number of work_queue rows grouped by status.
// Statuses with no rows are omitted from the map.
func (q *DBQueue) CountByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM work_queue GROUP BY status`,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: count by status: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable
	counts := make(map[string]int64)
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("queue: scan status count: %w", err)
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: count by status rows: %w", err)
	}
	return counts, nil
}

// CancelByLibrary rebuilds or deletes pending/failed/deferred work_queue rows whose
// output_paths derive from libraryID. Each affected row's output_paths JSON is
// filtered to retain only entries that appear in scan_results from libraries
// other than libraryID. Rows whose filtered list is empty are deleted; the
// rest are updated in place. Processing and done rows are left alone so the
// worker is not raced and historical writes are preserved.
//
// Returns the count of rows deleted and rows updated. Caller is expected to
// invoke this before scan.ClearByLibrary so the work_queue_scan_results
// junction is still populated when the operation runs.
func (q *DBQueue) CancelByLibrary(ctx context.Context, libraryID int64) (deleted int64, updated int64, retErr error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("queue: begin cancel-by-library tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleted, updated, err = cancelByLibrary(ctx, tx, libraryID, false)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("queue: commit cancel-by-library tx: %w", err)
	}
	return deleted, updated, nil
}

// CancelByLibraryTx runs the same logic as CancelByLibrary inside a caller-
// supplied transaction so the queue mutation can be committed atomically with
// other writes (e.g. scan_results delete). The caller owns Begin and Commit.
func (q *DBQueue) CancelByLibraryTx(ctx context.Context, tx *sql.Tx, libraryID int64) (deleted int64, updated int64, retErr error) {
	return cancelByLibrary(ctx, tx, libraryID, false)
}

// CountCancelByLibrary returns the (deleted, updated) projection that
// CancelByLibrary would produce, without writing. Intended for dry-run output.
func (q *DBQueue) CountCancelByLibrary(ctx context.Context, libraryID int64) (deleted int64, updated int64, retErr error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("queue: begin count-cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return cancelByLibrary(ctx, tx, libraryID, true)
}

func cancelByLibrary(ctx context.Context, tx *sql.Tx, libraryID int64, dryRun bool) (int64, int64, error) {
	type candidate struct {
		id          int64
		outputPaths string
	}
	candidateRows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT wq.id, wq.output_paths
         FROM work_queue wq
         JOIN work_queue_scan_results j ON j.work_queue_id = wq.id
         JOIN scan_results sr ON sr.id = j.scan_result_id
         WHERE sr.library_id = ?
           AND wq.status IN ('pending', 'failed', 'deferred')
         ORDER BY wq.id ASC`,
		libraryID,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("queue: cancel-by-library candidates: %w", err)
	}
	var candidates []candidate
	for candidateRows.Next() {
		var c candidate
		if err := candidateRows.Scan(&c.id, &c.outputPaths); err != nil {
			_ = candidateRows.Close()
			return 0, 0, fmt.Errorf("queue: scan cancel candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := candidateRows.Err(); err != nil {
		_ = candidateRows.Close()
		return 0, 0, fmt.Errorf("queue: cancel candidates rows: %w", err)
	}
	if err := candidateRows.Close(); err != nil {
		return 0, 0, fmt.Errorf("queue: close cancel candidates: %w", err)
	}

	var deleted, updated int64
	for _, c := range candidates {
		keep, err := keepSetForRow(ctx, tx, c.id, libraryID)
		if err != nil {
			return 0, 0, err
		}
		var paths []models.OutputPath
		if c.outputPaths != "" {
			if err := json.Unmarshal([]byte(c.outputPaths), &paths); err != nil {
				return 0, 0, fmt.Errorf("queue: unmarshal output_paths for row %d: %w", c.id, err)
			}
		}
		filtered := paths[:0:0]
		for _, p := range paths {
			if _, ok := keep[outputPathKey(p)]; ok {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			if dryRun {
				deleted++
				continue
			}
			// The status guard here is belt-and-suspenders: candidates were
			// selected with status IN ('pending', 'failed', 'deferred'), but SQLite WAL
			// admits a small window between candidate selection and this
			// per-row write during which the worker could move the row to
			// 'processing'. A 0 affected-rows result means the row moved on
			// and we skip counting it without raising an error.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM work_queue
                 WHERE id = ?
                   AND status IN ('pending', 'failed', 'deferred')`,
				c.id,
			)
			if err != nil {
				return 0, 0, fmt.Errorf("queue: cancel delete row %d: %w", c.id, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, 0, fmt.Errorf("queue: cancel delete rows affected %d: %w", c.id, err)
			}
			if n == 1 {
				deleted++
			}
			continue
		}
		if len(filtered) == len(paths) {
			// Defensive: candidate matched but every output_path entry was
			// also present in a non-X scan_result. Nothing to change.
			continue
		}
		if dryRun {
			updated++
			continue
		}
		newJSON, err := json.Marshal(filtered)
		if err != nil {
			return 0, 0, fmt.Errorf("queue: marshal filtered output_paths for row %d: %w", c.id, err)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE work_queue
             SET output_paths = ?
             WHERE id = ?
               AND status IN ('pending', 'failed', 'deferred')`,
			string(newJSON), c.id,
		)
		if err != nil {
			return 0, 0, fmt.Errorf("queue: cancel update row %d: %w", c.id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, fmt.Errorf("queue: cancel update rows affected %d: %w", c.id, err)
		}
		if n == 1 {
			updated++
		}
	}
	return deleted, updated, nil
}

func keepSetForRow(ctx context.Context, tx *sql.Tx, workQueueID int64, libraryID int64) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT sr.outdir, sr.filename
         FROM scan_results sr
         JOIN work_queue_scan_results j ON j.scan_result_id = sr.id
         WHERE j.work_queue_id = ?
           AND sr.library_id != ?`,
		workQueueID, libraryID,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: keep-set query row %d: %w", workQueueID, err)
	}
	defer func() { _ = rows.Close() }()

	keep := make(map[string]struct{})
	for rows.Next() {
		var outdir, filename string
		if err := rows.Scan(&outdir, &filename); err != nil {
			return nil, fmt.Errorf("queue: scan keep-set row %d: %w", workQueueID, err)
		}
		keep[outputPathKey(models.OutputPath{Outdir: outdir, Filename: filename})] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: keep-set rows %d: %w", workQueueID, err)
	}
	return keep, nil
}

func outputPathKey(p models.OutputPath) string {
	return p.Outdir + "\x00" + p.Filename
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkItem(row rowScanner) (WorkItem, error) {
	var item WorkItem
	var nextAttemptAt, createdAt, updatedAt, outputPaths string
	var completedAt sql.NullString
	var scanResultID sql.NullInt64
	err := row.Scan(
		&item.ID,
		&item.Inputs.Track.ArtistName,
		&item.Inputs.Track.TrackName,
		&item.Inputs.Track.AlbumName,
		&item.Inputs.Track.AlbumArtist,
		&item.Inputs.Outdir,
		&item.Inputs.Filename,
		&item.Inputs.SourcePath,
		&item.Status,
		&item.Priority,
		&item.Attempts,
		&item.MissCount,
		&item.ProvidersVersion,
		&nextAttemptAt,
		&item.LastError,
		&createdAt,
		&updatedAt,
		&completedAt,
		&outputPaths,
		&scanResultID,
	)
	if err != nil {
		return WorkItem{}, err
	}
	if scanResultID.Valid {
		item.Inputs.ScanResultID = scanResultID.Int64
	}
	item.NextAttemptAt, err = parseTime(nextAttemptAt)
	if err != nil {
		return WorkItem{}, err
	}
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return WorkItem{}, err
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return WorkItem{}, err
	}
	if completedAt.Valid {
		t, err := parseTime(completedAt.String)
		if err != nil {
			return WorkItem{}, err
		}
		item.CompletedAt = &t
	}
	item.Inputs.OutputPaths, err = unmarshalOutputPaths(outputPaths, item.Inputs.Outdir, item.Inputs.Filename)
	if err != nil {
		return WorkItem{}, err
	}
	return item, nil
}

func marshalOutputPaths(inputs models.Inputs) (string, error) {
	paths := inputs.OutputPaths
	if len(paths) == 0 {
		paths = []models.OutputPath{{
			Outdir:   inputs.Outdir,
			Filename: inputs.Filename,
		}}
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return "", fmt.Errorf("queue: marshal output paths: %w", err)
	}
	return string(b), nil
}

func unmarshalOutputPaths(s string, outdir string, filename string) ([]models.OutputPath, error) {
	if s == "" {
		return []models.OutputPath{{
			Outdir:   outdir,
			Filename: filename,
		}}, nil
	}
	var paths []models.OutputPath
	if err := json.Unmarshal([]byte(s), &paths); err != nil {
		return nil, fmt.Errorf("queue: unmarshal output paths: %w", err)
	}
	if len(paths) == 0 {
		paths = append(paths, models.OutputPath{Outdir: outdir, Filename: filename})
	}
	return paths, nil
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func requireAffected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", op, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
