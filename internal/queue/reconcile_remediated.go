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
	// Retired is set for a row prune retired as UnresolvableGoneError (its
	// audio is gone): reset refuses it, so the caller counts it as skipped.
	Retired bool
}

const (
	remediatedSelect = `SELECT id, COALESCE(source_path,''), artist, title, status,
                COALESCE(outcome_type,''), COALESCE(sync_tier,''), COALESCE(timing_outcome,''),
                COALESCE(word_timing_state,'') = 'queued', upgrade_queued <> 0,
                last_error = ?
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
	rows, err := q.db.QueryContext(ctx, query, UnresolvableGoneError)
	if err != nil {
		return nil, fmt.Errorf("queue: list remediated candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RemediatedCandidate
	for rows.Next() {
		var c RemediatedCandidate
		if err := rows.Scan(&c.ID, &c.AudioPath, &c.Artist, &c.Title, &c.Status, &c.PriorOutcome,
			&c.PriorSyncTier, &c.PriorTiming, &c.WordQueued, &c.UpgradeArmed, &c.Retired); err != nil {
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
	// Only word and line leave TierUnknownPredicate's tier arm; "" would store an
	// empty string and unsynced a synced row claiming an unsynced tier, both still
	// counted as tier unknown while this call reported success.
	if action == RemediatedTier && tier != SyncTierWord && tier != SyncTierLine {
		return false, fmt.Errorf("queue: apply remediated for id %d: invalid tier %q", c.ID, tier)
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: apply remediated for id %d: begin tx: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	// The guard also matches the listed snapshot (tier, timing verdict, identity),
	// so a row re-stamped between list and apply -- by SetTimingOutcomeIfIdle,
	// SetSyncTierIfPending or an identity re-key -- is left alone rather than
	// overwritten from stale state, and reset never invalidates a stale cache key.
	const guard = ` WHERE id = ? AND outcome_type = 'synced' AND status = 'done'
                AND COALESCE(word_timing_state,'') <> 'queued' AND upgrade_queued = 0
                AND COALESCE(sync_tier,'') = ? AND COALESCE(timing_outcome,'') = ?
                AND artist = ? AND title = ?`
	snap := []any{c.ID, c.PriorSyncTier, c.PriorTiming, c.Artist, c.Title}
	var res interface{ RowsAffected() (int64, error) }
	switch action {
	case RemediatedReset:
		// A row retired with a last_error sentinel (prune's gone-audio retire)
		// is refused: prune would re-retire it as done+synced+NULL tier, putting
		// it back in tier-unknown every run. outcome_type = NULL matches what
		// SetRemediatedFileState writes when no sidecar remains (#1130).
		res, err = tx.ExecContext(ctx,
			`UPDATE work_queue SET status = 'deferred', priority = -100, attempts = 0, refused_waits = 0, outcome_type = NULL,
                 next_attempt_at = ?, last_error = '', sync_tier = NULL,
                 word_timing_state = NULL, word_timing_generation = NULL, word_timing_checked_at = NULL,
                 upgrade_checked_at = NULL, upgrade_queued = 0, timing_outcome = NULL,
                 overrun_magnitude = NULL, overrun_ratio = NULL, evaluated_at = NULL,
                 timing_stamp_source = NULL, missync_recheck_generation = NULL`+guard+
				` AND COALESCE(last_error,'') = ''`,
			append([]any{formatTime(time.Now().UTC())}, snap...)...)
	case RemediatedUnsynced:
		res, err = tx.ExecContext(ctx, `UPDATE work_queue SET outcome_type = 'unsynced', sync_tier = NULL`+guard, snap...)
	case RemediatedTier:
		// Records the tier only. A row whose timing_outcome carries a remediation
		// verdict (categorical/mis_synced/degenerate) keeps it and so stays in
		// TierUnknownPredicate by design: the verdict is history the #1120
		// post-settle pass and the review queue read, and clearing it here would
		// erase a judgment this command never re-made. The caller counts such rows
		// separately rather than expecting them to drain.
		res, err = tx.ExecContext(ctx, `UPDATE work_queue SET sync_tier = ?`+guard, append([]any{tier}, snap...)...)
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
			// A 'processing' scan result is mid-flight for another claim; leave it
			// (as Fail and Retry scope their writebacks) rather than drop its reservation.
			`UPDATE scan_results SET status = 'pending' WHERE status NOT IN ('pending','processing') AND id IN
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
