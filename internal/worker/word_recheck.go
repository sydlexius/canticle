package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// wordRecheckWritable is the ONLY gate in front of the writer in recheck mode
// (#982): the result carries word timings on its line cues, is not an
// instrumental, and the timing guard promotes it as-is, so the writer lands a
// synced .lrc. Anything else -- unsynced, instrumental, line-synced without
// words, or a word result the guard would demote or quarantine -- is never
// written, because WriteLRC removes the opposite sidecar and would replace the
// settled .lrc with a .txt or nothing (the downgrade trap, plan 2.4).
func wordRecheckWritable(song models.Song, audioSeconds int) bool {
	if song.Track.Instrumental == 1 || orchestrator.QualityOf(song) != orchestrator.QualityWordSynced {
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
//   - served: a writable word result (cache or lane) is written and the row
//     settles done + served.
//   - absent: every word lane answered with no words or no match, or its words
//     are unusable here (demoted, quarantined, script- or verifier-rejected). NOTHING is
//     written and the row settles done + absent.
//   - unanswered (unknown word answer, throttle, open breaker, transport,
//     verification error): the row is re-deferred still 'queued'.
//
// No path calls Defer, RetireMiss or Fail, so miss_count, attempts and the
// miss-retirement budget are never touched, and lane_attempts is written only
// on served.
func (w *Worker) runWordRecheck(ctx context.Context, item queue.WorkItem, track models.Track) error {
	ctxNoCancel := context.WithoutCancel(ctx)
	orch := w.wordOrchestrator()
	if orch == nil {
		// No lane can ever answer under this configuration, so the provider path
		// is exhausted for it. absent is stamped under THIS lane set's
		// generation, so adding a word-capable lane re-opens the row.
		return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
	}
	var cached models.Song
	_, err := w.cache.LookupAccepted(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(track.TrackLength),
		func(raw string) bool {
			cached = lyrics.DecodeCachedSong(raw, track)
			return wordRecheckWritable(cached, track.TrackLength)
		})
	switch {
	case err == nil:
		// A cached word result is written with zero provider requests.
		w.lastItemContactedProvider = false
		return w.writeWordRecheck(ctx, item, track, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return w.deferWordRecheck(ctx, item, fmt.Errorf("worker: lookup cache: %w", err))
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
		w.recordHit(ctxNoCancel, item.ID, song.WinningLane)
		w.recordLaneAttempts(ctxNoCancel, item.ID, song.LaneAttempts)
		return w.writeWordRecheck(ctx, item, track, song)
	}
	if orchestrator.QualityOf(song) == orchestrator.QualityWordSynced || song.WordAnswer == models.WordAnswerAbsent {
		// Words that the timing guard would demote or quarantine (a held word
		// result included) are unusable here, which is absent (plan 2.4 row 3);
		// #1007 can retime them.
		w.consecutiveFailures = 0
		return w.settleWordRecheck(ctx, item, queue.WordTimingAbsent)
	}
	// A result that says nothing about words (a lane did not answer): retry.
	return w.deferWordRecheck(ctx, item, errors.New("worker: word recheck: no word answer"))
}

// writeWordRecheck writes a writable word result and settles the row served.
// A write error defers the row: a companion write can fail after the .lrc
// landed, and the retry is served from the cache at zero requests.
func (w *Worker) writeWordRecheck(ctx context.Context, item queue.WorkItem, track models.Track, song models.Song) error {
	song.AudioDurationSeconds = track.TrackLength
	for _, p := range outputPaths(item.Inputs) {
		if err := w.writer.WriteLRC(song, p.Filename, p.Outdir); err != nil {
			return w.deferWordRecheck(ctx, item, fmt.Errorf("worker: write item %d output: %w", item.ID, err))
		}
	}
	ctxNoCancel := context.WithoutCancel(ctx)
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
// The queue effect never touches a counter; the worker-level effects mirror the
// ordinary path: a shutdown releases the row untouched, a throttle or open
// breaker idles the pass, and any other error feeds the failure backoff.
func (w *Worker) deferWordRecheck(ctx context.Context, item queue.WorkItem, cause error) error {
	noCancel := context.WithoutCancel(ctx)
	if errors.Is(ctx.Err(), context.Canceled) && errors.Is(cause, context.Canceled) {
		if err := w.queue.Release(noCancel, item.ID); err != nil {
			return fmt.Errorf("worker: release word recheck %d after shutdown: %w", item.ID, err)
		}
		return nil
	}
	if err := w.queue.DeferWordRecheck(noCancel, item.ID, w.circuitOpenDuration, cause.Error()); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("worker: defer word recheck %d after %v: %w", item.ID, cause, err)
	}
	slog.Debug("worker word recheck: unanswered; re-deferred", "id", item.ID, "retry_after", w.circuitOpenDuration, "cause", cause)
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
