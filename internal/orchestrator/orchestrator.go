package orchestrator

import (
	"context"
	"fmt"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

// Mode names the dispatch strategy. Only "ordered" is implemented; "parallel"
// is reserved by the design (docs/multi-provider-orchestration.md) and rejected
// by New until built.
const (
	// ModeOrdered queries lanes one at a time in priority order and returns the
	// first suitable result.
	ModeOrdered = "ordered"
)

// Orchestrator dispatches a lyrics lookup across an ordered set of provider
// lanes. With a single Musixmatch lane it is a behavior-preserving pass-through
// of the worker's prior single-fetch path: the one lane's breaker carries the
// throttle state, and a suitable / best-available / classified-error result
// flows back unchanged.
//
// It satisfies providers.Fetcher, so the worker can hold it in place of a bare
// fetcher.
type Orchestrator struct {
	lanes []*Lane
	guard ScriptGuard
	mode  string
}

// New builds an ordered orchestrator over lanes in priority order. mode must be
// "ordered" (the default for an empty string); any other value is rejected,
// since parallel dispatch is not implemented yet.
func New(mode string, lanes ...*Lane) (*Orchestrator, error) {
	if mode == "" {
		mode = ModeOrdered
	}
	if mode != ModeOrdered {
		return nil, fmt.Errorf("orchestrator: unsupported mode %q (only %q is implemented)", mode, ModeOrdered)
	}
	return &Orchestrator{lanes: lanes, mode: mode}, nil
}

// SetGuard installs the suitability script guard. A nil or disabled guard
// imposes no script filtering on the suitability decision. The worker keeps its
// own guard for the terminal policy-rejection path; this guard only governs
// whether the orchestrator advances to the next lane.
func (o *Orchestrator) SetGuard(g ScriptGuard) { o.guard = g }

// FindLyrics runs the ordered dispatch:
//
//   - Iterate lanes in priority order. Return the first SUITABLE result
//     immediately (the next lane is never consulted).
//   - An unavailable (open breaker) or unsuitable lane is skipped; the best
//     result seen so far (highest quality, possibly instrumental) is retained.
//   - If no lane yields a suitable result but at least one returned some result,
//     return that best-available result with a nil error so the worker writes
//     the instrumental / unsynced fallback.
//   - If every lane errored, return the highest-precedence error (Gap 4).
//   - If every available lane's breaker was open, return ErrLaneUnavailable.
func (o *Orchestrator) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	var (
		bestSong    models.Song
		haveBest    bool
		bestQuality = QualityNone

		topErr   error
		topClass = OutcomeSuccess

		consulted int
	)

	for _, lane := range o.lanes {
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}

		song, err := lane.FindLyrics(ctx, track)
		class := ClassifyOutcome(err)

		if class == OutcomeUnavailable {
			// The breaker was open and the provider was not called. An unavailable
			// lane does not contribute to error ranking; it only matters when EVERY
			// lane was unavailable (handled below via consulted == 0).
			continue
		}
		consulted++

		if err == nil {
			if IsSuitable(song, o.guard) {
				return song, nil
			}
			// Not suitable on its own, but retain it as a best-available fallback
			// (the highest quality wins; ties keep the earliest, higher-priority lane).
			if q := QualityOf(song); !haveBest || q > bestQuality {
				bestSong, bestQuality, haveBest = song, q, true
			}
			continue
		}

		// Track the highest-precedence error across consulted lanes (Gap 4).
		if topErr == nil || class.precedence() > topClass.precedence() {
			topErr, topClass = err, class
		}
	}

	// A best-available result (instrumental / unsynced / guard-rejected) is
	// returned ahead of any error: the worker writes the best we have rather than
	// backing off, exactly as the single-lane path does today.
	if haveBest {
		return bestSong, nil
	}

	// No result at all. If at least one lane was actually consulted, surface the
	// highest-precedence error. If every lane was unavailable (breaker open),
	// surface the unavailable sentinel so the worker releases the item.
	if consulted == 0 && len(o.lanes) > 0 {
		return models.Song{}, ErrLaneUnavailable
	}
	if topErr != nil {
		return models.Song{}, topErr
	}
	// No lanes configured at all.
	return models.Song{}, ErrLaneUnavailable
}
