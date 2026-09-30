package queue

import (
	"context"
	"fmt"
)

// WordGenerateOptions scopes the word-sync generation candidate set (#1007).
type WordGenerateOptions struct {
	// GeneratorVersion is the running generator's version: a row stamped with
	// it was already handled and is skipped; a version bump re-opens it.
	GeneratorVersion int64
	// WordGeneration is the worker's live word-capability generation; only an
	// 'absent' verdict under exactly it means the provider path had its chance.
	WordGeneration int64
	// Limit caps the list when > 0.
	Limit int
}

// wordGenerateCommon holds the terms both arms share (one bound arg, the
// generator version): settled, a source path to derive the sidecar from, not
// prune's retire-as-gone sentinel, not held by the word-recheck or upgrade
// sweep, and not handled by this generator version since its last completion:
// a re-settle (re-fetch, purge reset, served recheck) moves completed_at, so a
// verdict on an earlier file never hides a new one.
const wordGenerateCommon = ` status = 'done'
   AND TRIM(COALESCE(source_path, '')) <> ''
   AND COALESCE(last_error, '') = ''` + notWordRecheckQueued + `
   AND (word_generate_version IS NULL OR word_generate_version <> ?
        OR word_generate_at <= completed_at)`

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

// wordGenerateRetimeArm: known words whose timing overran the audio (#440),
// as a .txt or a kept/demoted/quarantined .lrc (read what is on disk). No #982
// verdict is required: the recheck never examines mis_synced. A row held by the
// accept-time guard had every lane tried; one marked by the timing sweep or
// revalidate --apply was NOT re-asked of current lanes, and nothing here does.
const wordGenerateRetimeArm = ` AND timing_outcome = 'mis_synced'`

// ListWordGenerateCandidates returns up to opts.Limit candidate ids, least
// recently offered first (never offered leads), then oldest completion. The
// arms are disjoint and each is its own indexed SELECT. Read-only.
func (q *DBQueue) ListWordGenerateCandidates(ctx context.Context, opts WordGenerateOptions) ([]int64, error) {
	const cols = `SELECT id, word_generate_at, completed_at FROM work_queue WHERE`
	query := cols + wordGenerateCommon + wordGenerateLineArm + ` UNION ALL ` + cols + wordGenerateCommon + wordGenerateRetimeArm //nolint:gosec // reason: G202 -- package-constant fragments, bound parameters only
	query = `SELECT id FROM (` + query + `) ORDER BY word_generate_at ASC, completed_at ASC, id ASC`
	args := []any{opts.GeneratorVersion, opts.WordGeneration, opts.GeneratorVersion}
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return q.queryIDs(ctx, "list word generate candidates", query, args...)
}

// MarkWordGenerateOffered stamps the offer time before the generator sees ids,
// so a row it keeps leaving unhandled rotates behind never-offered rows. It
// clears the version (already void on a candidate; kept beside a fresh time it
// would hide a re-settled row). StampWordGenerateAttempt requires the offer.
func (q *DBQueue) MarkWordGenerateOffered(ctx context.Context, ids []int64) error {
	now := formatTime(q.now())
	for _, id := range ids {
		if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET word_generate_at = ?, word_generate_version = NULL
             WHERE id = ? AND status = 'done'`, now, id); err != nil {
			return fmt.Errorf("queue: mark word generate offered id %d: %w", id, err)
		}
	}
	return nil
}

// StampWordGenerateAttempt records that the generator at version handled ids
// (whatever it decided: wrote, rejected at a gate, or found nothing to align),
// so they are skipped until the version or the completion changes. Call it
// ONLY for rows a generator processed. It lands only on a done row offered
// strictly after its last completion, so a row re-settled since the offer
// (the generator judged the old file) is not stamped; the generator must not
// move completed_at itself. Returns the ids stamped.
func (q *DBQueue) StampWordGenerateAttempt(ctx context.Context, ids []int64, version int64) ([]int64, error) {
	var stamped []int64
	now := formatTime(q.now())
	for _, id := range ids {
		res, err := q.db.ExecContext(ctx, `UPDATE work_queue SET word_generate_version = ?, word_generate_at = ?
             WHERE id = ? AND status = 'done' AND word_generate_at > COALESCE(completed_at, '')`, version, now, id)
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
