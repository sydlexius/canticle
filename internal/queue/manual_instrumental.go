package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ManualLane is the provider_lane a hand-marked instrumental row carries, so
// the reports lane cell is not blank (#1218).
const ManualLane = "manual"

var (
	// ErrManualInstrumentalInFlight is returned when the row is 'processing':
	// a worker holds it, so the caller retries later.
	ErrManualInstrumentalInFlight = errors.New("queue: row is in flight")
	// ErrManualInstrumentalNotFound is returned when no work_queue row has the id.
	ErrManualInstrumentalNotFound = errors.New("queue: no such work item")
)

// MarkManualInstrumentalTx settles row id as a hand-marked instrumental inside
// the caller's transaction (#1218): done, outcome 'instrumental', lane 'manual'
// with upstream cleared in the same statement, instrumental_result left NULL
// (so every detector query keyed on instrumental_result = 1 excludes it), and
// the lyric-bearing record (tier, word timing, hand edit, timing verdict, upgrade
// trip) cleared. Returns changed=false, nil when the row is already marked (the
// original mark time is kept). A 'processing' row yields
// ErrManualInstrumentalInFlight and nothing is written.
func MarkManualInstrumentalTx(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (changed bool, err error) {
	var status string
	var markedAt sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT status, manual_instrumental_at FROM work_queue WHERE id = ?`, id).Scan(&status, &markedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrManualInstrumentalNotFound
	}
	if err != nil {
		return false, fmt.Errorf("queue: mark manual instrumental %d: %w", id, err)
	}
	if status == StatusProcessing {
		return false, ErrManualInstrumentalInFlight
	}
	if markedAt.Valid {
		return false, nil
	}
	ts := formatTime(now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET manual_instrumental_at = ?,
             status = 'done',
             outcome_type = 'instrumental',
             provider_lane = ?,
             upstream = NULL,
             instrumental_result = NULL,
             sync_tier = NULL,
             word_timing_state = NULL,
             word_timing_generation = NULL,
             word_timing_checked_at = NULL,
             lyric_edited_at = NULL,
             lyric_offset_ms = NULL,
             timing_outcome = NULL,
             overrun_magnitude = NULL,
             overrun_ratio = NULL,
             evaluated_at = NULL,
             refused_waits = 0,
             upgrade_queued = 0,
             upgrade_checked_at = NULL,
             last_error = '',
             completed_at = ?
         WHERE id = ?`, ts, ManualLane, ts, id); err != nil {
		return false, fmt.Errorf("queue: mark manual instrumental %d: %w", id, err)
	}
	if err := writeBackScanResultsDone(ctx, tx, id); err != nil {
		return false, fmt.Errorf("queue: mark manual instrumental %d scan_results writeback: %w", id, err)
	}
	return true, nil
}

// UnmarkManualInstrumentalTx removes the mark and re-queues the row for a fresh
// fetch at PriorityScan, with linked scan_results back to 'pending'. Returns
// false, nil for a row that is not marked.
func UnmarkManualInstrumentalTx(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET manual_instrumental_at = NULL,
             status = 'deferred',
             priority = ?,
             outcome_type = NULL,
             provider_lane = NULL,
             upstream = NULL,
             completed_at = NULL,
             attempts = 0,
             next_attempt_at = ?,
             last_error = '',
             refused_waits = 0
         WHERE id = ? AND manual_instrumental_at IS NOT NULL AND status <> 'processing'`,
		PriorityScan, formatTime(now), id)
	if err != nil {
		return false, fmt.Errorf("queue: unmark manual instrumental %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: unmark manual instrumental %d rows affected: %w", id, err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results SET status = 'pending'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status = 'done'`, id); err != nil {
		return false, fmt.Errorf("queue: unmark manual instrumental %d scan_results writeback: %w", id, err)
	}
	return true, nil
}

// MarkManualInstrumental is MarkManualInstrumentalTx in its own transaction.
func (q *DBQueue) MarkManualInstrumental(ctx context.Context, id int64) (bool, error) {
	return q.inTx(ctx, "mark manual instrumental", func(tx *sql.Tx) (bool, error) {
		return MarkManualInstrumentalTx(ctx, tx, id, q.now())
	})
}

// UnmarkManualInstrumental is UnmarkManualInstrumentalTx in its own transaction.
func (q *DBQueue) UnmarkManualInstrumental(ctx context.Context, id int64) (bool, error) {
	return q.inTx(ctx, "unmark manual instrumental", func(tx *sql.Tx) (bool, error) {
		return UnmarkManualInstrumentalTx(ctx, tx, id, q.now())
	})
}

func (q *DBQueue) inTx(ctx context.Context, op string, fn func(*sql.Tx) (bool, error)) (bool, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: begin %s tx: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := fn(tx)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("queue: commit %s tx: %w", op, err)
	}
	return ok, nil
}

// ManualInstrumental reports whether row id is hand-marked and when.
func (q *DBQueue) ManualInstrumental(ctx context.Context, id int64) (at time.Time, marked bool, err error) {
	var s sql.NullString
	if err := q.db.QueryRowContext(ctx,
		`SELECT manual_instrumental_at FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("queue: manual instrumental %d: %w", id, err)
	}
	if !s.Valid {
		return time.Time{}, false, nil
	}
	at, err = time.Parse(timeFormat, s.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("queue: manual instrumental %d time %q: %w", id, s.String, err)
	}
	return at, true, nil
}

// ManualInstrumentalAmong returns the ids among ids that are hand-marked.
func (q *DBQueue) ManualInstrumentalAmong(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool)
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.db.QueryContext(ctx,
		`SELECT id FROM work_queue WHERE manual_instrumental_at IS NOT NULL AND id IN (?`+ //nolint:gosec // reason: G202 -- only "?" placeholders are concatenated
			strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: manual instrumental among: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("queue: manual instrumental among scan: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: manual instrumental among rows: %w", err)
	}
	return out, nil
}
