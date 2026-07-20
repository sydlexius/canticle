package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/backoff"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
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
	// ProvidersVersion is the active provider-set generation (providers.Generation)
	// stamped onto the row at enqueue time. It is written only on the initial
	// insert; the ON CONFLICT refresh and the Defer/RecheckDeferred paths leave it
	// unchanged, so a row keeps the generation it was first enqueued under. The
	// worker compares it against the current generation to invalidate cached
	// results when the provider set changes. 0 means "no generation configured".
	ProvidersVersion int
	// DetectInstrumental is the per-item instrumental-detection decision stamped
	// onto the row at enqueue time (resolved CLI > per-library > global). Written
	// only on the initial insert; the ON CONFLICT refresh and Defer/RecheckDeferred
	// paths leave it unchanged. nil (NULL) means "no decision stamped" -> the worker
	// falls back to the global config default, which covers all pre-existing rows.
	DetectInstrumental *bool
	NextAttemptAt      time.Time
	LastError          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CompletedAt        *time.Time
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
	// batchSize is the shuffled lookahead buffer size (#571). When randomized and
	// > 0, Dequeue draws this many eligible rows at random once per batch and
	// serves them in a stamped order; <= 0 restores the per-item random path. Set
	// via SetBatchSize from the commands layer after config is loaded.
	batchSize int
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
		batchSize:   defaultBatchSize,
	}
}

// defaultBatchSize keeps NewDBQueue self-consistent when a caller never calls
// SetBatchSize. It mirrors config.queueBatchSizeDefault; the commands layer
// wires the configured value over it at startup.
const defaultBatchSize = 10

// SetBatchSize sets the shuffled lookahead buffer size (#571). It lets callers
// apply the configured queue.batch_size / MXLRC_QUEUE_BATCH_SIZE setting without
// changing the NewDBQueue call sites. A value <= 0 disables batching and
// restores the per-item random path.
func (q *DBQueue) SetBatchSize(n int) {
	q.batchSize = n
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
             artist, title, album, album_artist, artist_key, title_key, outdir, filename, source_path, output_paths, scan_result_id, status, priority, providers_version, detect_instrumental, next_attempt_at
         )
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
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
		nullableBool(inputs.DetectInstrumental),
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
         SET status = 'processing',
             prev_status = status
         WHERE id = (
             SELECT id
             FROM work_queue
             WHERE status IN ('pending', 'failed', 'deferred')
               AND next_attempt_at <= ?
             ORDER BY priority DESC, RANDOM()
             LIMIT 1
         )
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`

// dequeueDeterministicSQL claims the next ready item in stable FIFO order within
// a priority tier (created_at, then id).
const dequeueDeterministicSQL = `UPDATE work_queue
         SET status = 'processing',
             prev_status = status
         WHERE id = (
             SELECT id
             FROM work_queue
             WHERE status IN ('pending', 'failed', 'deferred')
               AND next_attempt_at <= ?
             ORDER BY priority DESC, created_at ASC, id ASC
             LIMIT 1
         )
         RETURNING id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`

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

// SettleOutcome explains what a guarded settle actually did. It is an enum rather
// than a bool because "the UPDATE matched no rows" is ambiguous, and the ambiguity
// is dangerous: the caller has already written a marker to disk, and whether that
// marker must be removed or preserved depends on WHY the row was not settled.
type SettleOutcome int

const (
	// SettleFailed means the operation errored; the outcome is unknown.
	SettleFailed SettleOutcome = iota
	// Settled means this call wrote the verdict and completed the row.
	Settled
	// SettleClaimed means a serve-mode worker owns the row now (it is no longer
	// deferred and carries no instrumental verdict). Nothing was written, so the
	// caller's marker is orphaned and must be removed.
	SettleClaimed
	// SettleAlreadyInstrumental means a PEER BACKFILL settled the row first with the
	// same verdict. The marker on disk is correct and MUST NOT be removed --
	// deleting it would destroy a valid result the peer just established.
	SettleAlreadyInstrumental
	// SettleRowGone means the row no longer exists (e.g. pruned mid-run). Nothing
	// was written and no marker is warranted.
	SettleRowGone
)

// SettleInstrumental records an instrumental verdict on a DEFERRED row and
// completes it, in ONE transaction: telemetry, instrumental_result=1,
// outcome_type='instrumental', status='done', and the scan_results writeback all
// commit together or not at all.
//
// It is a single guarded statement rather than the worker's
// stamp-then-stamp-then-complete sequence because the backfill does NOT own its
// rows. The worker holds its row in 'processing' for the whole write, so nothing
// can race it. The backfill leaves rows 'deferred' while the (slow) detector runs,
// and Dequeue selects status IN ('pending','failed','deferred') -- so a serve-mode
// worker can and will claim one mid-classification. Guarding only the final step
// would let the earlier stamps land on a row the worker already owns, leaving it
// stamped instrumental while the worker goes on to complete it with real lyrics.
//
// The outcome is stateful, not a bool, because zero affected rows only proves the
// row is no longer deferred -- it does NOT prove a worker claimed it. A PEER
// BACKFILL may have settled it first, and the two demand opposite actions: a
// worker claim means this run's marker is orphaned and must be removed, while a
// peer settle means the marker on disk is CORRECT and deleting it would destroy a
// valid result. So on a no-op the row is re-read inside the same transaction and
// classified.
func (q *DBQueue) SettleInstrumental(ctx context.Context, id int64, tel InstrumentalTelemetry) (outcome SettleOutcome, retErr error) {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: begin settle instrumental tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET instrumental_result = 1,
             music_sum = ?,
             vocal_peak = ?,
             speech_mean = ?,
             vocal_class = ?,
             detector_version = ?,
             outcome_type = 'instrumental',
             status = 'done',
             completed_at = ?,
             last_error = ''
         WHERE id = ?
           AND status = 'deferred'`,
		tel.MusicSum, tel.VocalPeak, tel.SpeechMean, tel.VocalClass, tel.DetectorVersion,
		now, id,
	)
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle instrumental: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle instrumental rows affected: %w", err)
	}
	if n == 0 {
		return q.classifyNoSettle(ctx, tx, id)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'done'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status != 'done'`,
		id,
	); err != nil {
		return SettleFailed, fmt.Errorf("queue: settle instrumental scan_results writeback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SettleFailed, fmt.Errorf("queue: commit settle instrumental tx: %w", err)
	}
	return Settled, nil
}

// classifyNoSettle explains why the guarded settle UPDATE matched nothing. It
// reads inside the caller's transaction, so the answer is consistent with the
// UPDATE that just ran rather than a later, separately-racing read.
func (q *DBQueue) classifyNoSettle(ctx context.Context, tx *sql.Tx, id int64) (SettleOutcome, error) {
	var status string
	var result *int
	err := tx.QueryRowContext(ctx,
		`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, id,
	).Scan(&status, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return SettleRowGone, nil
	}
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: classify no-settle for %d: %w", id, err)
	}
	// A peer backfill got there first: the row already carries the same verdict this
	// run was about to write, and the marker on disk is ITS marker and is correct.
	if status == "done" && result != nil && *result == 1 {
		return SettleAlreadyInstrumental, nil
	}
	return SettleClaimed, nil
}

