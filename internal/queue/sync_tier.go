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

// SetSyncTier records the on-disk sync tier for id (#1075): 'word', 'line',
// 'unsynced', or "" to clear it back to NULL (a completion that wrote no
// synced sidecar, or a reopened row). Unconditional on status, matching
// SetOutcomeType -- the worker calls this before Complete while the row is
// still 'processing', and a no-op for a missing id is benign.
func (q *DBQueue) SetSyncTier(ctx context.Context, id int64, tier string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET sync_tier = ? WHERE id = ?`,
		nullIfEmpty(tier), id,
	)
	if err != nil {
		return fmt.Errorf("queue: set sync tier for id %d: %w", id, err)
	}
	return nil
}
