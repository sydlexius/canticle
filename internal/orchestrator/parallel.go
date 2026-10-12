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
	// local mirrors Lane.Local(): the lane resolved without an outbound provider
	// request. Carried per result so parallel-mode attribution reports locality
	// the same way ordered mode does (#534).
	local bool
	// instrumentalOnly mirrors Lane.instrumentalOnly: such a lane is never
	// counted as untried (noteUntried, #950).
	instrumentalOnly bool
	// wordCapable mirrors Lane.WordCapable() for the word-answer aggregate.
	wordCapable bool
	// open is the lane's breaker read right after it reported (noteTransport).
	open bool
}

// findParallel dispatches the provider lanes concurrently and races the results:
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
//
// An instrumental-only lane (the detector) never joins the race (#528): see
// stagedLanes. It runs on THIS goroutine, either before any lane goroutine
// starts or after every one has reported, so it never overlaps a provider
// request and a slow provider cannot lose to a fast instrumental verdict.
func (o *Orchestrator) findParallel(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
	if err := ctx.Err(); err != nil {
		return models.Song{}, err
	}

	var r dispatchResult
	var consulted []attemptedLane // shared with the staged lanes; described below
	// Pre stage: no goroutine exists yet, so a commit here returns with no
	// provider asked. That is the whole detector-first guarantee.
	pre, race, post := o.stagedLanes()
	for _, lane := range pre {
		song, done := o.runStaged(ctx, lane, track, sourcePath, &r, &consulted)
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}
		if done {
			return song, nil
		}
	}
	preConsulted := r.consulted
	if len(race) == 0 {
		// Nothing to race: the select loop would wait on a channel nobody writes.
		return o.finishParallel(ctx, post, track, sourcePath, &r, consulted, 0)
	}

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan laneResult, len(race))
	for _, lane := range race {
		lane := lane
		go func() {
			song, err := lane.FindLyrics(childCtx, track, sourcePath)
			results <- laneResult{song: song, err: err, name: lane.Name(), local: lane.Local(), instrumentalOnly: lane.instrumentalOnly, wordCapable: lane.WordCapable(), open: lane.open()}
		}()
	}

	var (
		heldSong models.Song
		heldLane string
		haveHeld bool // a suitable unsynced result is held, pending a synced upgrade
		upgrade  <-chan time.Time
		pending  = len(race)
		// INVARIANT for the #534 pacing signal: a lane still in flight when an
		// early winner returns is canceled and never lands in consulted, so its
		// locality is absent from the attribution. That is safe only while no
		// LOCAL lane races a provider lane. The detector lane is local and is
		// installed under parallel dispatch, but it is staged, never raced
		// (#528): a pre-stage win returns before any provider request starts, and
		// a post-stage win comes after every racing lane has reported (a
		// breaker-open lane was never asked, and a post lane runs only when at
		// least one was). So local-only attribution means no provider was asked.
		//
		// A local lane that is NOT instrumental-only (the extracted lane in #538)
		// would race, and could win while a provider request is in flight,
		// leaving only Local:true attribution and skipping the pause --
		// under-throttling the provider. Propagate an explicit request-started
		// signal from the lane before adding one.
		//
		// consulted accumulates the names of lanes that ACTUALLY ran the provider
		// (everything that increments r.consulted), so per-track attribution counts
		// only attempted lanes -- mirroring ordered mode. A breaker-open lane
		// (OutcomeUnavailable, provider not called) and a canceled loser are
		// excluded, so neither is recorded as a spurious miss. On an early synced
		// win, lanes still in flight have not reported and so get no row: we do not
		// know their outcome, and recording them as misses would be the very
		// over-count this table exists to avoid.
		//
		// wordAnswered counts word-capable lanes that answered the word question
		// (answersWord), for ungatedWordAnswer.
		wordAnswered int
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
			// A canceled lane classifies as transport, so it is never untried.
			r.noteUntried(res.err, class, res.name, res.instrumentalOnly)
			if answersWord(res.wordCapable, res.song, res.err) {
				wordAnswered++
			}
			switch {
			case errors.Is(res.err, context.Canceled):
				// A canceled loser, or a parent-canceled lane: no catalog signal, skip.
			case class == OutcomeUnavailable:
				// Breaker open, provider not called: skip (matters only if ALL
				// unavailable, or to hold back a timing-refused result, #950).
			default:
				r.consult(res.local)
				consulted = append(consulted, attemptedLane{name: res.name, local: res.local})
				kind := candidateRetain
				if res.err == nil {
					kind = classifyCandidate(ctx, res.song, track, o.guard, o.blocks)
				}
				switch {
				case res.err == nil && (kind == candidateCommit || kind == candidateHold):
					// #950: only a result the timing guard promotes as-is may commit
					// as synced and cancel the rest. A demotable one lands as .txt, so
					// it takes the held-unsynced path below, exactly like any other
					// suitable unsynced result (and so still outranks a guard-rejected
					// one). A quarantined result is never suitable here. While a
					// lyric is held, a result commits over it only if what the writer
					// lands outranks it (landedQuality), so a provider instrumental
					// carrying a subtitle line never replaces held words.
					if kind == candidateCommit && QualityOf(res.song) >= QualitySynced &&
						(!haveHeld || landedQuality(res.song, track) > QualityUnsynced) {
						res.song.WinningLane = res.name
						// Attribute over the lanes consulted SO FAR (the winner plus any
						// lane that already reported a non-unavailable result): the winner
						// is the hit, every consulted loser a miss. This is the over-count
						// fix -- a lane that reported a miss before this synced winner is
						// recorded as a miss -- without inventing misses for breaker-open
						// or still-in-flight lanes that were never consulted.
						res.song.LaneAttempts = laneAttemptsFor(consulted, res.name)
						res.song.WordAnswer = o.ungatedWordAnswer(res.song.WordAnswer, wordAnswered)
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
				case res.err == nil && kind == candidateBlocked:
					// Blocked (#1394): an answer that is neither held nor retained.
					r.blocked = true
					// Not a provider miss, not a word answer (see findOrdered).
					consulted = consulted[:len(consulted)-1]
					if answersWord(res.wordCapable, res.song, nil) {
						wordAnswered--
					}
				case res.err == nil:
					r.retainCandidate(res.song, res.name, retainQuality(res.song, track), kind)
				default:
					r.noteTransport(class, res.open)
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
					heldSong.WordAnswer = o.ungatedWordAnswer(heldSong.WordAnswer, wordAnswered)
					return heldSong, nil
				}
				if r.consulted == preConsulted {
					// No racing lane was asked (every breaker open). A post lane
					// answers only after an asked provider, so the row waits.
					post = nil
				}
				return o.finishParallel(ctx, post, track, sourcePath, &r, consulted, wordAnswered)
			}
		case <-upgrade:
			// The window elapsed with no synced upgrade: commit the held unsynced,
			// unless the parent was canceled in the meantime.
			if err := ctx.Err(); err != nil {
				return models.Song{}, err
			}
			heldSong.WinningLane = heldLane
			heldSong.LaneAttempts = laneAttemptsFor(consulted, heldLane)
			heldSong.WordAnswer = o.ungatedWordAnswer(heldSong.WordAnswer, wordAnswered)
			return heldSong, nil
		}
	}
}