// StampUnclassifiedMiss records a NOT-instrumental verdict on a DEFERRED row,
// leaving it deferred: a provider may still find lyrics for it. Guarded on
// 'deferred' for the same reason as SettleInstrumental -- it must not stamp a row
// a worker has since claimed and is writing its own verdict onto.
//
// Reports stamped=false when the row is no longer deferred.
func (q *DBQueue) StampUnclassifiedMiss(ctx context.Context, id int64, tel InstrumentalTelemetry) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET instrumental_result = 0,
             music_sum = ?,
             vocal_peak = ?,
             speech_mean = ?,
             vocal_class = ?,
             detector_version = ?
         WHERE id = ?
           AND status = 'deferred'`,
		tel.MusicSum, tel.VocalPeak, tel.SpeechMean, tel.VocalClass, tel.DetectorVersion, id,
	)
	if err != nil {
		return false, fmt.Errorf("queue: stamp unclassified miss: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: stamp unclassified miss rows affected: %w", err)
	}
	return n > 0, nil
}

// Release returns a processing item to the pool it was claimed from, without
// recording a failure. Used when the worker dequeued the item but cannot
// process it for a reason that should not count against the row's retry budget
// (e.g. the global rate-limit circuit breaker tripped). Attempts and
// next_attempt_at are left untouched so the row is immediately eligible for the
// next dequeue.
//
// The restored status is prev_status, which Dequeue stamps with the row's
// pre-claim status. A throttle release is therefore a true no-op round-trip: a
// deferred row goes back to 'deferred', a failed row back to 'failed'. Forcing
// every release to 'pending' (the pre-#569 behavior) stranded deferred rows in
// status='pending' with the PriorityMiss deprioritization still applied, where
// they sorted with the deferred backlog instead of as fresh work and were
// invisible to RecheckDeferred and `queue deferred`, both being scoped
// WHERE status = 'deferred'.
//
// last_error is cleared only when the row returns to 'pending'. A row restored
// to 'failed' keeps its reason: a throttle is not the song's fault, but the
// row stays in the failures view, and wiping the reason there would strand it
// as an "unknown" bucket in CountFailuresByReason with its cause destroyed.
//
// Dequeue is the sole writer of status='processing' and always re-stamps
// prev_status as it claims, so a stale prev_status from an earlier claim can
// never be consumed. Any future path that sets 'processing' directly must
// stamp prev_status too.
//
// An empty prev_status (a row claimed before migration 030 added the column)
// falls back to 'pending', preserving the historical behavior.
func (q *DBQueue) Release(ctx context.Context, id int64) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET status = CASE
                 WHEN prev_status = '' THEN 'pending'
                 ELSE prev_status
             END,
             last_error = CASE
                 WHEN prev_status IN ('', 'pending') THEN ''
                 ELSE last_error
             END
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
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
		nextAttempts,
		nextAttemptAt,
		lastError,
		id,
	)
	item, err := scanWorkItem(row)
	if err != nil {
		return WorkItem{}, fmt.Errorf("queue: fail: %w", err)
	}

	// Write the failure back to the linked scan_results rows, mirroring Complete's
	// and RetireMiss's writeback, so a failed item does not strand the scan ledger
	// in 'processing' (#454). Scoped to 'processing' so a retryable failure only
	// clears the wedge and never downgrades a row already marked 'done'.
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'failed'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status = 'processing'`,
		id,
	); err != nil {
		return WorkItem{}, fmt.Errorf("queue: fail scan_results writeback: %w", err)
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
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
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
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
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
                       miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id
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
                   miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id`,
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
	// retried state. Reset both 'processing' rows and the 'failed' rows that
	// Fail now writes back (#454); skip 'pending'/'done' so we never overwrite a
	// terminal outcome (both are non-retryable-here states).
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
         SET status = 'pending'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status IN ('processing', 'failed')`,
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

// CountFailuresByReason returns the count of work_queue rows with status
// 'failed' grouped by last_error. Rows whose last_error is empty are grouped
// under "unknown". Deferred rows (benign misses) are excluded because they are
// not errors. Used by the GET /metrics endpoint.
func (q *DBQueue) CountFailuresByReason(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(last_error, ''), 'unknown') AS reason, COUNT(*)
         FROM work_queue
         WHERE status = 'failed'
         GROUP BY reason`,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: count failures by reason: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable
	counts := make(map[string]int64)
	for rows.Next() {
		var reason string
		var n int64
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, fmt.Errorf("queue: scan failure reason count: %w", err)
		}
		counts[reason] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: count failures by reason rows: %w", err)
	}
	return counts, nil
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

// RecordProviderHit atomically increments the hit counter for the named provider
// lane. If no row exists for lane yet it is created with hits=1, misses=0.
func (q *DBQueue) RecordProviderHit(ctx context.Context, lane string) error {
	if lane == "" {
		return nil
	}
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO provider_outcomes(lane, hits, misses) VALUES(?, 1, 0)
         ON CONFLICT(lane) DO UPDATE SET hits = hits + 1`,
		lane,
	)
	if err != nil {
		return fmt.Errorf("queue: record provider hit for %q: %w", lane, err)
	}
	return nil
}

