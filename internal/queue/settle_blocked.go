package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/db"
)

// OutcomeBlocked is the outcome_type of a row whose every available result is a
// lyric body the operator marked wrong for the track (#1395). Nothing is on disk
// and nothing will be: re-fetching returns the same blocked body. The column has
// no CHECK constraint by design, so this needs no migration.
const OutcomeBlocked = "blocked"

// OutcomeDetailBlocked is the fixed outcome_detail a blocked row carries so the
// existing detail rendering (Recent outcomes) says why the row has no file. It is
// a constant: no lyric text, title or path can reach the column through it.
const OutcomeDetailBlocked = "blocked by operator"

// SettleBlocked completes a processing row as blocked in ONE statement:
// outcome_type='blocked', status='done', and the scan_results writeback commit
// together or not at all, modeled on SettleGuardRejected (#655).
//
// The lanes answered with a result, they did not miss, so miss_count, attempts
// and lane_attempts are deliberately untouched. last_error is cleared (a policy
// outcome is not a failure) and refused_waits reset, as on every settle.
// The settle is conditional on a lyric_blocks row still existing for the row's
// identity, in the same statement (SettleNoBlock otherwise), so it cannot race
// an unblock: that either commits first (no settle) or reopens the row after.
// Nothing here schedules a retry: only ReopenBlockedTx brings the row back.
//
// A blocked row has no file, so the same statement clears every column that
// describes a result: provider_lane and upstream (always written together),
// sync_tier, the timing verdict and a served/absent word verdict. Left behind,
// a stale lane would count the row as that lane's latest served track
// (LatestServedTrack) and attribute it in the source breakdown. A queued word
// recheck state is not a verdict and is left alone.
func (q *DBQueue) SettleBlocked(ctx context.Context, id int64) (SettleOutcome, error) {
	var outcome SettleOutcome
	err := db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		var err error
		outcome, err = q.settleBlockedOnce(ctx, id)
		return err
	})
	return outcome, err
}

func (q *DBQueue) settleBlockedOnce(ctx context.Context, id int64) (SettleOutcome, error) {
	now := formatTime(q.now())
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: begin settle blocked tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue
         SET outcome_type = ?,
             outcome_detail = ?,
             status = 'done',
             completed_at = ?,
             last_error = '',
             refused_waits = 0,
             provider_lane = NULL,
             upstream = NULL,
             sync_tier = NULL,
             timing_outcome = NULL,
             overrun_magnitude = NULL,
             overrun_ratio = NULL,
             evaluated_at = NULL,
             word_timing_generation = CASE WHEN word_timing_state IN ('served', 'absent') THEN NULL ELSE word_timing_generation END,
             word_timing_checked_at = CASE WHEN word_timing_state IN ('served', 'absent') THEN NULL ELSE word_timing_checked_at END,
             word_timing_state = CASE WHEN word_timing_state IN ('served', 'absent') THEN NULL ELSE word_timing_state END
         WHERE id = ?
           AND status = ?
           AND EXISTS (SELECT 1 FROM lyric_blocks b
                       WHERE b.artist_key = work_queue.artist_key AND b.title_key = work_queue.title_key)`,
		OutcomeBlocked, OutcomeDetailBlocked, now, id, StatusProcessing,
	)
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle blocked: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle blocked rows affected: %w", err)
	}
	if n == 0 {
		// Still processing means the only unmet condition was the block: an
		// unblock committed after the worker observed it, and settling now would
		// strand a blocked row that no later unblock could reopen by block id.
		var status string
		serr := tx.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status)
		if serr != nil && !errors.Is(serr, sql.ErrNoRows) {
			return SettleFailed, fmt.Errorf("queue: settle blocked status read: %w", serr)
		}
		if serr == nil && status == StatusProcessing {
			return SettleNoBlock, nil
		}
		return q.classifyNoSettle(ctx, tx, id)
	}
	if err := writeBackScanResultsDone(ctx, tx, id); err != nil {
		return SettleFailed, fmt.Errorf("queue: settle blocked scan_results writeback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SettleFailed, fmt.Errorf("queue: commit settle blocked tx: %w", err)
	}
	return Settled, nil
}

// ReopenBlockedTx reopens the settled row stored under (artistKey, titleKey) to
// 'pending' inside the caller's transaction, but only while it is still
// outcome_type='blocked': a row since settled by a real result is left alone.
// It is the reopen for clearing a block (#1397); the clear and the reopen
// should share one transaction. Reports whether a row was reopened. It reuses
// ReopenDoneRowTx, so every settle column is cleared with the label. A manually
// marked row carries outcome_type='instrumental', so it never matches the
// blocked lookup here; the manual-mark protection that matters is
// ReopenDoneRowTx's own manual_instrumental_at guard.
func ReopenBlockedTx(ctx context.Context, tx *sql.Tx, artistKey, titleKey string, now time.Time) (bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM work_queue
         WHERE artist_key = ? AND title_key = ? AND status = 'done' AND outcome_type = ?`,
		artistKey, titleKey, OutcomeBlocked,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue: find blocked row: %w", err)
	}
	return ReopenDoneRowTx(ctx, tx, id, now)
}
