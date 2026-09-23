package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// wordRecheckWritable is the ONLY gate in front of the writer in recheck mode
// (#982): the result carries words the writer will land (its own predicate,
// lyrics.HasQualifyingWords; looser words would delete an owned .elrc and stamp
// served on a file without them), is not an instrumental, and the timing guard
// promotes it as-is. Anything else -- unsynced, instrumental, line-synced without
// words, or a word result the guard would demote or quarantine -- is never
// written, because WriteLRC removes the opposite sidecar and would replace the
// settled .lrc with a .txt or nothing (the downgrade trap, plan 2.4).
func wordRecheckWritable(song models.Song, audioSeconds int) bool {
	if song.Track.Instrumental == 1 || orchestrator.QualityOf(song) != orchestrator.QualityWordSynced || !lyrics.HasQualifyingWords(song) {
		return false
	}
	song.AudioDurationSeconds = audioSeconds
	decision, _, _ := lyrics.DecidePromotion(song)
	return decision == lyrics.PromoteAsIs
}

// wordOrchestrator is the recheck dispatch: the configured provider lanes that
// can serve word timings, in configured order, always ordered, committing only
// a word-synced result (plan 2.3). Built per recheck row from the live lanes
// (it holds no state of its own; the breakers live on the lanes). It is nil
// when no configured lane is word-capable.
func (w *Worker) wordOrchestrator() *orchestrator.Orchestrator {
	var lanes []*orchestrator.Lane
	for _, l := range w.lanes {
		if l.WordCapable() {
			lanes = append(lanes, l)
		}
	}
	if len(lanes) == 0 {
		return nil
	}
	orch, err := orchestrator.New(orchestrator.ModeOrdered, lanes...)
	if err == nil {
		err = orch.SetMinCommitQuality(orchestrator.QualityWordSynced)
	}
	if err != nil {
		// Unreachable: ordered mode, non-nil lanes, an in-range quality.
		slog.Error("worker: build word-recheck orchestrator", "error", err)
		return nil
	}
	if len(lanes) > 1 && w.scriptGuard != nil {
		orch.SetGuard(w.scriptGuard)
	}
	return orch
}

// WordGeneration exposes the generation this worker stamps, so the serve-mode
// recheck sweep (#1048) selects stale 'absent' verdicts against the lanes serve
// actually built rather than re-deriving them from config. Read it after the
// lane setters and before the worker loop starts: w.lanes is not synchronized.
func (w *Worker) WordGeneration() int64 { return w.wordGeneration() }

// wordGeneration is the word-capability generation of the configured lanes,
// stamped with every recheck verdict.
func (w *Worker) wordGeneration() int64 {
	names := make([]string, len(w.lanes))
	for i, l := range w.lanes {
		names[i] = l.Name()
	}
	return providers.WordGeneration(names)
}

// runWordRecheck processes a row MarkWordRecheckQueued flipped back into the
// queue (word_timing_state='queued', #982). Its ONLY question is "does a
// word-capable lane have word timings for this settled .lrc?", and the settle
// table (plan 2.4) keeps every answer off the counters an ordinary miss moves:
//
//   - served: a writable word result from a lane is written and the row
//     settles done + served.
//   - absent: every word lane answered with no usable words or no match
//     (usable = qualifying words the guard promotes as-is), or the words were
//     script- or verifier-rejected. NOTHING is written; done + absent.
//   - unanswered (a word lane did not answer: unknown, throttle, open breaker,
//     transport, verification error): re-deferred still 'queued', at most
//     maxWordRecheckWaits times, then un-flipped (queue.DeferWordRecheck).
//
// The cache is NOT consulted: an entry carries no lane or fetch time, so a
// cache-served rewrite would strip [source:]/[fetched:] from a settled file
// and NULL fetched_at. No path calls Defer, RetireMiss or Fail, and none writes
// lane_attempts or provider_outcomes: a recheck result would repoint the row's
// ordinary per-track hit history (#282), e.g. flip a Musixmatch hit to a miss
// when only its words were missing, and would count hits with no matching miss.
func (w *Worker) runWordRecheck(ctx context.Context, item queue.WorkItem, track models.Track) error {
	orch := w.wordOrchestrator()
	if orch == nil {
		// No lane can ever answer under this configuration, so the provider path
		// is exhausted for it. absent is stamped under THIS lane set's
		// generation, so adding a word-capable lane re-opens the row.
		return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
	}
	song, err := orch.FindLyrics(ctx, track, "")
	if err != nil {
		if orchestrator.ClassifyOutcome(err) == orchestrator.OutcomeBenignMiss && song.WordAnswer == models.WordAnswerAbsent {
			// Every word lane answered no match: terminal (maintainer-approved
			// default, plan section 7 question 1).
			w.consecutiveFailures = 0
			return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
		}
		return w.deferWordRecheck(ctx, item, err)
	}
	w.lastItemContactedProvider = contactedProvider(song)
	if wordRecheckWritable(song, track.TrackLength) {
		if reject, reason := w.guardReject(item, song); reject {
			slog.Info("worker word recheck: script guard rejected the word result", "id", item.ID, "reason", reason)
			return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
		}
		if verr := w.verify(ctx, item, song, Confidence(item.Inputs.Track, song.Track)); errors.Is(verr, errVerificationRejected) {
			return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
		} else if verr != nil {
			return w.deferWordRecheck(ctx, item, verr)
		}
		song.FetchedAt = w.now()
		if serr := w.store(ctx, track, song); serr != nil {
			return w.deferWordRecheck(ctx, item, serr)
		}
		return w.writeWordRecheck(ctx, item, track, song)
	}
	if song.WordAnswer == models.WordAnswerAbsent {
		// The gated orchestrator's aggregate: every word lane answered, none
		// with usable words (a held or unqualified word result included, plan
		// 2.4 rows 2-3; #1007 can retime them).
		w.consecutiveFailures = 0
		return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
	}
	// Some word lane did not answer (plan 2.4 row 4): retry, never absent.
	return w.deferWordRecheck(ctx, item, errNoWordAnswer)
}

