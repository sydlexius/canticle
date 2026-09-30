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

// upgradeMissyncedPredicate is the second
// population (#1120): a settled row marked mis_synced AFTER its fetch (the #443
// sweep, `revalidate --apply`, or a pre-055 stamp of unknown source), never
// re-asked of the current lanes. A fetch-stamped row is excluded: the
// orchestrator already tried every lane. missync_recheck_generation is the one-pass
// marker, so a row is re-asked only when the lane set (the generation) changed.
// Disjoint from upgradeCandidatePredicate (which excludes mis_synced), from the
// word recheck (same exclusion) and, being 'done', from an armed trip. No hold
// window for a row not admitted since its verdict (evaluated_at); a
// re-admission under the same verdict (a lane-set change, or a trip no lane
// answered, which SettleUpgradeTrip leaves unmarked) waits out the other arm's
// week hold, so an unreachable provider cannot re-spend a trip every cycle.
// Two args: the generation, then the hold cut.
const upgradeMissyncedPredicate = ` status = 'done'
   AND timing_outcome = 'mis_synced'
   AND COALESCE(timing_stamp_source, '') <> 'fetch'
   AND (missync_recheck_generation IS NULL OR missync_recheck_generation <> ?)
   AND (COALESCE(upgrade_checked_at, '') < ? OR upgrade_checked_at < COALESCE(evaluated_at, ''))
   AND COALESCE(word_timing_state, '') <> 'queued'
   AND TRIM(COALESCE(source_path, '')) <> ''
   AND COALESCE(last_error, '') = ''`

// ListUpgradeCandidates returns up to limit ids from both populations: settled
// and last admitted before holdBefore (below the line rung), and post-settle
// mis_synced rows not yet passed under the current providers generation
// (SetProvidersVersion, #1120). Never-admitted first, then longest-held. Read-only.
func (q *DBQueue) ListUpgradeCandidates(ctx context.Context, holdBefore time.Time, limit int) ([]int64, error) {
	cut := formatTime(holdBefore)
	const cols = `SELECT id, upgrade_checked_at, completed_at FROM work_queue WHERE`
	return q.queryIDs(ctx, "list upgrade candidates", `SELECT id FROM (`+cols+upgradeCandidatePredicate+ //nolint:gosec // reason: G202 -- package-constant fragments, bound parameters only
		` UNION ALL `+cols+upgradeMissyncedPredicate+`) ORDER BY upgrade_checked_at ASC, completed_at ASC, id ASC LIMIT ?`,
		cut, cut, q.providersVersion, cut, limit)
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
// the worker bypasses it on an upgrade trip. A mis_synced row's (#1120)
// one-pass marker is recorded only when the trip settles on an answer
// (SettleUpgradeTrip), never here, so a trip that never reached a lane is not
// a pass. A landed trip re-stamps the row at fetch, which leaves the arm.
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
             WHERE id = ? AND (`+upgradeCandidatePredicate+` OR `+upgradeMissyncedPredicate+`)`, //nolint:gosec // reason: G202 -- package-constant fragments, bound parameters only
				PriorityMiss, now, now, id, cut, cut, q.providersVersion, cut)
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
//
// answered says the lanes answered (a miss, a result judged and refused, a
// guard or verifier verdict). Only then does a mis_synced row (#1120) record
// its one provider pass under the current generation; a trip that never
// reached a lane (transport failures to the cap, a verifier error) is not a
// pass and is re-offered after the week hold.
func (q *DBQueue) SettleUpgradeTrip(ctx context.Context, id int64, answered bool) (settled bool, err error) {
	err = db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		res, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'done', last_error = '', refused_waits = 0, attempts = 0,
                 missync_recheck_generation = CASE WHEN ? AND timing_outcome = 'mis_synced' THEN ? ELSE missync_recheck_generation END
             WHERE id = ? AND status = 'processing' AND upgrade_queued = 1`, answered, q.providersVersion, id)
		if err != nil {
			return fmt.Errorf("queue: settle upgrade trip id %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		settled = n > 0
		return err
	})
	return settled, err
}
