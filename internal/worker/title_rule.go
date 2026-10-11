package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// SetTitleRule turns the instrumental title rule on or off (#1468). The caller
// derives it from config's InstrumentalTitleMode: every mode but off acts on a
// Strong title the same way, so the worker needs no more than the switch. Off
// (the zero value) leaves every row on the path it took before the rule existed.
func (w *Worker) SetTitleRule(enabled bool) { w.titleRule = enabled }

// absentOnlyWriter is the write that lands only on an empty stem
// (lyrics.LRCWriter.WriteLRCIfAbsent) plus its read-only twin, which names what
// that write would keep without writing (lyrics.LRCWriter.Occupant).
type absentOnlyWriter interface {
	WriteLRCIfAbsent(models.Song, string, string) error
	Occupant(models.Song, string, string) (*lyrics.KeptError, error)
}

// titleRuleSettles reports whether the rule settles this row as instrumental:
// the rule is on and instrumentaltitle.Actionable says the title may be acted
// on (Strong, and for a dash-suffix the text before the separator is known not
// to be the track's artist). The artists handed over are the row's own tags,
// which between them hold the resolved name RunOnce put in track.ArtistName.
// The album is the refreshed one, so a caller outside the worker (the bulk
// repair, #1470) agrees with this gate only if it passes the same album.
func (w *Worker) titleRuleSettles(item queue.WorkItem, track models.Track) bool {
	if !w.titleRule {
		return false
	}
	tagged := item.Inputs.Track
	_, ok := instrumentaltitle.Actionable(track.TrackName, track.AlbumName, tagged.ArtistName, tagged.AlbumArtist)
	return ok
}

// titleRuleGate settles a Strong instrumental-titled row without asking any
// lane or the cache (#1145 D2). It runs before the word-recheck branch, so it
// covers all three kinds of row, and it never touches a file already on disk:
//
//   - a word-recheck row settles with no word verdict to add (its file stays);
//   - an upgrade trip settles back onto its file record (its file stays);
//   - any other row gets the marker only where its stem holds nothing. A lyric
//     file or an instrumental marker of ANY provenance (the detector's, a
//     provider's, a hand-typed or hand-marked one) stays byte for byte and the
//     row settles through the kept path, its lane untouched: a marker the rule
//     did not originate must never become `--source title-rule` purge's to delete.
//
// handled=false means the row is not the rule's and RunOnce carries on
// unchanged. No provider was contacted, so no pacing wait is charged.
func (w *Worker) titleRuleGate(ctx context.Context, item queue.WorkItem, track models.Track) (handled bool, err error) {
	if !w.titleRuleSettles(item, track) {
		return false, nil
	}
	if item.WordTimingState == queue.WordTimingQueued {
		if w.wordSyncOff() {
			return false, nil // runWordRecheck releases it untouched, with no fetch
		}
		w.lastItemContactedProvider = false
		w.consecutiveFailures = 0
		return true, w.settleWordRecheck(ctx, item)
	}
	if item.UpgradeQueued {
		w.lastItemContactedProvider = false
		return true, w.settleUpgradeTrip(ctx, item)
	}
	guarded, ok := w.writer.(absentOnlyWriter)
	if !ok {
		// Loud, never silent: without the guarded write a forced writer could
		// replace a lyric, so the row takes the ordinary path instead.
		slog.Error("worker title rule: writer lacks WriteLRCIfAbsent or Occupant; row takes the ordinary path", "id", item.ID)
		return false, nil
	}
	w.lastItemContactedProvider = false
	var preOK bool
	if item, preOK = w.preflightOutputs(ctx, item); !preOK {
		return true, w.fail(ctx, item, fmt.Errorf("worker: item %d output: %w", item.ID, lyrics.ErrOutputDirMissing))
	}
	return true, w.completeTitleInstrumental(ctx, item, track, guarded)
}

