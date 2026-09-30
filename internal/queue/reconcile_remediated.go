package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
)

// Actions ApplyRemediated performs (#1143).
const (
	// RemediatedReset returns the row to the fetch path (no sidecar remains).
	RemediatedReset = "reset"
	// RemediatedUnsynced re-describes the row as unsynced (only a .txt remains).
	RemediatedUnsynced = "unsynced"
	// RemediatedTier records the classified tier of a present .lrc.
	RemediatedTier = "tier"
)

// RemediatedCandidate is one row the dashboard counts as "Synced (tier
// unknown)" that `scan reconcile-remediated` must re-describe. The prior
// fields exist for the write-ahead backup record.
type RemediatedCandidate struct {
	ID                                       int64
	Artist, Title, AudioPath, Status         string
	PriorOutcome, PriorSyncTier, PriorTiming string
	WordQueued, UpgradeArmed                 bool
}

const (
	remediatedSelect = `SELECT id, COALESCE(source_path,''), artist, title, status,
                COALESCE(outcome_type,''), COALESCE(sync_tier,''), COALESCE(timing_outcome,''),
                COALESCE(word_timing_state,'') = 'queued', upgrade_queued <> 0
           FROM work_queue
          WHERE outcome_type = 'synced' AND `
	remediatedTail = `
            AND (status IN ('done','processing') OR word_timing_state = 'queued' OR upgrade_queued <> 0)`
)

// ListRemediatedCandidates returns the synced rows matching tierUnknown, the
// reports package's TierUnknownPredicate (passed in so queue stays free of a
// reports dependency and the predicate is never copied). It also admits
// processing, word-recheck-queued and upgrade-armed rows so the caller can
// count them as skipped.
func (q *DBQueue) ListRemediatedCandidates(ctx context.Context, tierUnknown string) ([]RemediatedCandidate, error) {
	query := remediatedSelect + tierUnknown + remediatedTail //nolint:gosec // reason: G202 -- tierUnknown is reports.TierUnknownPredicate, a compile-time constant
	rows, err := q.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("queue: list remediated candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RemediatedCandidate
	for rows.Next() {
		var c RemediatedCandidate
		if err := rows.Scan(&c.ID, &c.AudioPath, &c.Artist, &c.Title, &c.Status, &c.PriorOutcome,
			&c.PriorSyncTier, &c.PriorTiming, &c.WordQueued, &c.UpgradeArmed); err != nil {
			return nil, fmt.Errorf("queue: scan remediated candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyRemediated applies one action to a candidate in a single transaction,
// guarded so a row that raced (claimed, rechecking, armed, already re-described)
// is untouched. backup runs inside the transaction once the row is confirmed,
// before commit (backup-first, as SetSyncTierIfPending). RemediatedReset also
// invalidates the cache entry in the SAME transaction, so a cache hit cannot
// re-satisfy the refetch (the #474 pattern), and clears the remediation stamps
// so the fresh result is judged. A row that raced returns false, nil.
func (q *DBQueue) ApplyRemediated(ctx context.Context, c RemediatedCandidate, action, tier string, backup func() error) (bool, error) {
	if action == RemediatedTier && !validSyncTier(tier) {
		return false, fmt.Errorf("queue: apply remediated for id %d: invalid tier %q", c.ID, tier)
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: apply remediated for id %d: begin tx: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	const guard = ` WHERE id = ? AND outcome_type = 'synced' AND status = 'done'
                AND COALESCE(word_timing_state,'') <> 'queued' AND upgrade_queued = 0`
	var res interface{ RowsAffected() (int64, error) }
	switch action {
	case RemediatedReset:
		res, err = tx.ExecContext(ctx,
			`UPDATE work_queue SET status = 'deferred', priority = -100, attempts = 0,
                 next_attempt_at = ?, last_error = '', sync_tier = NULL,
                 word_timing_state = NULL, word_timing_generation = NULL, word_timing_checked_at = NULL,
                 upgrade_checked_at = NULL, upgrade_queued = 0, timing_outcome = NULL,
                 overrun_magnitude = NULL, overrun_ratio = NULL, evaluated_at = NULL,
                 timing_stamp_source = NULL, missync_recheck_generation = NULL`+guard,
			formatTime(time.Now().UTC()), c.ID)
	case RemediatedUnsynced:
		res, err = tx.ExecContext(ctx, `UPDATE work_queue SET outcome_type = 'unsynced', sync_tier = NULL`+guard, c.ID)
	case RemediatedTier:
		res, err = tx.ExecContext(ctx, `UPDATE work_queue SET sync_tier = ?`+guard, tier, c.ID)
	default:
		return false, fmt.Errorf("queue: apply remediated for id %d: unknown action %q", c.ID, action)
	}
	if err != nil {
		return false, fmt.Errorf("queue: apply remediated %s for id %d: %w", action, c.ID, err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil || n == 0 {
		return false, rerr
	}
	if action == RemediatedReset {
		if _, err := tx.ExecContext(ctx,
			`UPDATE scan_results SET status = 'pending' WHERE status <> 'pending' AND id IN
               (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?
                UNION SELECT scan_result_id FROM work_queue WHERE id = ? AND scan_result_id IS NOT NULL)`,
			c.ID, c.ID); err != nil {
			return false, fmt.Errorf("queue: reset scan_results for id %d: %w", c.ID, err)
		}
		if _, err := cache.Invalidate(ctx, tx, c.Artist, c.Title); err != nil {
			return false, fmt.Errorf("queue: invalidate cache for id %d: %w", c.ID, err)
		}
	}
	if backup != nil {
		if berr := backup(); berr != nil {
			return false, fmt.Errorf("queue: apply remediated for id %d: backup failed, rolled back: %w", c.ID, berr)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("queue: apply remediated for id %d: commit: %w", c.ID, err)
	}
	return true, nil
}
