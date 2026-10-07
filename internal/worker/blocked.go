package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sydlexius/canticle/internal/queue"
)

// settleBlocked finishes a row whose every available result is blocked for its
// identity (orchestrator.ErrAllResultsBlocked, or the writer's lyrics.ErrBlocked
// backstop), #1395. The row settles done with outcome_type='blocked'. It is NOT a
// miss: no miss_count, attempts or lane attempt is charged, and nothing retries
// it on a timer. Only clearing the block reopens it (queue.ReopenBlockedTx).
//
// The settle is PERMANENT until the block is cleared. A lane that missed, or was
// in outage, when the others answered blocked is NOT retried: the issue asks for
// no timer retry, and a timer would re-ask the blocked lanes forever for a body
// the operator has already rejected. Clearing the block is the only way back.
//
// Combined with an open breaker or an unanswered lane, the dispatch first parks
// just this row through the bounded refused-wait (deferRefusedUntried); once that
// budget is spent the same all-blocked error arrives here and the row settles.
//
// An upgrade trip keeps its settled file record: it only disarms the trip.
func (w *Worker) settleBlocked(ctx context.Context, item queue.WorkItem) error {
	slog.Info("worker: every result is blocked for this track", "id", item.ID)
	if item.UpgradeQueued {
		return w.settleUpgradeTrip(ctx, item)
	}
	noCancel := context.WithoutCancel(ctx)
	w.stampDetectorMissTelemetry(noCancel, item.ID)
	// Nothing lands, so the settle statement itself clears the lane, tier and
	// word verdict (queue.SettleBlocked): no best-effort pre-clear is needed.
	outcome, err := w.queue.SettleBlocked(noCancel, item.ID)
	if err != nil {
		return w.fail(ctx, item, fmt.Errorf("worker: settle blocked item %d: %w", item.ID, err))
	}
	if outcome != queue.Settled {
		// The worker holds the row, so a non-Settled outcome means it was pruned
		// or moved underneath us: release rather than fail a row that may be gone.
		slog.Warn("worker blocked settle did not settle; releasing", "id", item.ID, "outcome", outcome)
		if relErr := w.queue.Release(noCancel, item.ID); relErr != nil {
			return fmt.Errorf("worker: release unsettled blocked item %d: %w", item.ID, relErr)
		}
	}
	w.consecutiveFailures = 0
	return nil
}
