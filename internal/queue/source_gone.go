package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/db"
)

// GoneOutcome is what SettleGoneUpgradeTrip did with a trip whose source file
// the worker found provably gone.
type GoneOutcome int

const (
	// GoneNotTrip means the row is not a processing upgrade trip (a lost race);
	// nothing changed.
	GoneNotTrip GoneOutcome = iota
	// GoneMarked means first sighting (or the mark is younger than confirmAfter). The
	// trip settled back to done as an ANSWERED miss, so the escalating hold
	// applies (#1118), and source_gone_at is recorded.
	GoneMarked
	// GoneRetired means a later sighting, with the mark at least confirmAfter old.
	GoneRetired
)

// SettleGoneUpgradeTrip settles a processing upgrade trip whose source file the
// caller proved gone (#1597). Gone is one observation, not a verdict (downtime
// is not absence), so it takes two sightings separated by confirmAfter:
//
//   - the first records source_gone_at and settles the trip as an answered miss
//     (status 'done', last_error ”, attempts 0, upgrade_miss_count + 1), what
//     SettleUpgradeTrip(answered=true) leaves, so the row waits out the
//     escalating hold and the file record (outcome_type, sync_tier, completed_at)
//     is untouched; a younger existing mark is kept, never reset;
//   - a later one, with the mark at least confirmAfter old, retires the row to
//     the state prune's retirement leaves: status 'done', completed_at now,
//     last_error UnresolvableGoneError, the mark KEPT, linked scan_results
//     written back done like RetireMiss. That takes it out of the upgrade sweep
//     (which needs an empty last_error) and the word recheck. The 053 trigger
//     clears upgrade_queued.
//
// The kept source_gone_at is how prune and enqueue_move tell this retirement
// from a never-fetched one: they resurrect only a sentinel row with no mark.
func (q *DBQueue) SettleGoneUpgradeTrip(ctx context.Context, id int64, confirmAfter time.Duration) (GoneOutcome, error) {
	out := GoneNotTrip
	err := db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		out = GoneNotTrip
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("queue: begin settle gone trip tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := q.now()
		var confirmed bool
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(source_gone_at <= ?, 0) FROM work_queue
             WHERE id = ? AND status = 'processing' AND upgrade_queued = 1`,
			formatTime(now.Add(-confirmAfter)), id).Scan(&confirmed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("queue: read gone mark id %d: %w", id, err)
		}
		if confirmed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE work_queue SET status = 'done', completed_at = ?, last_error = ?, refused_waits = 0,
                     attempts = 0
                 WHERE id = ?`, formatTime(now), UnresolvableGoneError, id); err != nil {
				return fmt.Errorf("queue: retire gone id %d: %w", id, err)
			}
			if err := writeBackScanResultsDone(ctx, tx, id); err != nil {
				return fmt.Errorf("queue: retire gone scan_results writeback: %w", err)
			}
			out = GoneRetired
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE work_queue SET status = 'done', last_error = '', refused_waits = 0, attempts = 0,
                     upgrade_miss_count = upgrade_miss_count + 1,
                     source_gone_at = COALESCE(source_gone_at, ?)
                 WHERE id = ?`, formatTime(now), id); err != nil {
				return fmt.Errorf("queue: mark gone id %d: %w", id, err)
			}
			out = GoneMarked
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("queue: commit settle gone trip tx: %w", err)
		}
		return nil
	})
	if err != nil {
		return GoneNotTrip, err
	}
	return out, nil
}

// ClearSourceGone drops the source-gone sighting mark: the file or its output
// directory is back, so the next gone sighting starts the two-sighting count
// over (#1597). A row with no mark is a no-op.
func (q *DBQueue) ClearSourceGone(ctx context.Context, id int64) error {
	return db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		if _, err := q.db.ExecContext(ctx,
			`UPDATE work_queue SET source_gone_at = NULL WHERE id = ? AND source_gone_at IS NOT NULL`, id); err != nil {
			return fmt.Errorf("queue: clear source gone id %d: %w", id, err)
		}
		return nil
	})
}