// RecordProviderMiss atomically increments the miss counter for the named
// provider lane. If no row exists for lane yet it is created with hits=0,
// misses=1.
func (q *DBQueue) RecordProviderMiss(ctx context.Context, lane string) error {
	if lane == "" {
		return nil
	}
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO provider_outcomes(lane, hits, misses) VALUES(?, 0, 1)
         ON CONFLICT(lane) DO UPDATE SET misses = misses + 1`,
		lane,
	)
	if err != nil {
		return fmt.Errorf("queue: record provider miss for %q: %w", lane, err)
	}
	return nil
}

// RecordLaneAttempts persists the per-track, per-lane attempt outcomes for one
// work_queue row into lane_attempts (migration 022) for a true per-track
// hit-rate (issue #282). attempts holds one entry per ATTEMPTED lane: Hit true
// for the lane that served the track, false for every other attempted lane
// (including a lane that lost to a later winner). An empty attempts slice is a
// no-op. The whole batch is written in one transaction so a row's attempts are
// all-or-nothing. UNIQUE(queue_id, lane) makes re-fetches idempotent: an
// --upgrade re-run upserts the latest outcome (and refreshes attempted_at)
// rather than violating the constraint.
func (q *DBQueue) RecordLaneAttempts(ctx context.Context, queueID int64, attempts []models.LaneAttempt) error {
	if len(attempts) == 0 {
		return nil
	}
	at := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queue: record lane attempts begin for id %d: %w", queueID, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, a := range attempts {
		if a.Lane == "" {
			continue
		}
		hit := 0
		if a.Hit {
			hit = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO lane_attempts(queue_id, lane, hit, attempted_at) VALUES(?, ?, ?, ?)
             ON CONFLICT(queue_id, lane) DO UPDATE SET hit = excluded.hit, attempted_at = excluded.attempted_at`,
			queueID, a.Lane, hit, at,
		); err != nil {
			return fmt.Errorf("queue: record lane attempt for id %d lane %q: %w", queueID, a.Lane, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: record lane attempts commit for id %d: %w", queueID, err)
	}
	return nil
}

// SetProviderLane stamps the winning provider lane name onto a work_queue row.
// Call at completion time (before Complete) so the row permanently records which
// provider served it. A NULL provider_lane means not-yet-completed, retired
// without a match, or a row that predates this column.
func (q *DBQueue) SetProviderLane(ctx context.Context, id int64, lane string) error {
	if lane == "" {
		return nil
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET provider_lane = ? WHERE id = ?`,
		lane, id,
	)
	if err != nil {
		return fmt.Errorf("queue: set provider lane for id %d: %w", id, err)
	}
	return nil
}

// InstrumentalTelemetry carries the five score fields from an audio detection
// run. All fields are set when detection ran; the zero value (empty struct) is
// used on the not-ran path, keeping the five DB columns NULL (pre-telemetry /
// detection-disabled semantics are preserved by the caller passing no telemetry
// struct in that case).
type InstrumentalTelemetry struct {
	// MusicSum is the summed instrumental-class MEAN probability (music gate score).
	MusicSum float64
	// VocalPeak is the peak (max-over-frames) of the winning vocal class (sung-vocal gate score).
	VocalPeak float64
	// SpeechMean is the summed frame-MEAN of speech classes (speech gate score).
	SpeechMean float64
	// VocalClass is the name of the vocal class that produced VocalPeak. Empty
	// when no vocal class scored or when the sidecar returned no max map.
	VocalClass string
	// DetectorVersion is the app version string at detection time (internal/version.Version).
	DetectorVersion string
}

// SetInstrumentalResult stamps the audio-detection outcome and telemetry onto a
// work_queue row in a single UPDATE. result=1 means the audio detector confirmed
// instrumental; result=0 means the detector ran but the track is not instrumental.
// The five telemetry fields are written atomically with instrumental_result so
// each persisted row records the scores that produced the decision. Call before
// Complete while the row is still in 'processing' status (the UPDATE is a no-op
// on any other status, which is benign).
func (q *DBQueue) SetInstrumentalResult(ctx context.Context, id int64, result int, tel InstrumentalTelemetry) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET instrumental_result = ?,
             music_sum = ?,
             vocal_peak = ?,
             speech_mean = ?,
             vocal_class = ?,
             detector_version = ?
         WHERE id = ?`,
		result,
		tel.MusicSum,
		tel.VocalPeak,
		tel.SpeechMean,
		tel.VocalClass,
		tel.DetectorVersion,
		id,
	)
	if err != nil {
		return fmt.Errorf("queue: set instrumental result for id %d: %w", id, err)
	}
	return nil
}

