package queue

import (
	"context"
	"fmt"
)

// WordGenerateOptions scopes the word-sync generation candidate set (#1007).
type WordGenerateOptions struct {
	// GeneratorVersion is the running generator's version: a row whose
	// word_generate_version equals it was already handled and is skipped, and
	// any other value (a version bump) re-opens it.
	GeneratorVersion int64
	// WordGeneration is the worker's live word-capability generation (the
	// word-recheck sweep's). Only an 'absent' verdict under exactly this
	// generation counts as "the provider path had its chance".
	WordGeneration int64
	// Limit caps the list when > 0.
	Limit int
}

// wordGenerateCommon holds the terms both arms share (one bound arg, the
// generator version): settled, a source path to derive the sidecar from, not
// prune's retire-as-gone sentinel, not held by the word-recheck or upgrade
// sweep, and not yet handled by this generator version.
const wordGenerateCommon = ` status = 'done'
   AND TRIM(COALESCE(source_path, '')) <> ''
   AND COALESCE(last_error, '') = ''` + notWordRecheckQueued + `
   AND (word_generate_version IS NULL OR word_generate_version <> ?)`

// wordGenerateLineArm (one bound arg, the word generation): a line-synced
// .lrc on disk (sync_tier, #1075) whose provider re-examination (#982)
// answered 'absent' under the CURRENT word generation. NULL, 'queued',
// 'served' and a stale-generation 'absent' are not candidates: the provider
// path goes first. Timing-remediated rows belong to the other arm or to none.
// Its first terms match idx_work_queue_word_timing's partial WHERE.
const wordGenerateLineArm = ` AND outcome_type = 'synced'
   AND sync_tier = 'line'
   AND word_timing_state = 'absent'
   AND word_timing_generation = ?
   AND COALESCE(timing_outcome, '') NOT IN ('categorical', 'mis_synced', 'degenerate')`

// wordGenerateRetimeArm: known words whose timing overran the audio (#440). The
// accept-time guard settled these as a .txt (outcome unsynced); the timing
// sweep leaves outcome_type describing the old .lrc and demoted, quarantined
// or kept the file per on_mis_synced, so the generator must read what is on
// disk. No #982 verdict is required: the recheck never examines mis_synced
// rows, and a provider re-fetch returns the same catalog timing. Categorical
// (another song's words) and degenerate are not candidates.
const wordGenerateRetimeArm = ` AND timing_outcome = 'mis_synced'`

// ListWordGenerateCandidates returns up to opts.Limit ids for word-sync
// generation, never-handled first, then oldest completion. The two arms are
// disjoint (the line arm excludes mis_synced) and each is its own indexed
// SELECT, so a cycle never scans the library. Read-only.
func (q *DBQueue) ListWordGenerateCandidates(ctx context.Context, opts WordGenerateOptions) ([]int64, error) {
	query := `SELECT id, word_generate_at, completed_at FROM work_queue WHERE` + wordGenerateCommon + wordGenerateLineArm + //nolint:gosec // reason: G202 -- package-constant fragments, bound parameters only
		` UNION ALL SELECT id, word_generate_at, completed_at FROM work_queue WHERE` + wordGenerateCommon + wordGenerateRetimeArm
	query = `SELECT id FROM (` + query + `) ORDER BY word_generate_at ASC, completed_at ASC, id ASC`
	args := []any{opts.GeneratorVersion, opts.WordGeneration, opts.GeneratorVersion}
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return q.queryIDs(ctx, "list word generate candidates", query, args...)
}

// StampWordGenerateAttempt records that the generator at version handled ids
// (whatever it decided: wrote, rejected at a gate, or found nothing to align),
// so the next cycle skips them until the version changes. Call it ONLY for
// rows a generator actually processed; stamping a merely selected row would
// hide it from real generation forever. Keyed on id and status = 'done' (a
// row the worker took back in the meantime is not stamped); not revalidated
// against the candidate predicate, because a successful generation itself
// changes the row (a new sync tier) and the marker must still land. Returns
// the ids stamped.
func (q *DBQueue) StampWordGenerateAttempt(ctx context.Context, ids []int64, version int64) ([]int64, error) {
	var stamped []int64
	now := formatTime(q.now())
	for _, id := range ids {
		res, err := q.db.ExecContext(ctx, `UPDATE work_queue SET word_generate_version = ?, word_generate_at = ?
             WHERE id = ? AND status = 'done'`, version, now, id)
		if err != nil {
			return stamped, fmt.Errorf("queue: stamp word generate id %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return stamped, fmt.Errorf("queue: stamp word generate id %d: %w", id, err)
		} else if n > 0 {
			stamped = append(stamped, id)
		}
	}
	return stamped, nil
}
