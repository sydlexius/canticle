package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

// laneResult carries one lane's outcome back from its dispatch goroutine. The
// buffered results channel provides synchronization, so no field needs a mutex.
type laneResult struct {
	song models.Song
	err  error
}

// findParallel dispatches every lane concurrently and races the results:
//
//   - The first SUITABLE synced result wins immediately and cancels the rest.
//   - The first SUITABLE unsynced result is held and a bounded upgrade window is
//     armed (only while another lane could still deliver a synced upgrade). A synced
//     result arriving within the window preempts it; the window elapsing, or every
//     remaining lane reporting, commits the held unsynced result.
//   - Non-suitable results and errors fall through to the same resolution as ordered
//     mode (best-available > highest-precedence error > unavailable sentinel).
//
// Cancellation/leak contract: a child context is canceled on every return path via
// defer; the results channel is buffered to the launched-goroutine count so a losing
// lane's send never blocks after cancel; the collector discards canceled-lane
// results and does not wait for losers to drain.
func (o *Orchestrator) findParallel(ctx context.Context, track models.Track) (models.Song, error) {
	if err := ctx.Err(); err != nil {
		return models.Song{}, err
	}

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan laneResult, len(o.lanes))
	for _, lane := range o.lanes {
		lane := lane
		go func() {
			song, err := lane.FindLyrics(childCtx, track)
			results <- laneResult{song: song, err: err}
		}()
	}

	var (
		r        dispatchResult
		heldSong models.Song
		haveHeld bool // a suitable unsynced result is held, pending a synced upgrade
		upgrade  <-chan time.Time
		pending  = len(o.lanes)
	)

	for {
		select {
		case <-ctx.Done():
			// Parent cancellation (shutdown / request abort) beats any pending commit,
			// including a held unsynced result and the upgrade window: never turn a
			// canceled dispatch into a successful lyrics write.
			return models.Song{}, ctx.Err()
		case res := <-results:
			pending--
			class := ClassifyOutcome(res.err)
			switch {
			case errors.Is(res.err, context.Canceled):
				// A canceled loser, or a parent-canceled lane: no catalog signal, skip.
			case class == OutcomeUnavailable:
				// Breaker open, provider not called: skip (matters only if ALL unavailable).
			default:
				r.consulted++
				switch {
				case res.err == nil && IsSuitable(res.song, o.guard):
					if QualityOf(res.song) >= QualitySynced {
						return res.song, nil // synced: commit now; defer cancels the losers.
					}
					// Suitable but unsynced: hold the first such result. Arm the upgrade
					// window only while another lane could still deliver a synced upgrade;
					// if this was the last lane, the pending == 0 check below commits it.
					if !haveHeld {
						heldSong, haveHeld = res.song, true
					}
					if upgrade == nil && pending > 0 {
						upgrade = time.After(o.raceWait)
					}
				case res.err == nil:
					r.retain(res.song)
				default:
					r.rankErr(res.err, class)
				}
			}
			if pending == 0 {
				if err := ctx.Err(); err != nil {
					return models.Song{}, err
				}
				if haveHeld {
					return heldSong, nil
				}
				return o.resolve(ctx, &r)
			}
		case <-upgrade:
			// The window elapsed with no synced upgrade: commit the held unsynced,
			// unless the parent was canceled in the meantime.
			if err := ctx.Err(); err != nil {
				return models.Song{}, err
			}
			return heldSong, nil
		}
	}
}