// SetOutcomeType records what was actually written for a work_queue row
// ("synced" | "unsynced" | "instrumental") so reports classify by the real
// outcome instead of the enqueue-time output_paths filename, which is always
// the planned .lrc and is never updated at completion (#379). The worker calls
// this before Complete while the row is still in 'processing'; the UPDATE keys
// on id alone (no status guard, matching SetInstrumentalResult), and is a no-op
// for a missing id, which is benign.
func (q *DBQueue) SetOutcomeType(ctx context.Context, id int64, outcomeType string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET outcome_type = ? WHERE id = ?`,
		outcomeType, id,
	)
	if err != nil {
		return fmt.Errorf("queue: set outcome type for id %d: %w", id, err)
	}
	return nil
}

// ProviderHits returns a map from lane name to cumulative hit count. Lanes that
// have never recorded a hit are omitted.
func (q *DBQueue) ProviderHits(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT lane, hits FROM provider_outcomes WHERE hits > 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: provider hits: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable
	counts := make(map[string]int64)
	for rows.Next() {
		var lane string
		var n int64
		if err := rows.Scan(&lane, &n); err != nil {
			return nil, fmt.Errorf("queue: scan provider hits: %w", err)
		}
		counts[lane] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: provider hits rows: %w", err)
	}
	return counts, nil
}

// ProviderMisses returns a map from lane name to cumulative miss count. Lanes
// that have never recorded a miss are omitted.
func (q *DBQueue) ProviderMisses(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT lane, misses FROM provider_outcomes WHERE misses > 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: provider misses: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable
	counts := make(map[string]int64)
	for rows.Next() {
		var lane string
		var n int64
		if err := rows.Scan(&lane, &n); err != nil {
			return nil, fmt.Errorf("queue: scan provider misses: %w", err)
		}
		counts[lane] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: provider misses rows: %w", err)
	}
	return counts, nil
}

// CountInstrumental returns the number of work_queue rows where the audio
// detector confirmed the track as instrumental (instrumental_result = 1).
func (q *DBQueue) CountInstrumental(ctx context.Context) (int64, error) {
	var n int64
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE instrumental_result = 1`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count instrumental: %w", err)
	}
	return n, nil
}

// Borderline bands for the reconcile telemetry prefilter. A row whose vocal_peak
// sits just above the default vocal gate (detector defaultVocalMaxConfidence =
// 0.03) or whose speech_mean sits just below the default speech gate
// (defaultSpeechMaxConfidence = 0.20) is "borderline": small score drift could
// flip the verdict, so it is worth re-inferring. Bounds are constants so they can
// be tuned in one place; reconcile always re-infers the narrowed set to confirm a
// disagreement before clearing anything.
const (
	reconcileVocalBorderlineLo  = 0.03
	reconcileVocalBorderlineHi  = 0.05
	reconcileSpeechBorderlineLo = 0.15
	reconcileSpeechBorderlineHi = 0.20
)

// instrumentalNarrowedPredicate builds the telemetry-narrowed candidate filter
// appended after "instrumental_result = 1". A flagged row is a reconcile candidate
// when it is borderline (vocal_peak or speech_mean in band), cross-version
// (detector_version differs from currentVersion), or un-scored (any telemetry
// column NULL, e.g. a pre-#404 marker written before the telemetry columns
// existed). currentVersion is the value #404 stamps into detector_version
// (internal/version.Version). The returned clause uses only literal SQL and
// placeholders, so it is safe to concatenate.
func instrumentalNarrowedPredicate(currentVersion string) (clause string, args []any) {
	clause = ` AND (
            (vocal_peak BETWEEN ? AND ?)
            OR (speech_mean BETWEEN ? AND ?)
            OR detector_version IS NULL OR detector_version <> ?
            OR music_sum IS NULL OR vocal_peak IS NULL OR speech_mean IS NULL OR vocal_class IS NULL
        )`
	args = []any{
		reconcileVocalBorderlineLo, reconcileVocalBorderlineHi,
		reconcileSpeechBorderlineLo, reconcileSpeechBorderlineHi,
		currentVersion,
	}
	return clause, args
}

// ListInstrumentalOptions controls ListInstrumental and CountInstrumentalNarrowed.
type ListInstrumentalOptions struct {
	// LibraryID, when non-nil, scopes results to rows linked to that library via
	// the work_queue_scan_results junction.
	LibraryID *int64
	// Limit caps the number of returned rows when > 0.
	Limit int
	// All returns the entire instrumental_result = 1 population, bypassing the
	// telemetry-narrowed prefilter. CurrentVersion is then irrelevant.
	All bool
	// CurrentVersion is the detector version (internal/version.Version) the
	// cross-version prefilter compares stored detector_version against. Required
	// when All is false.
	CurrentVersion string
}

// ListUnclassifiedOptions narrows ListUnclassified.
type ListUnclassifiedOptions struct {
	// LibraryID, when non-nil, scopes results to rows linked to that library via
	// the work_queue_scan_results junction.
	LibraryID *int64
	// Limit caps the number of returned rows when > 0.
	Limit int
	// GlobalDetectDefault resolves rows whose per-item detect_instrumental is NULL,
	// mirroring how the worker resolves it. It MUST be applied here, in SQL, rather
	// than by the caller after the fact: Limit is applied by the database, so an
	// ineligible row filtered in Go has already consumed a slot. Because the
	// ordering is deterministic, the same ineligible rows would fill every capped
	// run and the eligible rows behind them would never be reached -- a permanent
	// starvation rather than a slow drain.
	GlobalDetectDefault bool
}

