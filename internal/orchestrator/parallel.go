package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// laneResult carries one lane's outcome back from its dispatch goroutine. The
// buffered results channel provides synchronization, so no field needs a mutex.
type laneResult struct {
	song models.Song
	err  error
	name string // lane name, for WinningLane tagging
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
			results <- laneResult{song: song, err: err, name: lane.Name()}
		}()
	}

	var (
		r        dispatchResult
		heldSong models.Song
		heldLane string
		haveHeld bool // a suitable unsynced result is held, pending a synced upgrade
		upgrade  <-chan time.Time
		pending  = len(o.lanes)
		// consulted accumulates the names of lanes that ACTUALLY ran the provider
		// (everything that increments r.consulted), so per-track attribution counts
		// only attempted lanes -- mirroring ordered mode. A breaker-open lane
		// (OutcomeUnavailable, provider not called) and a canceled loser are
		// excluded, so neither is recorded as a spurious miss. On an early synced
		// win, lanes still in flight have not reported and so get no row: we do not
		// know their outcome, and recording them as misses would be the very
		// over-count this table exists to avoid.
		consulted []string
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
				consulted = append(consulted, res.name)
				switch {
				case res.err == nil && IsSuitable(res.song, o.guard):
					if QualityOf(res.song) >= QualitySynced {
						res.song.WinningLane = res.name
						// Attribute over the lanes consulted SO FAR (the winner plus any
						// lane that already reported a non-unavailable result): the winner
						// is the hit, every consulted loser a miss. This is the over-count
						// fix -- a lane that reported a miss before this synced winner is
						// recorded as a miss -- without inventing misses for breaker-open
						// or still-in-flight lanes that were never consulted.
						res.song.LaneAttempts = laneAttemptsFor(consulted, res.name)
						return res.song, nil // synced: commit now; defer cancels the losers.
					}
					// Suitable but unsynced: hold the first such result. Arm the upgrade
					// window only while another lane could still deliver a synced upgrade;
					// if this was the last lane, the pending == 0 check below commits it.
					if !haveHeld {
						heldSong, haveHeld, heldLane = res.song, true, res.name
					}
					if upgrade == nil && pending > 0 {
						upgrade = time.After(o.raceWait)
					}
				case res.err == nil:
					r.retain(res.song, res.name)
				default:
					r.rankErr(res.err, class)
				}
			}
			if pending == 0 {
				if err := ctx.Err(); err != nil {
					return models.Song{}, err
				}
				if haveHeld {
					heldSong.WinningLane = heldLane
					heldSong.LaneAttempts = laneAttemptsFor(consulted, heldLane)
					return heldSong, nil
				}
				song, err := o.resolve(ctx, &r)
				// Every lane has now reported, so consulted holds exactly the attempted
				// lanes (unavailable / canceled lanes excluded). A best-available
				// fallback names its serving lane as the hit; an error returns no
				// winner, so every consulted lane is a miss. The worker persists only on
				// the success and benign-miss paths, so attaching on the error song is
				// harmless.
				song.LaneAttempts = laneAttemptsFor(consulted, song.WinningLane)
				return song, err
			}
		case <-upgrade:
			// The window elapsed with no synced upgrade: commit the held unsynced,
			// unless the parent was canceled in the meantime.
			if err := ctx.Err(); err != nil {
				return models.Song{}, err
			}
			heldSong.WinningLane = heldLane
			heldSong.LaneAttempts = laneAttemptsFor(consulted, heldLane)
			return heldSong, nil
		}
	}
}
