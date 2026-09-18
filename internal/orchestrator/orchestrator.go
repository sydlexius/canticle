package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/models"
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
// FindLyrics takes the per-item sourcePath alongside the track (a file-reading
// lane such as the detector needs the on-disk audio path), so Orchestrator no
// longer matches the two-argument providers.Fetcher shape; the worker calls
// FindLyrics directly rather than holding it behind that interface.
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

// LaneNames returns the names of all lanes the orchestrator dispatches over, in
// priority order. Used by the worker miss-recording path to increment the miss
// counter for every active lane without touching the orchestrator's internal
// lane slice directly.
func (o *Orchestrator) LaneNames() []string {
	names := make([]string, len(o.lanes))
	for i, l := range o.lanes {
		names[i] = l.Name()
	}
	return names
}

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
func (o *Orchestrator) FindLyrics(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
	if o.mode == ModeParallel {
		return o.findParallel(ctx, track, sourcePath)
	}
	return o.findOrdered(ctx, track, sourcePath)
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
func (o *Orchestrator) findOrdered(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
	var r dispatchResult
	// attempted accumulates the names of lanes actually CONSULTED (the provider was
	// called), in order, so per-track hit/miss attribution counts only lanes that
	// were tried. Ordered mode stops at the first suitable result, so lanes after
	// the winner are never consulted and never appear here. Skipped (breaker-open)
	// lanes are excluded too: the provider was not called, so it was not attempted.
	//
	// Each entry carries the lane's locality alongside its name so the attribution
	// can tell the worker whether an outbound provider request was made (#534).
	var attempted []attemptedLane
	for _, lane := range o.lanes {
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}
		if r.haveHeld && lane.instrumentalOnly {
			// A suitable lyric is already held (#950), and an instrumental verdict
			// can never outrank words, so the detector is not run at all.
			continue
		}

		song, err := lane.FindLyrics(ctx, track, sourcePath)
		class := ClassifyOutcome(err)
		r.noteUntried(err, class, lane.Name(), lane.instrumentalOnly)

		if class == OutcomeUnavailable {
			// The breaker was open and the provider was not called. An unavailable
			// lane does not contribute to error ranking; it only matters when EVERY
			// lane was unavailable (handled by resolve via consulted == 0), or when
			// it holds back a timing-refused result (noteUntried above, #950).
			continue
		}
		r.consulted++
		attempted = append(attempted, attemptedLane{name: lane.Name(), local: lane.Local()})

		if err == nil {
			// acceptable, not bare IsSuitable (#950): a result the writer's
			// timing guard would refuse or demote does not end the dispatch.
			// It commits unless a held demotable lyric lands at least as well;
			// on that tie the earlier (higher-priority) lane keeps it. The rank
			// is what the writer lands (landedQuality), so a provider
			// instrumental carrying a subtitle line never replaces held words.
			kind := classifyCandidate(song, track, o.guard)
			switch kind {
			case candidateCommit:
				if !r.haveHeld || landedQuality(song, track) > QualityUnsynced {
					song.WinningLane = lane.Name()
					song.LaneAttempts = laneAttemptsFor(attempted, lane.Name())
					return song, nil
				}
				continue
			case candidateHold:
				r.hold(song, lane.Name())
				continue
			case candidateRetain, candidateRefused:
			}
			r.retainCandidate(song, lane.Name(), retainQuality(song, track), kind)
			continue
		}

		r.rankErr(err, class)
	}

	song, err := o.resolve(ctx, &r)
	// Attach per-track attribution to whatever resolve returns: a best-available
	// fallback names its serving lane as the hit -- including an exhausted
	// timing-refused result that lands nothing, which is still attributed to its
	// lane as the hit, exactly as before; an error (benign miss / transport)
	// returns no winner, so every attempted lane is recorded as a miss. The worker
	// persists these only on the success and benign-miss paths (not on hard
	// failures), so carrying them on the error song here is harmless.
	song.LaneAttempts = laneAttemptsFor(attempted, song.WinningLane)
	return song, err
}

// laneAttemptsFor builds the per-track attribution for the given attempted lane
// names: Hit is true for the lane equal to winner (the empty string means no
// winner -> all misses) and false for every other attempted lane.
func laneAttemptsFor(attempted []attemptedLane, winner string) []models.LaneAttempt {
	if len(attempted) == 0 {
		return nil
	}
	out := make([]models.LaneAttempt, len(attempted))
	for i, a := range attempted {
		out[i] = models.LaneAttempt{Lane: a.name, Hit: a.name == winner, Local: a.local}
	}
	return out
}

// attemptedLane is one consulted lane: its name and whether it resolved without
// an outbound provider request. Locality is captured at attempt time rather
// than looked up later, so the attribution stays correct even if the lane set
// is rebuilt between dispatch and use.
type attemptedLane struct {
	name  string
	local bool
}

