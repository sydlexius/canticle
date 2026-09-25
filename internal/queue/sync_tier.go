package queue

import (
	"context"
	"fmt"
)

// On-disk sync tier values (#1075), stored in work_queue.sync_tier.
//
// DELIBERATELY SEPARATE from word_timing_state (see that column's doc
// comment): this records what the .lrc ARTIFACT actually is, independent of
// whether any provider lane was ever asked. A line-synced file written under
// output.word_sync_mode=off, served by a non-word-capable lane, or served
// from cache all carry a correct SyncTierLine here even though
// word_timing_state stays NULL for every one of them.
const (
	// SyncTierWord means the .lrc carries A2 inline word markers, or an owned
	// .elrc companion sits beside it.
	SyncTierWord = "word"
	// SyncTierLine means line-level timestamps only, no word markers or
	// companion.
	SyncTierLine = "line"
	// SyncTierUnsynced means the "synced" outcome's .lrc carries no
	// timestamps at all -- a corrupted or hand-placed file. Vanishingly rare
	// on a live write (WriteLRC only takes the .lrc branch when Subtitles
	// carries cues), but a real state the backfill scan can observe on disk.
	SyncTierUnsynced = "unsynced"
)

// validSyncTier reports whether tier is a recognized SyncTier* value or ""
// (clear to NULL). Shared by every writer of work_queue.sync_tier, matching
// SetWordTimingState's own validation.
func validSyncTier(tier string) bool {
	switch tier {
	case "", SyncTierWord, SyncTierLine, SyncTierUnsynced:
		return true
	default:
		return false
	}
}

// SetSyncTier records the on-disk sync tier for id (#1075): 'word', 'line',
// 'unsynced', or "" to clear it back to NULL (a completion that wrote no
// synced sidecar, or a reopened row). Unconditional on status, matching
// SetOutcomeType -- the worker calls this before Complete while the row is
// still 'processing', and a no-op for a missing id is benign. An
// unrecognized tier is refused before the UPDATE runs.
func (q *DBQueue) SetSyncTier(ctx context.Context, id int64, tier string) error {
	if !validSyncTier(tier) {
		return fmt.Errorf("queue: set sync tier for id %d: invalid tier %q", id, tier)
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET sync_tier = ? WHERE id = ?`,
		nullIfEmpty(tier), id,
	)
	if err != nil {
		return fmt.Errorf("queue: set sync tier for id %d: %w", id, err)
	}
	return nil
}

// SetSyncTierIfPending is the backfill's guarded writer (#1075 finding 3):
// unlike SetSyncTier, it applies only if the row still matches
// ListSyncTierPending's predicate, so a raced row is never overwritten. Same
// invalid-tier guard as SetSyncTier (hostile-review, Copilot 4097431615).
//
// backup, if non-nil, runs INSIDE this transaction -- only once RowsAffected
// confirms the row applied, and BEFORE commit (identityrepair.apply's
// backup-first contract; #1087 review, Copilot 4100243142 / CodeRabbit
// 4100264748): the record is durable before the stamp commits, a backup
// failure rolls the stamp back, and a raced row (RowsAffected == 0) never
// calls backup, so no record is written for a change that did not happen.
func (q *DBQueue) SetSyncTierIfPending(ctx context.Context, id int64, tier string, backup func() error) (bool, error) {
	if !validSyncTier(tier) {
		return false, fmt.Errorf("queue: set sync tier if pending for id %d: invalid tier %q", id, tier)
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: set sync tier if pending for id %d: begin tx: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue SET sync_tier = ?
         WHERE id = ? AND outcome_type = 'synced' AND status = 'done' AND sync_tier IS NULL`,
		nullIfEmpty(tier), id,
	)
	if err != nil {
		return false, fmt.Errorf("queue: set sync tier if pending for id %d: %w", id, err)
	}
	n, rerr := res.RowsAffected()
	if rerr != nil {
		return false, fmt.Errorf("queue: set sync tier if pending for id %d: rows affected: %w", id, rerr)
	}
	if n == 0 {
		return false, nil
	}
	if backup != nil {
		if berr := backup(); berr != nil {
			return false, fmt.Errorf("queue: set sync tier if pending for id %d: backup failed, stamp rolled back: %w", id, berr)
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return false, fmt.Errorf("queue: set sync tier if pending for id %d: commit: %w", id, cerr)
	}
	return true, nil
}

// SyncTierCandidate is one row the `scan reconcile-sync-tier` backfill (#1075)
// must classify: its id and the audio path the sidecar is derived from.
type SyncTierCandidate struct {
	ID        int64
	AudioPath string
}

// ListSyncTierPending returns every completed synced row with no recorded
// sync_tier (outcome_type='synced' AND status='done' AND sync_tier IS NULL).
// NOT index-covered: migration 052's partial index over this predicate was
// dropped after EXPLAIN QUERY PLAN showed SQLite preferring
// idx_work_queue_dequeue instead (#1075 finding 7; see that migration).
// Loaded fully into memory: a one-time backfill over a bounded population.
func (q *DBQueue) ListSyncTierPending(ctx context.Context) ([]SyncTierCandidate, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, COALESCE(source_path, '') FROM work_queue
         WHERE outcome_type = 'synced' AND status = 'done' AND sync_tier IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("queue: list sync tier pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SyncTierCandidate
	for rows.Next() {
		var c SyncTierCandidate
		if err := rows.Scan(&c.ID, &c.AudioPath); err != nil {
			return nil, fmt.Errorf("queue: scan sync tier candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list sync tier pending rows: %w", err)
	}
	return out, nil
}