// detectEligibleClause selects rows whose effective detection decision is ON:
// the per-item stamp when present, else the global default. Mirrors the worker's
// resolution (item.DetectInstrumental ?? global).
func detectEligibleClause(globalDefault bool) (clause string, args []any) {
	// A NULL stamp defers to the global default; when that default is off, no NULL
	// row is eligible at all.
	return " AND (detect_instrumental = 1 OR (detect_instrumental IS NULL AND ?))", []any{globalDefault}
}

// ListUnclassified returns work_queue rows the detector has never scored
// (instrumental_result IS NULL) that are parked on a benign provider miss
// (status = 'deferred') and carry a source path to read.
//
// This is the population ListInstrumental structurally cannot reach: it selects
// instrumental_result = 1 AND status = 'done', so it only re-checks verdicts the
// detector already confirmed, in order to clear false positives. Nothing selects
// the inverse -- rows never classified at all -- which is why a queue drained
// before the detector shipped stays permanently unexamined (#499).
//
// 'deferred' is the operative status: those rows already lost their provider
// round-trip, so classifying them costs no provider request. 'processing' rows are
// excluded (in-flight), as are rows with no source_path (nothing to feed the
// detector). Read-only.
func (q *DBQueue) ListUnclassified(ctx context.Context, opts ListUnclassifiedOptions) (items []WorkItem, retErr error) {
	const baseQuery = `SELECT id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                       miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id
                       FROM work_queue
                       WHERE instrumental_result IS NULL
                         AND status = 'deferred'
                         AND TRIM(COALESCE(source_path, '')) <> ''`
	const orderClause = ` ORDER BY priority DESC, created_at ASC, id ASC`
	query := baseQuery
	var args []any
	// Eligibility BEFORE the limit: an ineligible row filtered in Go has already
	// consumed a slot, and the deterministic ordering would hand the same
	// ineligible rows to every capped run forever.
	detClause, detArgs := detectEligibleClause(opts.GlobalDetectDefault)
	query += detClause
	args = append(args, detArgs...)
	libClause, libArgs := recheckLibraryClause(opts.LibraryID)
	query += libClause
	args = append(args, libArgs...)
	query += orderClause
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := q.db.QueryContext(ctx, query, args...) //nolint:gosec // G202: all concatenated fragments are package constants / recheckLibraryClause's + detectEligibleClause's fixed clauses; never user-built SQL
	if err != nil {
		return nil, fmt.Errorf("queue: list unclassified: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close list unclassified rows: %w", err)
		}
	}()
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list unclassified scan: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list unclassified rows: %w", err)
	}
	return items, nil
}

// CountUnclassified returns the total number of rows ListUnclassified would
// consider, ignoring Limit, so a run can report coverage against the backlog.
// It applies the SAME eligibility rule as ListUnclassified: a total that counted
// rows the run can never classify would overstate the backlog and make a capped
// run's "N left unexamined" a lie.
func (q *DBQueue) CountUnclassified(ctx context.Context, libraryID *int64, globalDetectDefault bool) (int, error) {
	query := `SELECT COUNT(*) FROM work_queue
              WHERE instrumental_result IS NULL
                AND status = 'deferred'
                AND TRIM(COALESCE(source_path, '')) <> ''`
	var args []any
	detClause, detArgs := detectEligibleClause(globalDetectDefault)
	query += detClause
	args = append(args, detArgs...)
	libClause, libArgs := recheckLibraryClause(libraryID)
	query += libClause
	args = append(args, libArgs...)
	var n int
	if err := q.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil { //nolint:gosec // G202: fragments are package constants / recheckLibraryClause's + detectEligibleClause's fixed clauses; never user-built SQL
		return 0, fmt.Errorf("queue: count unclassified: %w", err)
	}
	return n, nil
}

// ListInstrumental returns work_queue rows flagged instrumental
// (instrumental_result = 1), hydrated with full Inputs (SourcePath and
// OutputPaths) so reconcile can locate audio sources and their sidecars. By
// default only the telemetry-narrowed candidate set is returned (borderline /
// cross-version / un-scored rows); set opts.All for the full flagged population.
// Read-only.
func (q *DBQueue) ListInstrumental(ctx context.Context, opts ListInstrumentalOptions) (items []WorkItem, retErr error) {
	// status = 'done' restricts to COMPLETED instrumental rows: the worker stamps
	// instrumental_result = 1 just before Complete, so a still-'processing' row could
	// otherwise be picked up and cleared mid-write.
	const baseQuery = `SELECT id, artist, title, album, album_artist, outdir, filename, source_path, status, priority, attempts,
                       miss_count, providers_version, detect_instrumental, next_attempt_at, last_error, created_at, updated_at, completed_at, output_paths, scan_result_id
                       FROM work_queue WHERE instrumental_result = 1 AND status = 'done'`
	const orderClause = ` ORDER BY priority DESC, created_at ASC, id ASC`
	query := baseQuery
	var args []any
	if !opts.All {
		clause, predArgs := instrumentalNarrowedPredicate(opts.CurrentVersion)
		query += clause
		args = append(args, predArgs...)
	}
	libClause, libArgs := recheckLibraryClause(opts.LibraryID)
	query += libClause
	args = append(args, libArgs...)
	query += orderClause
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := q.db.QueryContext(ctx, query, args...) //nolint:gosec // G202: all concatenated fragments are package constants / recheckLibraryClause's fixed clause; never user-built SQL
	if err != nil {
		return nil, fmt.Errorf("queue: list instrumental: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close list instrumental rows: %w", err)
		}
	}()
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list instrumental scan: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list instrumental rows: %w", err)
	}
	return items, nil
}

