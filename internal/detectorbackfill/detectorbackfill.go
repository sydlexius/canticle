// Package detectorbackfill attributes historical audio detections to the
// detector lane in lane_attempts (migration 022), correcting an under-count that
// exists because instrumentals settled before the detector became a lane (#501,
// #524) were resolved inline on the provider-miss path and never attributed to
// any lane (issue #537).
//
// This is a reporting artifact, not a behavioral bug: the tracks were detected
// correctly and their markers were written correctly. Only the attribution is
// missing.
//
// # The key, and why not the source tag
//
// The backfill is driven from work_queue.instrumental_result (migration 018),
// which records the audio-detection verdict independently of lane attribution:
//
//	1    -> the detector served the track          -> lane_attempts hit=1
//	0    -> the detector ran and lost              -> lane_attempts hit=0
//	NULL -> detection did not run for this item    -> no row
//
// It is deliberately NOT driven from a missing [source:] marker tag. On the
// instrumental branch of internal/lyrics/writer.go the source is song.WinningLane,
// replaced by lyrics.SourceDetector (the string "canticle-detector", NOT this
// package's LaneName) when song.DetectorVersion is set. Both a detector settle
// and a provider-flagged instrumental therefore carry a [source:] tag. A record
// with no source is one where NEITHER was recorded, which includes provider-flagged
// instrumentals predating WinningLane; attributing those to the detector would
// credit provider work to the detector and inflate the very figure this package
// exists to correct.
//
// # Both buckets, always
//
// reports.ProviderEffectiveness renders Hits/attempts where attempts = hits +
// misses, so filling only the hits would drive the displayed hit rate toward
// 100% and read worse than the current under-count. Hits and misses derive from
// a single scan of the same column here, so a hits-only fill is structurally
// impossible rather than merely avoided by care.
//
// # Recorded history wins over reconstruction
//
// Unlike queue.RecordLaneAttempts, which upserts with DO UPDATE so an --upgrade
// re-run refreshes the outcome, this package inserts with ON CONFLICT DO
// NOTHING. A lane_attempts row that already exists was recorded live and is
// authoritative; derived historical data must never overwrite it. That also
// makes the backfill idempotent at the row level: re-running it is a no-op.
//
// # Two uncovered remainders
//
// Rows with a NULL instrumental_result have no discriminator in the database and
// none in the marker files either, since the absent source tag is what makes
// them identifiable as a gap in the first place. They are counted and reported,
// never estimated: a guessed value written into a counter the dashboard presents
// as fact is worse than a known gap.
//
// The second remainder is invisible from here. lane_attempts deliberately
// carries no foreign key to work_queue so its history survives ClearDone, but
// this backfill reads work_queue, so detections on rows already swept cannot be
// recovered or even counted. Callers should say so rather than presenting the
// NULL tally as the whole remainder.
package detectorbackfill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LaneName is the lane string this package writes into lane_attempts.
//
// It is a fourth copy of the "detector" literal (alongside the unexported
// constants in internal/worker and internal/orchestrator, which a third package
// cannot import). Duplicating it is deliberate: issue #539 renames the Go
// identifiers only, and the persisted lane string stays "detector" precisely
// because it is the primary key of provider_outcomes and the value written to
// work_queue.provider_lane. Exporting one of the existing constants would invite
// that rename to move the stored value.
const LaneName = "detector"

// Change describes one work_queue row about to be attributed to the detector
// lane. It is reported to the caller once per row, for a preview under DryRun or
// a durable record under apply.
type Change struct {
	QueueID     int64  `json:"queue_id"`
	Lane        string `json:"lane"`
	Hit         bool   `json:"hit"`
	AttemptedAt string `json:"attempted_at"`
}