// errNoWordAnswer is a healthy round-trip that left the word question open. It
// is not a failure and never feeds the worker's failure backoff.
var errNoWordAnswer = errors.New("worker: word recheck: no word answer")

// maxWordRecheckWaits bounds the re-parks of one unanswered recheck row,
// mirroring maxRefusedWaits (#950) and sharing its refused_waits counter.
const maxWordRecheckWaits = 3

// writeWordRecheck writes a writable word result and settles the row served.
// A write error defers the row: a companion write can fail after the .lrc
// landed, and the retry asks the lanes again.
func (w *Worker) writeWordRecheck(ctx context.Context, item queue.WorkItem, track models.Track, song models.Song) error {
	song.AudioDurationSeconds = track.TrackLength
	for _, p := range outputPaths(item.Inputs) {
		if err := w.writer.WriteLRC(song, p.Filename, p.Outdir); err != nil {
			return w.deferWordRecheck(ctx, item, fmt.Errorf("worker: write item %d output: %w", item.ID, err))
		}
	}
	ctxNoCancel := context.WithoutCancel(ctx)
	// provider_lane follows the [source:] now on disk, so it moves only after
	// the write (purgeprovenance compares the two). No provider_outcomes hit:
	// like lane_attempts, that instrument counts ordinary dispatches, and an
	// absent recheck has no symmetric miss to record.
	if err := w.queue.SetProviderLane(ctxNoCancel, item.ID, song.WinningLane); err != nil {
		slog.Warn("worker: stamp provider lane failed", "id", item.ID, "lane", song.WinningLane, "error", err)
	}
	w.stampCompletionProvenance(ctxNoCancel, item.ID, song)
	w.stampTimingOutcome(ctxNoCancel, item, song, lyrics.GuardDurationSeconds(song))
	w.consecutiveFailures = 0
	return w.settleWordRecheck(ctx, item, queue.WordTimingServed)
}

// settleWordRecheck settles the row done with its verdict in one statement.
// On failure the row is re-deferred (never failed), so it stays a recheck row.
func (w *Worker) settleWordRecheck(ctx context.Context, item queue.WorkItem, state string) error {
	err := w.queue.SettleWordRecheck(context.WithoutCancel(ctx), item.ID, state, w.wordGeneration())
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		slog.Warn("worker word recheck: row no longer a processing recheck row; leaving it", "id", item.ID)
		return nil
	}
	return w.deferWordRecheck(ctx, item, fmt.Errorf("worker: settle word recheck %s: %w", state, err))
}

