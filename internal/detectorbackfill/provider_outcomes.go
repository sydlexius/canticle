package detectorbackfill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProviderOutcomesMarker names the one-shot provider_outcomes backfill in
// maintenance_markers (migration 027). Unlike lane_attempts, provider_outcomes
// is a bare per-lane counter (lane TEXT PRIMARY KEY, hits, misses) with no
// per-row key, so nothing about the write is naturally idempotent -- re-running
// would double-credit. The marker IS the idempotency, which is why it is
// committed in the same transaction as the counter update (#548).
const ProviderOutcomesMarker = "provider_outcomes_detector_hits_548"

// ProviderOutcomesResult reports what the counter backfill did.
type ProviderOutcomesResult struct {
	// Credited is the number of hits added to the detector lane's counter.
	Credited int64
	// AlreadyDone is true when the marker was present and nothing was written.
	AlreadyDone bool
}

// countUnattributedDetectorHitsSQL selects detector settles that were never
// credited to provider_outcomes.
//
// WHY THIS IS A RECONSTRUCTION AND NOT AN ESTIMATE. Every predicate is exact:
//
//   - instrumental_result = 1 is written by exactly two paths, both detector
//     settles (queue.SettleInstrumental, and the worker's detector path via
//     queue.Complete). No provider and no operator writes it, so a 1 means the
//     detector settled this row.
//   - outcome_type = 'instrumental' pins the settle to the instrumental outcome
//     rather than any later re-decision.
//   - provider_lane IS NULL is used as a proxy for "never credited to
//     provider_outcomes". See the KNOWN IMPRECISION below: it is a very good
//     proxy, not a guarantee.
//
// A detector settle is TERMINAL -- it happens at most once per row -- so rows
// map 1:1 onto counter increments. That is what makes the hit side recoverable
// at all.
//
// KNOWN IMPRECISION, stated rather than papered over. worker.recordHit performs
// TWO INDEPENDENT writes -- providerRecorder.RecordProviderHit and
// queue.SetProviderLane -- and its own doc comment notes both are non-fatal:
// each logs at Warn and does not affect the outcome. They are not in a shared
// transaction, so a mid-processing write failure can leave the pair
// inconsistent, in either direction:
//
//	counted but NOT stamped -> provider_lane IS NULL on an already-counted row.
//	                           This backfill credits it a SECOND time. Because
//	                           the pass is one-shot and marker-gated, that
//	                           over-count is permanent and undetectable.
//	stamped but NOT counted -> excluded by the predicate and never credited by
//	                           this or any later pass. A permanent under-count.
//
// Neither is quantifiable from recorded data -- nothing distinguishes a
// half-written pair from a normal row. Both require a database write to fail
// mid-processing, which is rare against local SQLite on a single connection
// (db.Open sets MaxOpenConns(1)), but rare is not zero across a large history.
//
// This is accepted deliberately: provider_outcomes is an approximate metrics
// counter, not an accounting ledger, and the correction here is large (three
// orders of magnitude more settles than the counter currently reflects) while
// the error term is plausibly single-digit. The alternative -- refusing to
// backfill hits at all, on the same "not exactly reconstructable" standard that
// rules out the miss side -- was considered and rejected on those proportions.
// Do not restate this as exactness later; the imprecision is real.
const countUnattributedDetectorHitsSQL = `
	SELECT COUNT(*) FROM work_queue
	WHERE instrumental_result = 1
	  AND outcome_type = 'instrumental'
	  AND provider_lane IS NULL`

// addDetectorHitsSQL mirrors the live writer's upsert
// (queue.DBQueue.RecordProviderHit) exactly, batched: same table, same conflict
// target, same "add to hits" semantics. Backfilled rows therefore follow the
// same rule as live rows in the same counter, which is the property #537
// rejected a hits-only fill for lacking.
const addDetectorHitsSQL = `
	INSERT INTO provider_outcomes(lane, hits, misses) VALUES(?, ?, 0)
	ON CONFLICT(lane) DO UPDATE SET hits = hits + ?`

// BackfillProviderOutcomes credits historical detector settles to the detector
// lane's provider_outcomes hit counter, once per database.
//
// THE MISS SIDE IS DELIBERATELY NOT BACKFILLED, AND CANNOT BE. It is not an
// oversight and it should not be "finished" later without new recorded data:
//
// The live miss writer (worker.recordMisses) credits every lane returned by
// orch.LaneNames() -- the lanes ACTIVE AT THAT MOMENT -- and only on the
// all-lanes-missed path. Reconstructing that needs two facts the schema does
// not carry. work_queue.miss_count does record how many times a row was
// deferred, so the raw multiplicity survives; what does not survive is WHEN
// those misses happened. The detector lane did not always exist, so a row with
// miss_count = 2 may have accrued one miss before the lane existed and one
// after, and nothing distinguishes them. Crediting the sum would attribute
// misses to a lane that was not running, and would do so with a precise-looking
// number -- an estimate wearing the costume of a fact.
//
// CONSEQUENCE, WHICH IS EXPECTED: /metrics (which reads provider_outcomes) and
// the dashboard's provider-effectiveness tile (which reads lane_attempts, see
// reports.ProviderEffectiveness) will report different detector figures. That
// difference is a known, documented gap, not a bug to reconcile. The hit counts
// converge after this backfill; the miss counts do not.
//
// The whole pass is one transaction: the counter update and the marker commit
// together or not at all. A partial commit would leave the counter credited
// with no marker, and the next startup would credit it again.
func BackfillProviderOutcomes(ctx context.Context, sqlDB *sql.DB) (ProviderOutcomesResult, error) {
	var res ProviderOutcomesResult

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("detectorbackfill: begin provider_outcomes tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM maintenance_markers WHERE name = ?`, ProviderOutcomesMarker).Scan(&one)
	switch {
	case err == nil:
		res.AlreadyDone = true
		return res, nil
	case !errors.Is(err, sql.ErrNoRows):
		return res, fmt.Errorf("detectorbackfill: check marker %q: %w", ProviderOutcomesMarker, err)
	}

	if err := tx.QueryRowContext(ctx, countUnattributedDetectorHitsSQL).Scan(&res.Credited); err != nil {
		return res, fmt.Errorf("detectorbackfill: count uncredited detector hits: %w", err)
	}

	// Credit only when there is something to credit, but ALWAYS stamp the marker
	// below: a database with nothing to backfill is done, and re-counting it on
	// every startup buys nothing.
	if res.Credited > 0 {
		if _, err := tx.ExecContext(ctx, addDetectorHitsSQL, LaneName, res.Credited, res.Credited); err != nil {
			return res, fmt.Errorf("detectorbackfill: credit detector hits: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_markers (name) VALUES (?)`, ProviderOutcomesMarker); err != nil {
		return res, fmt.Errorf("detectorbackfill: record marker %q: %w", ProviderOutcomesMarker, err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("detectorbackfill: commit provider_outcomes backfill: %w", err)
	}
	return res, nil
}
