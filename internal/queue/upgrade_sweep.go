package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/db"
)

// upgradeCandidatePredicate (two cutoff args) is the upgrade sweep's population
// (#553): settled rows BELOW the line rung (.txt, marker, untimed .lrc). Line
// rows belong to the word-recheck sweep (#1048). Excluded: remediation-guard
// rows, word-recheck rows, no source path, prune's retire-as-gone sentinel
// (last_error on a done row), and rows settled or admitted within the hold.
const upgradeCandidatePredicate = ` status = 'done'
   AND (outcome_type IN ('unsynced', 'instrumental') OR (outcome_type = 'synced' AND sync_tier = 'unsynced'))
   AND COALESCE(timing_outcome, '') NOT IN ('categorical', 'mis_synced', 'degenerate')
   AND COALESCE(word_timing_state, '') <> 'queued'
   AND TRIM(COALESCE(source_path, '')) <> ''
   AND COALESCE(last_error, '') = ''
   AND completed_at < ?
   AND COALESCE(upgrade_checked_at, '') < ?`

// ListUpgradeCandidates returns up to limit ids settled and last admitted
// before holdBefore, never-admitted first, then longest-held. Read-only.
func (q *DBQueue) ListUpgradeCandidates(ctx context.Context, holdBefore time.Time, limit int) ([]int64, error) {
	cut := formatTime(holdBefore)
	return q.queryIDs(ctx, "list upgrade candidates", `SELECT id FROM work_queue WHERE`+upgradeCandidatePredicate+ //nolint:gosec // reason: G202 -- package-constant fragment, bound parameters only
		` ORDER BY upgrade_checked_at ASC, completed_at ASC, id ASC LIMIT ?`, cut, cut, limit)
}

// CountUpgradeInFlight counts upgrade trips not yet settled. The 053 trigger
// clears the flag on every move to done/unavailable, so no settle path leaks one.
func (q *DBQueue) CountUpgradeInFlight(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE upgrade_queued = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count upgrade in flight: %w", err)
	}
	return n, nil
}

// MarkUpgradeQueued re-queues candidates in ONE statement, each revalidated
// against the predicate: 'pending' (several sweeps read 'deferred' as a miss)
// at PriorityMiss, due now, attempts/last_error/refused_waits cleared, armed
// (upgrade_queued) and stamped. outcome_type, sync_tier, lane, completed_at and
// miss_count are kept: they describe the file on disk. The cache is left alone:
// the worker bypasses it on an upgrade trip.
func (q *DBQueue) MarkUpgradeQueued(ctx context.Context, ids []int64, holdBefore time.Time) ([]int64, error) {
	var flipped []int64
	err := db.RetryBatchTx(ctx, "upgrade flip", func() error {
		flipped = flipped[:0]
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("queue: begin upgrade flip tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now, cut := formatTime(q.now()), formatTime(holdBefore)
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE work_queue SET status = 'pending', priority = ?, next_attempt_at = ?, attempts = 0,
                 last_error = '', refused_waits = 0, upgrade_queued = 1, upgrade_checked_at = ?
             WHERE id = ? AND`+upgradeCandidatePredicate, //nolint:gosec // reason: G202 -- package-constant fragment, bound parameters only
				PriorityMiss, now, now, id, cut, cut)
			if err != nil {
				return fmt.Errorf("queue: flip upgrade id %d: %w", id, err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("queue: flip upgrade id %d: %w", id, err)
			} else if n > 0 {
				flipped = append(flipped, id)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("queue: commit upgrade flip: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return flipped, nil
}

// SettleUpgradeTrip settles an upgrade trip that landed nothing (a miss, a
// refusal, a verifier or script-guard reject, a transport failure past its
// cap) back to 'done' with the file record untouched: outcome_type, sync_tier,
// timing_outcome, lane, completed_at and miss_count still describe the file on
// disk. false = the row is not a processing upgrade trip.
func (q *DBQueue) SettleUpgradeTrip(ctx context.Context, id int64) (settled bool, err error) {
	err = db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		res, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'done', last_error = '', refused_waits = 0, attempts = 0
             WHERE id = ? AND status = 'processing' AND upgrade_queued = 1`, id)
		if err != nil {
			return fmt.Errorf("queue: settle upgrade trip id %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		settled = n > 0
		return err
	})
	return settled, err
}