// Result tallies the outcome of a Run.
type Result struct {
	// Scanned counts work_queue rows carrying a usable instrumental_result.
	Scanned int
	// Hits and Misses count rows attributed as hit=1 and hit=0 respectively.
	// Both move together or the rendered hit rate is corrupted.
	Hits int
	// Misses counts rows attributed as hit=0 (the detector ran and lost).
	Misses int
	// AlreadyRecorded counts rows skipped because lane_attempts already held a
	// row for (queue_id, "detector"). Those were recorded live and are
	// authoritative; the backfill leaves them untouched.
	AlreadyRecorded int
	// UncoveredNull counts work_queue rows whose instrumental_result is NULL or
	// otherwise unusable. These are the recoverable-remainder-that-isn't: they
	// are reported, never estimated.
	UncoveredNull int
}

// Options controls a Run.
type Options struct {
	// DryRun computes and reports the attributions without writing them.
	DryRun bool
	// Report is invoked once per change: under DryRun before returning (a
	// preview), under apply inside the transaction before commit (a durable
	// record). Nil disables it. A Report error aborts the Run and rolls back.
	//
	// There is deliberately no Progress hook: measured at 72ms for 12k rows, the
	// run is far too short for a liveness signal to earn its keep.
	Report func(Change) error
}

// Backfiller attributes historical audio detections to the detector lane.
type Backfiller struct {
	db *sql.DB
}

// New builds a Backfiller over db.
func New(db *sql.DB) *Backfiller {
	return &Backfiller{db: db}
}

// row is one work_queue row's detection verdict, loaded up front so the writes
// do not hold a query cursor open on the single pooled connection.
type row struct {
	id          int64
	hit         bool
	attemptedAt string
}

// Run attributes every recoverable historical detection to the detector lane and
// returns the tally of what was written (or would be, under DryRun).
func (b *Backfiller) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result

	uncovered, err := b.countUncovered(ctx)
	if err != nil {
		return Result{}, err
	}
	res.UncoveredNull = uncovered

	rows, err := b.load(ctx)
	if err != nil {
		return Result{}, err
	}

	if opts.DryRun {
		return b.runDry(ctx, rows, res, opts)
	}
	return b.runApply(ctx, rows, res, opts)
}

// runDry previews the attributions without writing. It still consults
// lane_attempts so AlreadyRecorded reflects what an apply would actually skip,
// rather than over-promising the number of rows that would change.
func (b *Backfiller) runDry(ctx context.Context, rows []row, res Result, opts Options) (Result, error) {
	for _, rw := range rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Scanned++

		exists, err := b.alreadyRecorded(ctx, rw.id)
		if err != nil {
			return res, err
		}
		if exists {
			res.AlreadyRecorded++
			continue
		}

		tally(&res, rw.hit)
		if opts.Report != nil {
			if err := opts.Report(change(rw)); err != nil {
				return res, fmt.Errorf("detectorbackfill: report change for queue row %d: %w", rw.id, err)
			}
		}
	}
	return res, nil
}

