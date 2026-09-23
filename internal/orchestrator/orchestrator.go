package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
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
	// minCommit is the lowest quality that may END an ordered dispatch
	// (SetMinCommitQuality, #982). QualityNone, the zero value, is no gate.
	minCommit Quality
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

// SetMinCommitQuality gates which results may end an ordered dispatch (#982).
// With q set, a result the dispatch would otherwise commit ends it only when
// QualityOf reaches q; a lower one is kept (the best by landed quality, the
// earlier lane on a tie) and the remaining lanes are tried. When no lane
// reaches q the kept result is returned as the best available, ahead of a held
// demotable lyric unless it lands below unsynced (a result kept after a lyric
// was held always lands above it, so an unsynced one was kept first and wins
// the tie as the earlier lane). Every other rule is untouched: timing
// fall-through, held lyrics, ErrTimingRefusedUntried and first-lane ties.
//
// While gated, a result the orchestrator returns that does not itself carry
// served words has its WordAnswer replaced by the dispatch's aggregate: absent
// only when EVERY word-capable lane answered (a word answer, or a genuine
// no-match; see isWordNoMatch), otherwise unknown -- a lane that did not answer has not said "no words".
//
// QualityNone (the default) is no gate, which is byte-identical to a build
// without this option. Parallel mode refuses a gate: its race commits on
// arrival order, and word-only dispatch is ordered by design (#982 plan 2.3).
func (o *Orchestrator) SetMinCommitQuality(q Quality) error {
	if q < QualityNone || q > QualityWordSynced {
		return fmt.Errorf("orchestrator: minimum commit quality %d is outside [%d, %d]", q, QualityNone, QualityWordSynced)
	}
	if o.mode == ModeParallel && q > QualityNone {
		return fmt.Errorf("orchestrator: a minimum commit quality needs %q mode, not %q", ModeOrdered, o.mode)
	}
	o.minCommit = q
	return nil
}

// gateQuality is QualityOf for the commit gate: a word result reaches
// QualityWordSynced only when the writer would land its words
// (lyrics.HasQualifyingWords), so unusable words never end a word dispatch.
func gateQuality(song models.Song) Quality {
	if q := QualityOf(song); q != QualityWordSynced || lyrics.HasQualifyingWords(song) {
		return q
	}
	return QualitySynced
}

// isWordNoMatch reports a GENUINE no-match, the only error that answers the word
// question (#982); any other error leaves it unanswered, as absent is terminal.
func isWordNoMatch(err error) bool {
	return err != nil && (musixmatch.IsNoMatch(err) || petitlyrics.IsNoMatch(err))
}

// wordAnswerFor aggregates a gated dispatch's word answer from the number of
// word-capable lanes that answered.
func (o *Orchestrator) wordAnswerFor(answered int) models.WordAnswer {
	lanes := 0
	for _, l := range o.lanes {
		if l.WordCapable() {
			lanes++
		}
	}
	if lanes > 0 && answered == lanes {
		return models.WordAnswerAbsent
	}
	return models.WordAnswerUnknown
}

// answersWord reports whether a word-capable lane's result answered the word
// question: a word answer on a result, or a genuine no-match.
func answersWord(wordCapable bool, song models.Song, err error) bool {
	return wordCapable && (isWordNoMatch(err) || (err == nil && song.WordAnswer != models.WordAnswerUnknown))
}

// ungatedWordAnswer applies the aggregate to an UNGATED dispatch's result too
// (#982 slice 4, which stamps it on ordinary completions): the lane's own
// served or unknown stands, but its absent is terminal only when every
// word-capable lane answered. An ordinary dispatch ends at the first suitable
// lane, so a word lane after it was never asked and has not said "no words".
// A gated result already carries the aggregate, so this is a no-op there.
func (o *Orchestrator) ungatedWordAnswer(a models.WordAnswer, answered int) models.WordAnswer {
	if a != models.WordAnswerAbsent {
		return a
	}
	return o.wordAnswerFor(answered)
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
		if (r.haveHeld || r.haveGated) && lane.instrumentalOnly {
			// A suitable lyric is already held (#950), and an instrumental verdict
			// can never outrank words, so the detector is not run at all. A result
			// kept below the commit gate would have ended the dispatch ungated, so
			// the detector could not have run then either.
			continue
		}

		song, err := lane.FindLyrics(ctx, track, sourcePath)
		class := ClassifyOutcome(err)
		r.noteUntried(err, class, lane.Name(), lane.instrumentalOnly)
		if answersWord(lane.WordCapable(), song, err) {
			r.wordAnswered++
		}

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
					if gateQuality(song) < o.minCommit {
						// Below the commit gate (#982): keep it, try the next lane.
						r.gate(song, lane.Name(), landedQuality(song, track))
						continue
					}
					song.WinningLane = lane.Name()
					song.LaneAttempts = laneAttemptsFor(attempted, lane.Name())
					if o.minCommit > QualityNone && song.WordAnswer != models.WordAnswerServed {
						// Same aggregate as the resolve path: a lane's own absent is
						// terminal only when every word-capable lane answered (#982).
						song.WordAnswer = o.wordAnswerFor(r.wordAnswered)
					}
					song.WordAnswer = o.ungatedWordAnswer(song.WordAnswer, r.wordAnswered)
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
	if o.minCommit > QualityNone {
		// Nothing reached the gate, so even a lane's own "served" (words that
		// do not qualify, or a held word result) is not a usable answer: the
		// aggregate decides, absent only when every word lane answered (#982).
		song.WordAnswer = o.wordAnswerFor(r.wordAnswered)
	}
	song.WordAnswer = o.ungatedWordAnswer(song.WordAnswer, r.wordAnswered)
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
	// gated is the best result that would have committed but sat below the
	// minimum commit quality (#982); gatedQuality is its landed quality. Only
	// ever set while a gate is configured.
	gatedSong    models.Song
	gatedLane    string
	gatedQuality Quality
	haveGated    bool
	// wordAnswered counts word-capable lanes that answered the word question.
	wordAnswered int
}

// gate keeps song as the below-gate commit candidate if it lands strictly
// better than the one kept, so the earlier lane wins a tie.
func (r *dispatchResult) gate(song models.Song, laneName string, q Quality) {
	if !r.haveGated || q > r.gatedQuality {
		r.gatedSong, r.gatedLane, r.gatedQuality, r.haveGated = song, laneName, q, true
	}
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
	if r.haveGated && (!r.haveHeld || r.gatedQuality >= QualityUnsynced) {
		r.gatedSong.WinningLane = r.gatedLane
		return r.gatedSong, nil
	}
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