// completeTitleInstrumental writes the marker and completes the row. It mirrors
// completeDetectorInstrumental minus the telemetry: the verdict came from the
// title, so it never calls SettleInstrumental and never stamps
// instrumental_result (that column is the detector's record), and it records no
// lane attempt or provider counter, since no lane was dispatched. The marker is
// never cached: a cache entry would outlive the mode being turned off.
//
// provider_lane and outcome_type are REQUIRED before Complete. The lane is what
// makes `scan purge-provenance --source title-rule` the bulk undo (it must agree
// with the marker's [source:]), and a done row with no outcome is the #655
// state. A failure of either fails the row; the retry rewrites the same marker.
//
// A row with several outputs is ONE track, and the rule acts only on a track
// with nothing on disk. So every output is probed (read-only) before the first
// write, and a lyric or marker at any of them means nothing is written at any:
// the row settles through the kept path, lane untouched. Otherwise a lyric at
// one output would sit beside a title-rule marker at another, under a row whose
// lane says the rule settled it. The probe is a snapshot. A file that appears
// at one output after it is still refused there by the write, but the outputs
// already written keep their marker and the row records the title-rule settle.
func (w *Worker) completeTitleInstrumental(ctx context.Context, item queue.WorkItem, track models.Track, writer absentOnlyWriter) error {
	track.Instrumental = 1
	song := lyrics.StampBlockIdentity(ctx, models.Song{Track: track, WinningLane: lyrics.TitleRuleLaneName})
	paths := outputPaths(item.Inputs)
	var kept []*lyrics.KeptError
	var skipped []models.OutputPath // entries whose output directory is missing (#1430)
	var missingErr error
	wrote := false
	failWrite := func(cause error) error {
		return w.fail(ctx, item, fmt.Errorf("worker: write title-rule marker for item %d: %w", item.ID, scrubWritePaths(cause)))
	}
	probes := make([]error, len(paths)) // a *lyrics.KeptError where that output is occupied
	occupied := false
	for i, p := range paths {
		k, probeErr := writer.Occupant(song, p.Filename, p.Outdir)
		if probes[i] = probeErr; probeErr == nil && k != nil {
			probes[i], occupied = k, true
		}
	}
	for i, p := range paths {
		var writeErr error
		if occupied {
			// Nothing is written anywhere: this output's probe stands in for
			// its write, and an empty one is left empty.
			if writeErr = probes[i]; writeErr == nil {
				continue
			}
		} else {
			writeErr = writer.WriteLRCIfAbsent(song, p.Filename, p.Outdir)
		}
		switch {
		case errors.Is(writeErr, lyrics.ErrKeptBetter):
			kept = append(kept, keptErrorOf(writeErr))
		case errors.Is(writeErr, lyrics.ErrOutputDirMissing):
			skipped = append(skipped, p)
			if missingErr == nil {
				missingErr = writeErr
			}
		case writeErr != nil:
			return failWrite(writeErr)
		default:
			wrote = true
		}
	}
	if len(skipped) > 0 && ((!wrote && len(kept) == 0) || !w.skippedAllStale(ctx, item, skipped)) {
		return failWrite(missingErr)
	}
	if occupied || len(kept) == len(paths)-len(skipped) {
		// A lyric or a marker (anyone's) is already on disk: nothing landed, so
		// the row settles describing the kept file. (The second term is the
		// probe-to-write gap: every output filled after an all-empty probe.)
		return w.completeKept(ctx, item, kept)
	}
	ctxNoCancel := context.WithoutCancel(ctx)
	w.stampCompletionProvenance(ctxNoCancel, item.ID, song)
	w.clearWordTiming(ctxNoCancel, item)
	w.clearSyncTier(ctxNoCancel, item)
	err := w.queue.SetProviderLane(ctxNoCancel, item.ID, lyrics.TitleRuleLaneName, "")
	if err == nil {
		err = w.queue.SetOutcomeType(ctxNoCancel, item.ID, outcomeTypeInstrumental)
	}
	if err == nil {
		err = w.queue.Complete(ctxNoCancel, item.ID)
	}
	if err != nil {
		return w.failStuckItem(ctxNoCancel, item, capNever, fmt.Errorf("worker: settle title-rule item %d: %w", item.ID, err))
	}
	// id only: the title is library metadata and stays off Info.
	slog.Info("worker title rule: instrumental-titled track settled with a marker", "id", item.ID)
	w.consecutiveFailures = 0
	return nil
}

// skipDetectorFirst reports whether the detector must not be asked for this
// track. Under instrumental_detector.ordering = "front" the detector answers
// before any provider, and a karaoke backing track sounds instrumental to it;
// a karaoke-titled track must get its lyrics (maintainer decision 2026-10-11),
// so such a row goes to the providers with the detector left out of the
// dispatch. It applies only while the title rule is on, so mode off is unchanged.
func (w *Worker) skipDetectorFirst(track models.Track) bool {
	return w.titleRule && w.detectorOrdering == "front" &&
		instrumentaltitle.KaraokeExcluded(track.TrackName, track.AlbumName)
}