// ListVocalGateRejectionsOptions narrows the stamped-not-instrumental listing.
type ListVocalGateRejectionsOptions struct {
	LibraryID *int64
	Limit     int
}

// StampedRejection is one instrumental_result=0 row with its stored telemetry,
// for re-deciding under a recalibrated threshold without re-scanning audio.
type StampedRejection struct {
	ID         int64
	Artist     string
	Title      string
	SourcePath string
	Tel        InstrumentalTelemetry
}

// ListVocalGateRejections returns deferred rows the detector previously marked
// not-instrumental (instrumental_result=0) that carry re-decidable stored
// telemetry (music_sum/vocal_peak/speech_mean non-NULL). Legacy rows missing
// those scores are skipped: they cannot be re-decided from stored scores alone.
//
// The re-decision engine calls detector.Instrumental(music, vocalPeak,
// speechMean, ...) using only these three stored scores, which deliberately
// omits the live detector's maxAvailable/baselineComplete sidecar-completeness
// guards (internal/detector/http.go) -- those guards are not reconstructable
// from the three stored scores alone. A row where the vocal gate could not run
// cleanly (a legacy mean-only sidecar, maxAvailable=false) is stamped with
// vocal_peak=0.0 and an empty vocal_class, which can spuriously satisfy
// detector.Instrumental's vocal-gate check and cause the re-decision engine to
// settle it as instrumental -- a false instrumental marker that suppresses
// real lyric fetches. Excluding empty vocal_class rows drops exactly that
// degraded population: a genuine vocal-gate-buried instrumental always has a
// non-empty vocal_class (the faint vocal class that peaked is why it was
// rejected in the first place), and a complete detection with vocal_peak=0.0
// that still resulted in instrumental_result=0 must have failed the
// music/speech gates rather than the vocal gate, so it would be excluded from
// re-decision anyway -- this filter is harmless to legitimate candidates. This
// preserves the "any doubt resolves to not-instrumental" invariant.
//
// Residual gap: the rarer partial-baseline case (baselineComplete=false with a
// non-empty vocal_class) is NOT fully closed by this guard. Closing it would
// need a durable persisted "gate-complete" telemetry bit, tracked as a
// follow-up.
//
// Read-only.
func (q *DBQueue) ListVocalGateRejections(ctx context.Context, opts ListVocalGateRejectionsOptions) (out []StampedRejection, retErr error) {
	query := `SELECT id, COALESCE(artist,''), COALESCE(title,''), COALESCE(source_path,''),
	                 music_sum, vocal_peak, speech_mean, COALESCE(vocal_class,''), COALESCE(detector_version,'')
	          FROM work_queue
	          WHERE instrumental_result = 0 AND status = 'deferred'
	            AND music_sum IS NOT NULL AND vocal_peak IS NOT NULL AND speech_mean IS NOT NULL
	            AND TRIM(COALESCE(vocal_class,'')) <> ''`
	var args []any
	libClause, libArgs := recheckLibraryClause(opts.LibraryID)
	query += libClause //nolint:gosec // G202: libClause is a package-constant fragment from recheckLibraryClause, never user-built SQL
	args = append(args, libArgs...)
	query += ` ORDER BY id`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := q.db.QueryContext(ctx, query, args...) //nolint:gosec // G202: fragments are package constants / fixed clauses, never user-built SQL
	if err != nil {
		return nil, fmt.Errorf("queue: list vocal-gate rejections: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close list vocal-gate rejections rows: %w", err)
		}
	}()
	for rows.Next() {
		var r StampedRejection
		if err := rows.Scan(&r.ID, &r.Artist, &r.Title, &r.SourcePath,
			&r.Tel.MusicSum, &r.Tel.VocalPeak, &r.Tel.SpeechMean, &r.Tel.VocalClass, &r.Tel.DetectorVersion); err != nil {
			return nil, fmt.Errorf("queue: scan vocal-gate rejection: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list vocal-gate rejections rows: %w", err)
	}
	return out, nil
}

// ListVocalGateConfirmationsOptions narrows the stamped-instrumental listing.
type ListVocalGateConfirmationsOptions struct {
	LibraryID *int64
	Limit     int
}

// ListVocalGateConfirmations returns done rows the detector previously settled
// as instrumental (instrumental_result = 1) that carry re-decidable stored
// telemetry. It is the TIGHTENING-direction counterpart to
// ListVocalGateRejections: those rows sit in a gap of their own, because
// nothing revisits a confirmed positive once the thresholds move DOWN.
//
// Unlike the rejection listing, this one does not need the empty-vocal_class
// filter that drops the degraded mean-only-sidecar population. That filter
// exists because a vocal_peak stamped 0.0 by an incomplete sidecar can
// spuriously SATISFY the vocal gate and settle a false instrumental. Here the
// re-decision only ever acts when a row now FAILS the gate, which requires a
// vocal_peak at or above the threshold -- a degraded 0.0 stamp can never
// trigger a reversal. The filter would be inert, so it is omitted rather than
// cargo-culted.
//
// Read-only.
func (q *DBQueue) ListVocalGateConfirmations(ctx context.Context, opts ListVocalGateConfirmationsOptions) (out []StampedRejection, retErr error) {
	query := `SELECT id, COALESCE(artist,''), COALESCE(title,''), COALESCE(source_path,''),
	                 music_sum, vocal_peak, speech_mean, COALESCE(vocal_class,''), COALESCE(detector_version,'')
	          FROM work_queue
	          WHERE instrumental_result = 1 AND status = 'done'
	            AND music_sum IS NOT NULL AND vocal_peak IS NOT NULL AND speech_mean IS NOT NULL`
	var args []any
	libClause, libArgs := recheckLibraryClause(opts.LibraryID)
	query += libClause //nolint:gosec // reason: G202 -- libClause is a package-constant fragment from recheckLibraryClause, never user-built SQL
	args = append(args, libArgs...)
	query += ` ORDER BY id`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := q.db.QueryContext(ctx, query, args...) //nolint:gosec // reason: G202 -- fragments are package constants / fixed clauses, never user-built SQL
	if err != nil {
		return nil, fmt.Errorf("queue: list vocal-gate confirmations: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close list vocal-gate confirmations rows: %w", err)
		}
	}()
	for rows.Next() {
		var r StampedRejection
		if err := rows.Scan(&r.ID, &r.Artist, &r.Title, &r.SourcePath,
			&r.Tel.MusicSum, &r.Tel.VocalPeak, &r.Tel.SpeechMean, &r.Tel.VocalClass, &r.Tel.DetectorVersion); err != nil {
			return nil, fmt.Errorf("queue: scan vocal-gate confirmation: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list vocal-gate confirmations rows: %w", err)
	}
	return out, nil
}

