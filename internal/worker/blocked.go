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
	// Nothing lands, so no word verdict or tier may describe a file (as on the
	// guard path). Both are best-effort and precede the atomic settle.
	w.clearWordTiming(noCancel, item)
	w.clearSyncTier(noCancel, item)
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
