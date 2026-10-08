package worker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/sidecar"
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
	if outcome == queue.SettleNoBlock {
		// An unblock removed the block after this pass observed it: the row must
		// retry the fetch rather than settle blocked with nothing to reopen it.
		slog.Info("worker: block was cleared mid-pass; releasing to retry", "id", item.ID)
	} else if outcome != queue.Settled {
		// The worker holds the row, so a non-Settled outcome means it was pruned
		// or moved underneath us: release rather than fail a row that may be gone.
		slog.Warn("worker blocked settle did not settle; releasing", "id", item.ID, "outcome", outcome)
	}
	if outcome != queue.Settled {
		if relErr := w.queue.Release(noCancel, item.ID); relErr != nil {
			return fmt.Errorf("worker: release unsettled blocked item %d: %w", item.ID, relErr)
		}
	}
	w.consecutiveFailures = 0
	return nil
}

// heldByLaterPath reports whether any of the output paths not yet visited already
// holds a sidecar. The writer refuses a blocked result before its no-downgrade
// guard, so it never says "kept" for a path it refuses; without this read a
// multi-path row whose first path is blocked would settle blocked beside a file
// on a later path. It uses lyrics.RungOnDisk, the same judgment the no-downgrade
// guard makes. A path whose name cannot be derived is skipped: the write loop
// would refuse it the same way. The outdir is read as given (no root
// re-confinement), so a symlinked outdir is judged at its configured spelling.
func heldByLaterPath(song models.Song, later []models.OutputPath) bool {
	for _, p := range later {
		fn, err := lyrics.SidecarName(song.Track.ArtistName, song.Track.TrackName, p.Filename, true)
		if err != nil {
			continue
		}
		fp := filepath.Join(p.Outdir, fn)
		if lyrics.RungOnDisk(fp, sidecar.List(p.Outdir)) > lyrics.RungNone {
			return true
		}
	}
	return false
}