// runApply writes the attributions in one transaction. opts.Report runs INSIDE
// that transaction, before commit, so the restorable record is durable before
// the write it protects commits (backup-first) and a report failure rolls the
// whole run back. A single transaction is safe here because ON CONFLICT DO
// NOTHING makes a re-run after an abort a no-op rather than a double-count.
func (b *Backfiller) runApply(ctx context.Context, rows []row, res Result, opts Options) (Result, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("detectorbackfill: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, rw := range rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Scanned++

		hit := 0
		if rw.hit {
			hit = 1
		}
		// DO NOTHING, not DO UPDATE: a row already present was recorded live by
		// queue.RecordLaneAttempts and outranks this reconstruction.
		out, err := tx.ExecContext(ctx,
			`INSERT INTO lane_attempts(queue_id, lane, hit, attempted_at) VALUES(?, ?, ?, ?)
             ON CONFLICT(queue_id, lane) DO NOTHING`,
			rw.id, LaneName, hit, rw.attemptedAt,
		)
		if err != nil {
			return res, fmt.Errorf("detectorbackfill: insert attempt for queue row %d: %w", rw.id, err)
		}
		affected, err := out.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("detectorbackfill: rows affected for queue row %d: %w", rw.id, err)
		}
		if affected == 0 {
			res.AlreadyRecorded++
			continue
		}

		tally(&res, rw.hit)
		if opts.Report != nil {
			if err := opts.Report(change(rw)); err != nil {
				return res, fmt.Errorf("detectorbackfill: report change for queue row %d: %w", rw.id, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("detectorbackfill: commit: %w", err)
	}
	return res, nil
}

// tally records one attributed row in the hit or miss bucket.
func tally(res *Result, hit bool) {
	if hit {
		res.Hits++
		return
	}
	res.Misses++
}

// change shapes a loaded row into the reported Change.
func change(rw row) Change {
	return Change{QueueID: rw.id, Lane: LaneName, Hit: rw.hit, AttemptedAt: rw.attemptedAt}
}

// alreadyRecorded reports whether lane_attempts already holds a detector row for
// this work_queue id.
func (b *Backfiller) alreadyRecorded(ctx context.Context, queueID int64) (bool, error) {
	var one int
	err := b.db.QueryRowContext(ctx,
		`SELECT 1 FROM lane_attempts WHERE queue_id = ? AND lane = ?`, queueID, LaneName).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("detectorbackfill: check existing attempt for queue row %d: %w", queueID, err)
	}
}

// countUncovered counts work_queue rows that carry no usable detection verdict.
// These cannot be attributed from recorded data and are reported as a remainder
// rather than estimated into the counters.
func (b *Backfiller) countUncovered(ctx context.Context) (int, error) {
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue
          WHERE instrumental_result IS NULL OR instrumental_result NOT IN (0, 1)`).Scan(&n); err != nil {
		return 0, fmt.Errorf("detectorbackfill: count uncovered rows: %w", err)
	}
	return n, nil
}

// load reads every work_queue row carrying a usable detection verdict.
//
// attempted_at is sourced from work_queue.updated_at, which is a PROXY, not an
// observation: the true detection time was never recorded. It is an upper bound,
// since updated_at advances on every later touch of the row.
//
// completed_at is deliberately NOT used, because it correlates with the verdict
// being counted. Traced across EVERY writer of instrumental_result, not just one
// path per verdict:
//
//	HIT  (=1): queue.SettleInstrumental stamps completed_at in the same UPDATE
//	           (status -> 'done'); the worker's detector-settle path reaches
//	           queue.Complete, which also stamps it. Both hit writers set it.
//	MISS (=0): queue.StampUnclassifiedMiss is the only writer. Its UPDATE is
//	           guarded `AND status = 'deferred'` -- the row is ALREADY deferred
//	           when the 0 is stamped -- and it touches neither completed_at nor
//	           status, so completed_at stays NULL.
//
// Both verdicts are stamped on rows that reached 'deferred'; the asymmetry is
// purely that the hit writers promote the row to 'done' with a timestamp and the
// miss writer leaves it deferred and untimestamped.
//
// Keying on completed_at would therefore drop misses at a higher rate than hits
// and reintroduce exactly the ratio skew this package exists to prevent. That is
// the same failure mode as a hits-only fill, arriving by a subtler route: a
// filter that correlates with the outcome corrupts a rendered rate far more than
// it costs precision.
//
// The choice does not affect the corrected figures in any case:
// reports.ProviderEffectiveness groups by lane and never filters or sorts on
// attempted_at.
func (b *Backfiller) load(ctx context.Context) ([]row, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT id, instrumental_result, updated_at FROM work_queue
          WHERE instrumental_result IN (0, 1)
          ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("detectorbackfill: query work_queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []row
	for rows.Next() {
		var (
			rw     row
			result int
		)
		if err := rows.Scan(&rw.id, &result, &rw.attemptedAt); err != nil {
			return nil, fmt.Errorf("detectorbackfill: scan work_queue row: %w", err)
		}
		rw.hit = result == 1
		out = append(out, rw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("detectorbackfill: iterate work_queue: %w", err)
	}
	return out, nil
}