// stagedLanes splits the lane list for parallel dispatch. Instrumental-only
// lanes never race: those ahead of the first racing lane run before the
// fan-out (pre), those after it run only when the race committed nothing (post).
//
// A staged lane BETWEEN two racing lanes is a post lane: it runs after the
// whole race, not at its position, where ordered mode would ask it in
// position. The worker only ever installs the lane first or last.
func (o *Orchestrator) stagedLanes() (pre, race, post []*Lane) {
	for _, lane := range o.lanes {
		switch {
		case !lane.instrumentalOnly:
			race = append(race, lane)
		case len(race) == 0:
			pre = append(pre, lane)
		default:
			post = append(post, lane)
		}
	}
	return pre, race, post
}

// runStaged asks one instrumental-only lane outside the race and folds its
// outcome into r and consulted by the same rules findOrdered applies to a
// lane. done reports that song is the dispatch result.
//
// It must be called on the dispatch goroutine, never from a lane goroutine:
// the worker's memoizing detector is not safe for concurrent use. Such a lane
// answers no word question, so the word-answer count is not touched.
func (o *Orchestrator) runStaged(ctx context.Context, lane *Lane, track models.Track, sourcePath string, r *dispatchResult, consulted *[]attemptedLane) (song models.Song, done bool) {
	song, err := lane.FindLyrics(ctx, track, sourcePath)
	class := ClassifyOutcome(err)
	r.noteUntried(err, class, lane.Name(), lane.instrumentalOnly)
	if class == OutcomeUnavailable {
		// Breaker open, the lane was not run: skipped and not attributed.
		return models.Song{}, false
	}
	r.consult(lane.Local())
	*consulted = append(*consulted, attemptedLane{name: lane.Name(), local: lane.Local()})
	if err != nil {
		r.noteTransport(class, lane.open())
		r.rankErr(err, class)
		return models.Song{}, false
	}
	switch kind := classifyCandidate(ctx, song, track, o.guard, o.blocks); kind {
	case candidateCommit:
		// No held-lyric check, unlike findOrdered: a staged lane runs only while
		// nothing is held (before the race, or at its no-result tail).
		song.WinningLane = lane.Name()
		song.LaneAttempts = laneAttemptsFor(*consulted, lane.Name())
		return song, true
	case candidateBlocked:
		// Not a miss (see findOrdered): keep the lane out of the attribution.
		r.blocked = true
		*consulted = (*consulted)[:len(*consulted)-1]
	case candidateHold, candidateRetain, candidateRefused:
		// Not reachable for an instrumental verdict (never timing-judged).
		r.retainCandidate(song, lane.Name(), retainQuality(song, track), kind)
	}
	return models.Song{}, false
}

// finishParallel is the no-result tail: every racing lane has reported and
// nothing is held. The post-stage lanes run here (findOrdered likewise skips an
// instrumental-only lane once a lyric is held), then the shared resolution. The
// caller passes no post lanes when racing lanes exist and none could be asked.
func (o *Orchestrator) finishParallel(ctx context.Context, post []*Lane, track models.Track, sourcePath string, r *dispatchResult, consulted []attemptedLane, wordAnswered int) (models.Song, error) {
	for _, lane := range post {
		song, done := o.runStaged(ctx, lane, track, sourcePath, r, &consulted)
		if err := ctx.Err(); err != nil {
			return models.Song{}, err
		}
		if done {
			return song, nil
		}
	}
	song, err := o.resolve(ctx, r)
	// Every lane has now reported, so consulted holds exactly the attempted
	// lanes (unavailable / canceled lanes excluded). A best-available
	// fallback names its serving lane as the hit; an error returns no
	// winner, so every consulted lane is a miss. The worker persists only on
	// the success and benign-miss paths, so attaching on the error song is
	// harmless.
	song.LaneAttempts = laneAttemptsFor(consulted, song.WinningLane)
	song.WordAnswer = o.ungatedWordAnswer(song.WordAnswer, wordAnswered)
	return song, err
}