// deferWordRecheck re-parks a recheck row whose word question went unanswered.
// The queue effect spends one refused_waits (never attempts/miss_count); the
// worker-level effects mirror the ordinary path: a shutdown releases the row
// untouched, a throttle or open breaker idles the pass, errNoWordAnswer resets
// the failure backoff, and a transport cause (verifier error included) feeds it.
func (w *Worker) deferWordRecheck(ctx context.Context, item queue.WorkItem, cause error) error {
	noCancel := context.WithoutCancel(ctx)
	if errors.Is(ctx.Err(), context.Canceled) && errors.Is(cause, context.Canceled) {
		if err := w.queue.Release(noCancel, item.ID); err != nil {
			return fmt.Errorf("worker: release word recheck %d after shutdown: %w", item.ID, err)
		}
		return nil
	}
	released, err := w.queue.DeferWordRecheck(noCancel, item.ID, w.circuitOpenDuration, maxWordRecheckWaits, cause.Error())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("worker: defer word recheck %d after %v: %w", item.ID, cause, err)
	}
	if released {
		slog.Warn("worker word recheck: word lane still unanswered after the wait budget; row returned to done unverdicted",
			"id", item.ID, "waits", maxWordRecheckWaits, "cause", cause)
	} else {
		slog.Debug("worker word recheck: unanswered; re-deferred", "id", item.ID, "retry_after", w.circuitOpenDuration, "cause", cause)
	}
	if errors.Is(cause, errNoWordAnswer) {
		w.consecutiveFailures = 0
		return nil
	}
	switch orchestrator.ClassifyOutcome(cause) {
	case orchestrator.OutcomeUnavailable:
		return errLanesUnavailable
	case orchestrator.OutcomeAuthRateLimit:
		w.consecutiveFailures = 0
		return errThrottled
	case orchestrator.OutcomeTransport:
		w.consecutiveFailures++
	case orchestrator.OutcomeSuccess, orchestrator.OutcomeBenignMiss, orchestrator.OutcomeLaneOutage,
		orchestrator.OutcomeLaneNotReady, orchestrator.OutcomeRefusedUntried:
		w.consecutiveFailures = 0
	}
	return nil
}

// wordLandingWriter is the part of *lyrics.LRCWriter the ordinary word stamp
// reads. A writer without it (a test double) stamps nothing.
type wordLandingWriter interface {
	WordSyncEnabled() bool
	WordsLanded(song models.Song, filename, outdir string) bool
}

// ordinaryWordVerdict is an ORDINARY completion's word verdict (#982 slice 4),
// or "" for none. Only a synced .lrc served by a word-capable lane, with
// word_sync_mode not off, carries one, so a txt, instrumental, refused or
// innertube/cache-served completion leaves the row a recheck candidate:
//
//   - served: the words LANDED at every output path (the writer's own
//     HasQualifyingWords, inline or in an owned companion), never merely
//     "WordTimings is non-empty" (the 3b C1 class).
//   - absent: the dispatch's aggregate answer is absent, i.e. every
//     word-capable lane answered with no words (orchestrator
//     ungatedWordAnswer). A lane never asked leaves it unknown: "".
func (w *Worker) ordinaryWordVerdict(item queue.WorkItem, song models.Song) string {
	lw, ok := w.writer.(wordLandingWriter)
	if !ok || !lw.WordSyncEnabled() || outcomeTypeFromSong(song) != outcomeTypeSynced || !providers.WordCapable(song.WinningLane) {
		return ""
	}
	landed := true
	for _, p := range outputPaths(item.Inputs) {
		landed = landed && lw.WordsLanded(song, p.Filename, p.Outdir)
	}
	if landed {
		return queue.WordTimingServed
	}
	if song.WordAnswer == models.WordAnswerAbsent {
		return queue.WordTimingAbsent
	}
	return ""
}

// stampWordTiming records an ordinary completion's word verdict before
// Complete, best-effort like its sibling stamps: a lost stamp leaves the row
// NULL (a recheck candidate), never a wrong verdict, so it must not cost the
// written result. A completion with no verdict clears a prior one (a retried
// or reopened row); the detector-instrumental and guard-reject settles, which
// return before this, clear it via clearWordTiming, so no served/absent
// outlives an ordinary completion that did not re-derive it.
func (w *Worker) stampWordTiming(ctxNoCancel context.Context, item queue.WorkItem, song models.Song) {
	state := w.ordinaryWordVerdict(item, song)
	if state == "" {
		w.clearWordTiming(ctxNoCancel, item)
		return
	}
	if err := w.queue.SetWordTimingState(ctxNoCancel, item.ID, state, w.wordGeneration(), time.Time{}); err != nil {
		slog.Warn("worker: stamp word timing state failed; continuing", "id", item.ID, "error", err)
		// A failed stamp must not leave a reopened row's PRIOR verdict standing:
		// a kept served/absent that this completion contradicts excludes the row
		// from rechecks. Falling back to NULL keeps the "lost stamp = candidate"
		// contract.
		if item.WordTimingState != state {
			w.clearWordTiming(ctxNoCancel, item)
		}
	}
}

// clearWordTiming drops a prior served/absent verdict the dequeued row carried
// (never 'queued'; ClearWordTimingState's guard), best-effort and before the
// row settles: a lost clear costs at most one stale verdict, a kept one would
// outlive the file it described. A row with no verdict issues no statement.
func (w *Worker) clearWordTiming(ctxNoCancel context.Context, item queue.WorkItem) {
	if item.WordTimingState != queue.WordTimingServed && item.WordTimingState != queue.WordTimingAbsent {
		return
	}
	if err := w.queue.ClearWordTimingState(ctxNoCancel, item.ID); err != nil {
		slog.Warn("worker: clear word timing state failed; continuing", "id", item.ID, "error", err)
	}
}