// ListInstrumentalMarkersOptions narrows ListDetectorInstrumentalMarkers.
type ListInstrumentalMarkersOptions struct {
	LibraryID *int64
	Limit     int
}

// InstrumentalMarkerRow is one detector-written instrumental row with the fields
// needed to locate its on-disk .txt marker and its stored detector version.
type InstrumentalMarkerRow struct {
	ID              int64
	Inputs          models.Inputs
	DetectorVersion string
}

// ListDetectorInstrumentalMarkers returns the detector-written instrumental rows
// (instrumental_result = 1, status = 'done'), each with its resolved output paths
// and stored detector_version, so the marker-provenance backfill (#502) can stamp
// [source:canticle-detector]/[dv:] onto the ones still lacking a header. Provider-
// written instrumentals (outcome_type='instrumental' with instrumental_result
// NULL) are excluded: a bare provider marker is already authoritative to the
// scanner and the DB does not record which provider wrote it.
//
// Read-only.
func (q *DBQueue) ListDetectorInstrumentalMarkers(ctx context.Context, opts ListInstrumentalMarkersOptions) (out []InstrumentalMarkerRow, retErr error) {
	query := `SELECT id, artist, title, outdir, filename, source_path, output_paths, COALESCE(detector_version,'')
	          FROM work_queue
	          WHERE instrumental_result = 1 AND status = 'done'`
	var args []any
	libClause, libArgs := recheckLibraryClause(opts.LibraryID)
	query += libClause //nolint:gosec // G202: recheckLibraryClause returns a fixed parameterized clause, not user-built SQL
	args = append(args, libArgs...)
	query += ` ORDER BY id`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := q.db.QueryContext(ctx, query, args...) //nolint:gosec // G202: fragments are package constants / recheckLibraryClause's fixed clause
	if err != nil {
		return nil, fmt.Errorf("queue: list detector instrumental markers: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close detector instrumental markers rows: %w", err)
		}
	}()
	for rows.Next() {
		var r InstrumentalMarkerRow
		var outputPaths string
		if err := rows.Scan(&r.ID, &r.Inputs.Track.ArtistName, &r.Inputs.Track.TrackName,
			&r.Inputs.Outdir, &r.Inputs.Filename, &r.Inputs.SourcePath, &outputPaths, &r.DetectorVersion); err != nil {
			return nil, fmt.Errorf("queue: scan detector instrumental marker: %w", err)
		}
		r.Inputs.OutputPaths, err = unmarshalOutputPaths(outputPaths, r.Inputs.Outdir, r.Inputs.Filename)
		if err != nil {
			return nil, fmt.Errorf("queue: unmarshal output paths for row %d: %w", r.ID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list detector instrumental markers rows: %w", err)
	}
	return out, nil
}

// CountInstrumentalNarrowed returns how many instrumental_result = 1 rows the
// telemetry-narrowed prefilter would select (the same predicate ListInstrumental
// applies when opts.All is false), so reconcile can report prefiltered-vs-total
// scope without materializing rows.
func (q *DBQueue) CountInstrumentalNarrowed(ctx context.Context, currentVersion string, libraryID *int64) (int64, error) {
	clause, args := instrumentalNarrowedPredicate(currentVersion)
	libClause, libArgs := recheckLibraryClause(libraryID)
	// status = 'done' matches ListInstrumental: count only completed instrumental rows.
	query := `SELECT COUNT(*) FROM work_queue WHERE instrumental_result = 1 AND status = 'done'` + clause + libClause
	allArgs := append(args, libArgs...)
	var n int64
	if err := q.db.QueryRowContext(ctx, query, allArgs...).Scan(&n); err != nil { //nolint:gosec // G202: fragments are package constants / fixed library clause, not user input
		return 0, fmt.Errorf("queue: count instrumental narrowed: %w", err)
	}
	return n, nil
}

// ResetInstrumental clears an instrumental verdict and its telemetry and re-queues
// the row so the running scheduler re-fetches it behind foreground work. Modeled on
// RecheckRetired: in one transaction the row is set to status='deferred',
// priority=-100 (dequeue-eligible but strictly behind priority>=0 foreground work -
// the queue-level starvation guard), with instrumental_result, outcome_type,
// completed_at and all five telemetry columns cleared to NULL, last_error cleared,
// and next_attempt_at = now. Guarded by instrumental_result = 1 AND status = 'done'
// (a still-'processing' row mid-write is left alone), so it is a no-op on any other
// row. Linked scan_results rows are reset to 'pending'. Returns the
// number of work_queue rows affected (0 or 1).
func (q *DBQueue) ResetInstrumental(ctx context.Context, id int64) (int64, error) {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("queue: begin reset instrumental tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET status = 'deferred',
             priority = -100,
             instrumental_result = NULL,
             outcome_type = NULL,
             completed_at = NULL,
             music_sum = NULL,
             vocal_peak = NULL,
             speech_mean = NULL,
             vocal_class = NULL,
             detector_version = NULL,
             last_error = '',
             next_attempt_at = ?
         WHERE id = ? AND instrumental_result = 1 AND status = 'done'`,
		now, id,
	)
	if err != nil {
		return 0, fmt.Errorf("queue: reset instrumental update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: reset instrumental rows affected: %w", err)
	}
	if n == 0 {
		// No-op (row absent or not instrumental): nothing to write back.
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("queue: commit reset instrumental tx: %w", err)
		}
		return 0, nil
	}
	// Reset linked scan_results back to 'pending' so `scan results` reflects the
	// re-queued state, mirroring Retry / RecheckRetired.
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results
             SET status = 'pending'
             WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
               AND status = 'done'`,
		id,
	); err != nil {
		return 0, fmt.Errorf("queue: reset instrumental scan_results writeback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("queue: commit reset instrumental tx: %w", err)
	}
	return n, nil
}

// ResetInstrumentalToUnclassified clears a stamped not-instrumental verdict
// (instrumental_result = 0) back to NULL, without touching status, priority,
// or the stored telemetry columns. Used by instrumentalrecalib when a
// vocal-gate rejection now PASSES the reconfigured thresholds but was scored
// by a detector_version other than the current one: the stale telemetry is
// not trustworthy enough to settle on directly, so the row is dropped back
// to "never classified" and picked up by the next reconcile/backfill pass,
// which re-scans it with the current detector instead.
//
// Guarded on status = 'deferred' AND instrumental_result = 0 -- so a row a
// worker has since claimed, or one concurrently re-stamped to a positive verdict
// or already cleared, is left alone rather than having its verdict clobbered.
// Returns whether the row was reset.
func (q *DBQueue) ResetInstrumentalToUnclassified(ctx context.Context, id int64) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET instrumental_result = NULL
         WHERE id = ?
           AND status = 'deferred'
           AND instrumental_result = 0`,
		id,
	)
	if err != nil {
		return false, fmt.Errorf("queue: reset instrumental to unclassified: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: reset instrumental to unclassified rows affected: %w", err)
	}
	return n > 0, nil
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
	var detectInstrumental sql.NullBool
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
		&detectInstrumental,
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
	if detectInstrumental.Valid {
		b := detectInstrumental.Bool
		item.DetectInstrumental = &b
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

// nullableBool maps a tri-state *bool to a SQL value: nil -> NULL, else the bool.
// Used to stamp the per-item detect_instrumental decision (NULL = no decision).
func nullableBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
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

// UnsettleInstrumental reverses SettleInstrumental for a row a TIGHTENED vocal
// gate now rejects: the settled instrumental verdict is stamped back to
// instrumental_result = 0 and the row returns to 'deferred' so the provider
// lanes retry it.
//
// The stored telemetry columns are deliberately PRESERVED. They are the
// evidence the re-decision was made from, and stamping 0 (rather than NULL)
// records the truthful state -- the detector did score this row, and under the
// current thresholds those scores say not-instrumental. NULL would claim the
// row was never classified and would send it back for a needless re-scan.
//
// scan_results is deliberately NOT reverted, which is the asymmetry with
// SettleInstrumental's 'done' writeback. The scan scheduler enqueues from
// scan_results WHERE status = 'pending' (scan.ListPendingByLibrary), so
// flipping it back would race the scheduler into enqueuing a DUPLICATE
// alongside the work_queue row this method just returned to 'deferred'. The
// work_queue row is the live work item; the scan result stays settled.
//
// priority returns to PriorityScan rather than sinking to PriorityMiss. Matching
// Defer looks like the obvious choice and is wrong here: PriorityMiss exists for
// rows that are LIKELY TO MISS AGAIN, and a reversed instrumental is the
// opposite -- a track with real vocals the detector wrongly suppressed, so it is
// unusually likely to resolve on the next attempt.
//
// The cost of getting this wrong is not theoretical. Dequeue randomizes within a
// priority tier, so sinking a bounded correction into a deferred pool an order of
// magnitude larger makes only (reversed / pool) of each draw a corrected row.
// Measured on a production reversal: 880 rows dropped into a 15,634-row pool at
// ~180 rows/hr drew ~10/hr, clearing half in 2.5 days and trailing to roughly
// three weeks, versus about five hours at PriorityScan. These rows do not starve
// fresh work -- they ARE the corrected work, and the set is finite.
//
// Guarded on status = 'done' AND instrumental_result = 1, so a row settled with
// real lyrics, a row a worker has claimed, or one already reversed is left
// alone rather than reopened. Returns whether the row was reverted.
func (q *DBQueue) UnsettleInstrumental(ctx context.Context, id int64) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE work_queue
         SET instrumental_result = 0,
             outcome_type = NULL,
             status = 'deferred',
             completed_at = NULL,
             priority = ?,
             next_attempt_at = ?,
             last_error = 'instrumental verdict reversed by a tightened vocal gate'
         WHERE id = ?
           AND status = 'done'
           AND instrumental_result = 1`,
		PriorityScan, formatTime(q.now()), id,
	)
	if err != nil {
		return false, fmt.Errorf("queue: unsettle instrumental: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: unsettle instrumental rows affected: %w", err)
	}
	return n > 0, nil
}
