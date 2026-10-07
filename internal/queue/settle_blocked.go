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

// SettleBlocked completes a processing row as blocked in ONE statement:
// outcome_type='blocked', status='done', and the scan_results writeback commit
// together or not at all, modeled on SettleGuardRejected (#655).
//
// The lanes answered with a result, they did not miss, so miss_count, attempts
// and lane_attempts are deliberately untouched. last_error is cleared (a policy
// outcome is not a failure) and refused_waits reset, as on every settle.
// Nothing here schedules a retry: only ReopenBlockedTx brings the row back.
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
             outcome_detail = NULL,
             status = 'done',
             completed_at = ?,
             last_error = '',
             refused_waits = 0
         WHERE id = ?
           AND status = ?`,
		OutcomeBlocked, now, id, StatusProcessing,
	)
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle blocked: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SettleFailed, fmt.Errorf("queue: settle blocked rows affected: %w", err)
	}
	if n == 0 {
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
// ReopenDoneRowTx, so every settle column is cleared with the label and a
// manually marked instrumental row is still never reopened.
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