// dispatchResult accumulates the cross-lane outcome state shared by both dispatch
// modes: the best-available fallback (highest quality wins; ties keep the first
// retained) and the highest-precedence error (Gap 4). Its zero value is ready to
// use (QualityNone, OutcomeSuccess).
type dispatchResult struct {
	bestSong    models.Song
	bestLane    string // lane name that provided bestSong
	haveBest    bool
	bestQuality Quality
	topErr      error
	topClass    OutcomeClass
	consulted   int
	// held is the first result that is SUITABLE but that the timing guard would
	// demote to .txt (MisSynced / degenerate, #950): the result a build before
	// #950 committed on the spot. It outranks every retained (non-suitable)
	// result, because it is real words the writer will land; the first one held
	// keeps it, so the earlier lane wins a tie.
	heldSong models.Song
	heldLane string
	haveHeld bool
	// bestRefused reports that bestSong is a result the timing guard would
	// quarantine (candidateRefused), as opposed to a script-guard rejection or
	// a below-unsynced result (#950).
	bestRefused bool
	// untriedErr is the first error from a lane that did NOT answer: breaker
	// open, throttled / auth-failing, or not ready (#950). A transport failure
	// is not recorded: that lane answered, badly, and a request-shape refusal
	// (403, stale client version) is not fixed by waiting.
	untriedErr error
	// untriedLane names the lane untriedErr came from.
	untriedLane string
}

// noteUntried records err if its class says the lane did not answer the
// catalog question. A benign miss is an answer, as is a transport failure; a
// detector outage says nothing about lyrics. None of those holds a refused
// result back. An instrumental-only lane (the detector) is NEVER untried,
// whatever its class: its only possible answer is an instrumental marker, which
// cannot turn a refused lyric into words, and an open detector breaker is
// reported before the lane even checks whether detection is enabled for the
// item, so counting it would park rows that can never run it (#950 review I2).
func (r *dispatchResult) noteUntried(err error, class OutcomeClass, laneName string, instrumentalOnly bool) {
	if instrumentalOnly {
		return
	}
	switch class {
	case OutcomeUnavailable, OutcomeAuthRateLimit, OutcomeLaneNotReady:
		if r.untriedErr == nil {
			r.untriedErr, r.untriedLane = err, laneName
		}
	case OutcomeSuccess, OutcomeBenignMiss, OutcomeLaneOutage, OutcomeTransport, OutcomeRefusedUntried:
	}
}

// retainCandidate is retain plus the bookkeeping of WHY the kept result is not
// committable: bestRefused follows whichever result retain actually keeps.
// Known tie edge (#950 review M4): a result both guard-rejected and timing-
// quarantined classifies as candidateRetain at QualityNone; a later pure
// refusal ties it and is not retained, so no wait happens. Accepted: that row
// settles down the script-guard path either way.
func (r *dispatchResult) retainCandidate(song models.Song, laneName string, q Quality, kind candidate) {
	if r.retain(song, laneName, q) {
		r.bestRefused = kind == candidateRefused
	}
}

// hold keeps song as the demotable fallback unless one is already held.
func (r *dispatchResult) hold(song models.Song, laneName string) {
	if !r.haveHeld {
		r.heldSong, r.heldLane, r.haveHeld = song, laneName, true
	}
}

// retain keeps song as the best-available fallback if its quality q outranks the
// current one. q is the caller's retainQuality: what the writer would actually
// land, so a timing-refused synced result cannot outrank a later usable one.
// It reports whether song replaced the kept result.
func (r *dispatchResult) retain(song models.Song, laneName string, q Quality) bool {
	if !r.haveBest || q > r.bestQuality {
		r.bestSong, r.bestQuality, r.haveBest, r.bestLane = song, q, true, laneName
		return true
	}
	return false
}

// rankErr keeps err if its class outranks the current top error (Gap 4).
// Whether the erroring lane answered at all is the caller's noteUntried.
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
//
// A held demotable lyric (#950) is returned ahead of any retained result: it
// is suitable, and the writer lands its words as .txt.
//
// A retained result the timing guard would QUARANTINE lands nothing, and it
// ranks QualityNone, so it is kept only when nothing else was. If every lane
// answered (a transport failure counts as an answer), it is returned with a nil
// error and the row settles terminal done with timing_outcome=categorical and
// nothing written, as before #950. If some lane did NOT answer (untriedErr), it
// is returned WITH ErrTimingRefusedUntried: the worker parks that one row for a
// bounded wait (queue.DeferRefused) and settles the carried song as above once
// the wait budget is spent. A demotable (held) result never waits: its .txt is
// real output.
func (o *Orchestrator) resolve(ctx context.Context, r *dispatchResult) (models.Song, error) {
	if r.haveHeld {
		r.heldSong.WinningLane = r.heldLane
		return r.heldSong, nil
	}
	if r.haveBest && r.bestRefused && r.untriedErr != nil {
		r.bestSong.WinningLane = r.bestLane
		return r.bestSong, &RefusedUntriedError{Lane: r.untriedLane, Cause: r.untriedErr.Error()}
	}
	if r.haveBest {
		r.bestSong.WinningLane = r.bestLane
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
