package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

// Mode names the dispatch strategy (docs/multi-provider-orchestration.md).
const (
	// ModeOrdered queries lanes one at a time in priority order and returns the
	// first suitable result.
	ModeOrdered = "ordered"
	// ModeParallel dispatches every available lane concurrently and races them:
	// the first synced (strictly highest-quality) result wins immediately, while a
	// faster unsynced result is held for a bounded window so a slower synced result
	// can preempt it. It makes more upstream calls than ordered.
	ModeParallel = "parallel"
)

// DefaultRaceWait is the parallel-mode upgrade window applied when SetRaceWait is
// not called (or is called with a non-positive value). It mirrors the config
// default (providers.race_wait_seconds = 2).
const DefaultRaceWait = 2 * time.Second

// Orchestrator dispatches a lyrics lookup across a set of provider lanes. With a
// single Musixmatch lane it is a behavior-preserving pass-through of the worker's
// prior single-fetch path: the one lane's breaker carries the throttle state, and
// a suitable / best-available / classified-error result flows back unchanged.
//
// It satisfies providers.Fetcher, so the worker can hold it in place of a bare
// fetcher.
type Orchestrator struct {
	lanes []*Lane
	guard ScriptGuard
	mode  string
	// raceWait bounds the parallel-mode upgrade window (synced preempts a held
	// unsynced). Unused in ordered mode. Defaults to DefaultRaceWait.
	raceWait time.Duration
}

// New builds an orchestrator over lanes in priority order. mode must be "ordered"
// (the default for an empty string) or "parallel"; any other value is rejected.
func New(mode string, lanes ...*Lane) (*Orchestrator, error) {
	if mode == "" {
		mode = ModeOrdered
	}
	if mode != ModeOrdered && mode != ModeParallel {
		return nil, fmt.Errorf("orchestrator: unsupported mode %q (supported: %q, %q)", mode, ModeOrdered, ModeParallel)
	}
	if len(lanes) == 0 {
		return nil, fmt.Errorf("orchestrator: at least one lane is required")
	}
	for i, l := range lanes {
		if l == nil {
			return nil, fmt.Errorf("orchestrator: lane %d is nil", i)
		}
	}
	// Defensively copy so a caller mutating its slice after construction cannot
	// alter the orchestrator's dispatch order or inject a nil lane.
	owned := append([]*Lane(nil), lanes...)
	return &Orchestrator{lanes: owned, mode: mode, raceWait: DefaultRaceWait}, nil
}

// SetGuard installs the suitability script guard. A nil or disabled guard
// imposes no script filtering on the suitability decision. The worker keeps its
// own guard for the terminal policy-rejection path; this guard only governs
// whether the orchestrator advances to the next lane.
func (o *Orchestrator) SetGuard(g ScriptGuard) { o.guard = g }

// SetRaceWait sets the parallel-mode upgrade window read once per dispatch. A
// non-positive value is ignored so the constructed DefaultRaceWait is preserved.
// It has no effect in ordered mode.
func (o *Orchestrator) SetRaceWait(d time.Duration) {
	if d <= 0 {
		return
	}
	o.raceWait = d
}

// FindLyrics dispatches the lookup using the configured mode. Ordered mode walks
// lanes in priority order; parallel mode races them with a bounded synced-upgrade
// window. Both share the same suitability rule and the same resolution precedence
// (best-available > highest-precedence error > unavailable sentinel).
func (o *Orchestrator) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	if o.mode == ModeParallel {
		return o.findParallel(ctx, track)
	}
	return o.findOrdered(ctx, track)
}

// findOrdered iterates lanes in priority order:
//
//   - Return the first SUITABLE result immediately (the next lane is never
//     consulted).
//   - An unavailable (open breaker) or unsuitable lane is skipped; the best result
//     seen so far (highest quality, possibly instrumental) is retained.
//   - If no lane yields a suitable result but at least one returned some result,
//     return that best-available result with a nil error so the worker writes the
//     instrumental / unsynced fallback.
//   - If every lane errored, return the highest-precedence error (Gap 4).
//   - If every available lane's breaker was open, return ErrLaneUnavailable.
func (o *Orchestrator) findOrdered(ctx context.Context, track models.Track) (models.Song, error) {
	var r dispatchResult
	for _, lane := range o.lanes {
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}

		song, err := lane.FindLyrics(ctx, track)
		class := ClassifyOutcome(err)

		if class == OutcomeUnavailable {
			// The breaker was open and the provider was not called. An unavailable
			// lane does not contribute to error ranking; it only matters when EVERY
			// lane was unavailable (handled by resolve via consulted == 0).
			continue
		}
		r.consulted++

		if err == nil {
			if IsSuitable(song, o.guard) {
				return song, nil
			}
			r.retain(song)
			continue
		}

		r.rankErr(err, class)
	}

	return o.resolve(ctx, &r)
}

// dispatchResult accumulates the cross-lane outcome state shared by both dispatch
// modes: the best-available fallback (highest quality wins; ties keep the first
// retained) and the highest-precedence error (Gap 4). Its zero value is ready to
// use (QualityNone, OutcomeSuccess).
type dispatchResult struct {
	bestSong    models.Song
	haveBest    bool
	bestQuality Quality
	topErr      error
	topClass    OutcomeClass
	consulted   int
}

// retain keeps song as the best-available fallback if it outranks the current one.
func (r *dispatchResult) retain(song models.Song) {
	if q := QualityOf(song); !r.haveBest || q > r.bestQuality {
		r.bestSong, r.bestQuality, r.haveBest = song, q, true
	}
}

// rankErr keeps err if its class outranks the current top error (Gap 4).
func (r *dispatchResult) rankErr(err error, class OutcomeClass) {
	if r.topErr == nil || class.precedence() > r.topClass.precedence() {
		r.topErr, r.topClass = err, class
	}
}

// resolve applies the shared final precedence once every lane has reported and no
// suitable result was committed: a best-available result (instrumental / unsynced /
// guard-rejected) is returned ahead of any error so the worker writes the best we
// have rather than backing off. With no result at all, the highest-precedence
// error is surfaced; if every lane was unavailable (breaker open) the unavailable
// sentinel is returned so the worker releases the item, unless the parent context
// was canceled, in which case its error wins.
func (o *Orchestrator) resolve(ctx context.Context, r *dispatchResult) (models.Song, error) {
	if r.haveBest {
		return r.bestSong, nil
	}
	if r.consulted == 0 && len(o.lanes) > 0 {
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}
		return models.Song{}, ErrLaneUnavailable
	}
	if r.topErr != nil {
		return models.Song{}, r.topErr
	}
	// No lanes configured at all.
	return models.Song{}, ErrLaneUnavailable
}
