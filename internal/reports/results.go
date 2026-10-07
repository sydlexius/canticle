package reports

import (
	"context"
	"fmt"
)

// lineTierPredicate is the ONE definition of "this synced row is at the
// line-synced rung": sync_tier='line' with the same two exclusions as
// wordTierPredicate (a timing-remediated tier is stale; a row mid
// word-recheck is re-litigating its tier). Shared by ResultsBreakdown and
// ListBucket's previewability expression, so the two cannot disagree. No
// leading AND/WHERE.
const lineTierPredicate = lineTierFilePredicate + `
                      AND ` + timingVerdictExclusion

// resultBucketCaseSQL is the ONE classification of a done row into its result
// bucket ('word', 'line', 'tier_unknown', 'unsynced', 'instrumental', 'blocked', 'other').
// ResultsBreakdown and SourceBreakdown both select it, so a per-source count can
// never disagree with the dashboard tile. A single CASE chain: no row matches
// two arms and an unmatched row falls to 'other'. No leading SELECT.
const resultBucketCaseSQL = `CASE
                  WHEN ` + retiredPredicate + ` THEN 'other'
                  WHEN outcome_type = 'synced' AND ` + wordTierPredicate + ` THEN 'word'
                  WHEN outcome_type = 'synced' AND ` + lineTierPredicate + ` THEN 'line'
                  WHEN outcome_type = 'synced' AND ` + TierUnknownPredicate + ` THEN 'tier_unknown'
                  WHEN outcome_type = 'unsynced' THEN 'unsynced'
                  WHEN outcome_type = 'instrumental' THEN 'instrumental'
                  WHEN outcome_type = 'blocked' THEN 'blocked'
                  ELSE 'other'
                END`

// ResultsBreakdown is the complete split of the completed population
// (work_queue rows with status='done', the same rows QueueSummary.Done counts)
// by result type (#599, maintainer decision 2026-09-30). Every done row lands
// in EXACTLY ONE field, so the seven always sum to QueueSummary.Done.
//
// Every field reads work_queue only (status, outcome_type, sync_tier,
// timing_outcome, word_timing_state); no other table is consulted.
type ResultsBreakdown struct {
	// WordSynced: outcome_type='synced' AND wordTierPredicate. QueueSummary.Finished
	// is this PLUS the hand-marked instrumental rows (#1405), which land in
	// Instrumental here: Finished = WordSynced + marked rows under TopRungWord,
	// and WordSynced + LineSynced + marked rows under TopRungLine (#1275).
	WordSynced int64
	// LineSynced: outcome_type='synced' AND lineTierPredicate.
	LineSynced int64
	// Unsynced: outcome_type='unsynced' (only a .txt was written).
	Unsynced int64
	// Instrumental: outcome_type='instrumental', detector- and
	// provider-written alike (the CountInstrumental population, restricted
	// to status='done').
	Instrumental int64
	// Blocked: outcome_type='blocked' (#1395): every result for the track was
	// one an operator marked wrong, so it settled with nothing on disk. Not a
	// miss and not Other; a prune-retired row stays in Other.
	Blocked int64
	// SyncedTierUnknown: outcome_type='synced' AND TierUnknownPredicate (no
	// recorded tier, an 'unsynced' tier on a .lrc, a timing-remediated tier,
	// or a row mid word-recheck).
	SyncedTierUnknown int64
	// Other: every remaining done row -- a prune-retired row
	// (last_error = queue.UnresolvableGoneError, whatever stale outcome it
	// kept), outcome_type='rejected' (refused by
	// the language guard, nothing written) or NULL (settled before outcomes
	// were recorded). Exists so no completed row is ever dropped from the sum.
	Other int64
	// TopRung is the Repo's rung, so a caller can word the tiles to match
	// what QueueSummary.Finished counts. It does not change the split itself.
	TopRung TopRung
}

// Total is the sum of every bucket; it equals QueueSummary.Done.
func (b ResultsBreakdown) Total() int64 {
	return b.WordSynced + b.LineSynced + b.Unsynced + b.Instrumental + b.SyncedTierUnknown + b.Blocked + b.Other
}

// ResultsBreakdown returns the result-type split of status='done' rows in one
// scan over work_queue.
//
// This admits status='done' ONLY: a row mid
// word-recheck has been flipped to 'deferred' (queue.MarkWordRecheckQueued)
// and is counted by the queue-status row, not here, so the Results row can
// sum to Done exactly. The one reachable done+queued shape (prune's retired
// row) is still done, and lands in Other (the first arm of resultBucketCaseSQL).
//
// A row prune retired as unresolvable (last_error = queue.UnresolvableGoneError)
// is status='done' but keeps whatever outcome_type/sync_tier it had before, so
// its stale synced data must not count as a result: the first arm sends it to
// Other, still inside the total.
//
// The classification is a single CASE chain, so a row matching no named arm
// falls to Other rather than vanishing, and no row can match two arms.
func (r *Repo) ResultsBreakdown(ctx context.Context) (ResultsBreakdown, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+resultBucketCaseSQL+` AS bucket, COUNT(*)
         FROM work_queue
         WHERE status = 'done'
         GROUP BY bucket`)
	if err != nil {
		return ResultsBreakdown{}, fmt.Errorf("reports: results breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	b := ResultsBreakdown{TopRung: r.top}
	for rows.Next() {
		var bucket string
		var n int64
		if err := rows.Scan(&bucket, &n); err != nil {
			return ResultsBreakdown{}, fmt.Errorf("reports: scan results breakdown: %w", err)
		}
		switch bucket {
		case "word":
			b.WordSynced = n
		case "line":
			b.LineSynced = n
		case "tier_unknown":
			b.SyncedTierUnknown = n
		case "unsynced":
			b.Unsynced = n
		case "instrumental":
			b.Instrumental = n
		case "blocked":
			b.Blocked = n
		default:
			b.Other += n
		}
	}
	if err := rows.Err(); err != nil {
		return ResultsBreakdown{}, fmt.Errorf("reports: results breakdown rows: %w", err)
	}
	return b, nil
}

// lineEditableSQL is the ONE definition of "the offset editor may rewrite this
// row's .lrc": a settled synced row at the current line rung. PreviewSource and
// the queue listing both read it.
const lineEditableSQL = `status = 'done' AND outcome_type = 'synced' AND ` + lineTierPredicate
