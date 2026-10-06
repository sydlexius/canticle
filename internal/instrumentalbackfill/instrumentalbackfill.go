// Package instrumentalbackfill classifies work_queue rows the audio detector has
// never scored, writing an instrumental marker where it agrees.
//
// It exists because detection is otherwise reachable only as a side effect of a
// provider miss inside the worker: a row deferred before the detector shipped is
// never re-examined, and `scan reconcile` cannot help because its selector is
// `instrumental_result = 1 AND status = 'done'` -- it only re-checks verdicts the
// detector already confirmed, to clear false positives. The inverse direction,
// classifying a row nobody ever looked at, had no path at all (issue #499).
//
// The rows this targets are already deferred on a benign provider miss, so the
// catalog has been asked and had no answer. Only the local detector is consulted
// and NO provider request is made, which is also why a tripped provider breaker
// does not block a backfill.
package instrumentalbackfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// Store is the durable-queue surface a backfill needs. Satisfied by
// *queue.DBQueue; narrowed to a seam so every failure path is testable.
//
// Both writes are single guarded transactions reporting whether they applied,
// because the backfill does not own its rows: it leaves them 'deferred' while the
// detector runs, and a serve-mode worker can claim one mid-classification. A
// false return means a worker took the row and nothing was written.
type Store interface {
	CountUnclassified(ctx context.Context, libraryID *int64, globalDetectDefault bool) (int, error)
	ListUnclassified(ctx context.Context, opts queue.ListUnclassifiedOptions) ([]queue.WorkItem, error)
	SettleInstrumental(ctx context.Context, id int64, tel queue.InstrumentalTelemetry, owner queue.RowOwnership) (queue.SettleOutcome, error)
	StampUnclassifiedMiss(ctx context.Context, id int64, tel queue.InstrumentalTelemetry) (bool, error)
	// RecordLaneAttempts persists the per-track detector attempt that produced the
	// verdict, so the backfill's work appears in the per-track hit-rate report
	// (#282) the same way the worker's does. Idempotent per (queue_id, lane).
	RecordLaneAttempts(ctx context.Context, queueID int64, attempts []models.LaneAttempt) error
}

// Detector classifies a track from its audio. Satisfied by detector.Detector.
type Detector interface {
	Detect(ctx context.Context, path string) (detector.Result, error)
}

// FailureStore remembers audio files the detector could not sample, keyed on the
// file's (mtime, size) so a repaired or replaced file is retried (#1149).
// Satisfied by *scanfail.Store (scanfail.NewDetector); nil disables the memory.
type FailureStore interface {
	ShouldSkip(ctx context.Context, path string, mtimeNano, size int64) (bool, error)
	RecordFailure(ctx context.Context, path string, mtimeNano, size int64, readErr error) error
}

// Writer writes the instrumental marker sidecar. Satisfied by lyrics.Writer.
//
// Note the sidecar's real name is NOT the filename passed in: the writer derives
// it via lyrics.SidecarName, which swaps the extension (an instrumental marker is
// unsynced, so an enqueued "song.lrc" lands as "song.txt"). Use MarkerPaths, never
// the raw Inputs.Filename, to name what is actually on disk. A fake Writer in a
// test MUST reproduce that derivation or it will agree with a buggy caller and
// validate nothing.
type Writer interface {
	WriteLRC(song models.Song, filename, outdir string) error
}

// Change is one row the backfill settled as instrumental. It is the unit handed
// to Options.Report (the durable backup record) and Options.Preview (the dry-run
// line), so both describe exactly the same thing.
type Change struct {
	QueueID    int64
	Artist     string
	Title      string
	SourcePath string
	// Instrumental is the detector's verdict, and therefore which mutation this
	// change records: true settles the row and writes a marker, false stamps
	// instrumental_result=0 and leaves it deferred. Both are backed up, because
	// both remove the row from future backfills.
	Instrumental bool
	// MarkerPaths are the sidecars a positive change writes, empty for a negative
	// one. Derived via lyrics.SidecarName, never the raw enqueued filename.
	MarkerPaths []string
	Telemetry   queue.InstrumentalTelemetry
}

// Outcome statuses reported to Options.Outcome after a change's mutation
// resolves. The intent record (Options.Report) is written BEFORE the mutation for
// crash safety and so cannot say whether the change landed; a restore must
// consult the matching Outcome and replay only the ones confirmed applied (#515).
// This mirrors internal/instrumentalrecalib's Outcome so both sibling reconcile
// commands share the same backup-trail contract.
const (
	OutcomeApplied = "applied" // mutation committed (settled, stamped, or a peer settled it)
	OutcomeSkipped = "skipped" // worker-claimed / row-gone: nothing written, nothing to revert
	OutcomeFailed  = "failed"  // a Report/stamp/marker error, or an ambiguous settle (see Ambiguous)
)

// Outcome is the realized result of one change, handed to Options.Outcome after
// the mutation resolves so the backup trail records what actually happened rather
// than only intent (#515). Keyed by QueueID to the earlier Report record.
type Outcome struct {
	QueueID int64
	Status  string
	// Ambiguous marks the settle-commit-unknown case: the settle may have landed
	// and the marker was left in place, so a restore must verify before reverting
	// rather than treat it as a clean no-op.
	Ambiguous bool
}

// Result counts one Run.
//
// The verdict counts and the mutation counts are deliberately separate axes.
// Instrumental + NotInstrumental == Checked ALWAYS: they record what the detector
// heard, and no mutation outcome revises that. What happened to the row afterwards
// is RowsSettled / RowsStamped / SkippedClaimed / SkippedAlreadySettled / Errors.
type Result struct {
	Total           int // eligible rows in the backlog, before Limit
	Candidates      int // rows this run examined; exceeds Limit only by skipped rows paged past
	Checked         int // rows the detector actually classified
	Instrumental    int // detector agreed  (verdict axis)
	NotInstrumental int // detector disagreed (verdict axis)

	MarkersWritten   int // marker sidecars written and still on disk
	RowsSettled      int // rows settled instrumental and completed
	RowsStamped      int // rows stamped not-instrumental, left deferred
	SkippedDetectOff int // rows whose detect decision was off
	SkippedNoSource  int // rows with no readable source path
	// SkippedMissing counts rows whose audio file no longer exists. That is a
	// stale path for prune to retire, not a detector outcome, so it is not an
	// Error (#1149). The row stays a candidate until prune removes it.
	SkippedMissing int
	// SkippedUnsampleable counts rows whose audio ffmpeg already failed to sample
	// at the file's current (mtime, size); they are not re-attempted (#1149).
	SkippedUnsampleable   int
	SkippedClaimed        int // rows a serve-mode worker claimed mid-classification
	SkippedAlreadySettled int // rows a PEER BACKFILL settled first (marker preserved)
	// KeptOnDisk counts instrumental verdicts whose marker the writer refused
	// because better lyrics are already on disk (#553). Each is stamped
	// not-instrumental (and counted in RowsStamped), so it leaves the backlog.
	KeptOnDisk int
	Errors     int // non-fatal per-row failures
}

// Options controls a Run.
type Options struct {
	// LibraryID, when non-nil, limits the backfill to one library's rows.
	LibraryID *int64
	// Limit caps the candidate set when > 0. Result.Total still reports the full
	// backlog so a capped run can say what it left behind.
	Limit int
	// DryRun classifies and previews without mutating anything.
	DryRun bool
	// GlobalDetectDefault resolves rows whose per-item detect decision is NULL,
	// mirroring how the worker resolves it.
	GlobalDetectDefault bool
	// Report is invoked once per instrumental change BEFORE the row is mutated, so
	// an applied change always has its restorable record first. A Report error
	// aborts that row's mutation and counts an error; it never aborts the Run.
	// Nil disables it.
	Report func(Change) error
	// Outcome is invoked once per change AFTER its mutation resolves, carrying the
	// realized result (applied/skipped/failed) so the backup trail records what
	// actually happened, not just the earlier Report intent (#515). Fires for
	// every row that reached the mutation stage, including a Report failure. Never
	// fires in a dry run. An Outcome error is best-effort logged and does not abort
	// the Run. Nil disables it.
	Outcome func(Outcome) error
	// Preview is invoked once per instrumental change in a dry run instead of
	// mutating. Nil disables it.
	Preview func(Change)
}

// Backfiller classifies never-scored rows.
type Backfiller struct {
	store    Store
	det      Detector
	w        Writer
	failures FailureStore

	// cursorMu guards cursor: the position in the ordered backlog where the
	// previous capped gather stopped reading past remembered rows. In memory only,
	// so a fresh Backfiller (every CLI invocation, every serve start) begins at 0;
	// the serve sweep reuses one Backfiller, so its cycles rotate through the
	// remembered tail instead of re-reading the same prefix (#1149).
	cursorMu sync.Mutex
	cursor   int
}

// WithFailureStore remembers unsampleable audio so each file version is
// attempted once, not every cycle (#1149). Returns b for chaining.
func (b *Backfiller) WithFailureStore(fs FailureStore) *Backfiller {
	b.failures = fs
	return b
}

// New builds a Backfiller over store, classifying with det and writing with w.
func New(store Store, det Detector, w Writer) *Backfiller {
	return &Backfiller{store: store, det: det, w: w}
}

// Run classifies the never-scored backlog. Per-row failures are counted in
// Result.Errors and do not abort the run; only a failure to enumerate the
// backlog returns an error.
func (b *Backfiller) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result

	total, err := b.store.CountUnclassified(ctx, opts.LibraryID, opts.GlobalDetectDefault)
	if err != nil {
		return res, fmt.Errorf("instrumentalbackfill: count unclassified: %w", err)
	}
	res.Total = total

	// Eligibility is resolved in SQL so ineligible rows never consume Limit; the
	// per-item check below is the belt-and-braces half of the same rule.
	candidates, err := b.gather(ctx, opts, &res)
	if err != nil {
		return res, err
	}

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		item, src := c.item, c.src

		verdict, err := b.det.Detect(ctx, src)
		if err != nil {
			switch {
			case errors.Is(err, ffmpeg.ErrAudioMissing):
				res.SkippedMissing++
			case errors.Is(err, ffmpeg.ErrSampleFailed) && ctx.Err() == nil:
				res.Errors++
				// No path at Warn: it is private library metadata. The detector's own
				// Warn names the file; this one names the row.
				slog.Warn("instrumental backfill: audio could not be sampled; not retrying until the file changes",
					"queue_id", item.ID, "cause", "unsampleable")
				if c.haveVersion {
					if recErr := b.failures.RecordFailure(ctx, src, c.mtimeNano, c.size, err); recErr != nil {
						slog.Warn("instrumental backfill: could not record unsampleable file; it will be retried",
							"queue_id", item.ID, "error", recErr)
					}
				}
			default:
				res.Errors++
				slog.Warn("instrumental backfill: detection failed for row; will retry",
					"queue_id", item.ID, "error", err)
			}
			continue
		}
		res.Checked++

		tel := queue.InstrumentalTelemetry{
			MusicSum:        verdict.Confidence,
			VocalPeak:       verdict.VocalConfidence,
			SpeechMean:      verdict.SpeechConfidence,
			VocalClass:      verdict.WinningVocalClass,
			DetectorVersion: verdict.Version,
		}

		// Instrumental / NotInstrumental are DETECTOR-VERDICT counts and are never
		// walked back by a mutation outcome: a worker claim does not change what the
		// detector heard. They must always sum to Checked. The mutation outcome is
		// described by RowsSettled / SkippedClaimed / Errors instead.
		change := Change{
			QueueID:      item.ID,
			Artist:       item.Inputs.Track.ArtistName,
			Title:        item.Inputs.Track.TrackName,
			SourcePath:   src,
			Instrumental: verdict.Instrumental,
			Telemetry:    tel,
		}
		if verdict.Instrumental {
			change.MarkerPaths = MarkerPaths(item.Inputs)
		}

		if !verdict.Instrumental {
			res.NotInstrumental++
			if opts.DryRun {
				continue
			}
			b.stampNotInstrumental(ctx, opts, change, &res)
			continue
		}

		res.Instrumental++

		if opts.DryRun {
			if opts.Preview != nil {
				opts.Preview(change)
			}
			continue
		}

		// Backup first: an applied change must always have its restorable record on
		// disk before the change exists.
		if opts.Report != nil {
			if err := opts.Report(change); err != nil {
				res.Errors++
				b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeFailed})
				continue
			}
		}

		// written records exactly the markers that landed, so a rollback removes what
		// this run created and nothing else -- a partial write (some paths succeeded,
		// one failed) must not leave its successful siblings behind.
		written, err := b.writeMarkers(item)
		res.MarkersWritten += len(written)
		if errors.Is(err, lyrics.ErrKeptBetter) {
			// Better lyrics are already on disk (#553), so no marker can land and
			// the row must not settle instrumental. It must not stay unclassified
			// either: ListUnclassified is deterministically ordered, so the row
			// would be re-detected (audio read + ffmpeg + inference, a disk wake,
			// #684) on every cycle forever and, under a batch cap, starve the rows
			// behind it. Retire it with the not-instrumental stamp instead: the
			// verdict that stands for this row is "the file on disk is real
			// lyrics", the row stays deferred exactly as a negative verdict leaves
			// it, and the stored telemetry is the detector's real scores (so the
			// recalibrator's threshold re-decision still sees what it heard).
			//
			// RULE: the row is never stamped not-instrumental beside a marker. Any
			// marker an earlier output path wrote is taken back first; if that
			// rollback is incomplete (a removal failed, already counted in Errors),
			// the row is NOT stamped: it stays unclassified and the next cycle
			// retries. KeptOnDisk still counts only on the clean path, since it
			// documents rows that leave the backlog with a stamp.
			removed := b.rollback(written, &res)
			res.MarkersWritten -= removed
			if removed != len(written) {
				b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeFailed})
				continue
			}
			res.KeptOnDisk++
			// The positive change's mutation (marker + settle) did not happen.
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeSkipped})
			neg := change
			neg.Instrumental = false
			neg.MarkerPaths = nil
			b.stampNotInstrumental(ctx, opts, neg, &res)
			continue
		}
		if err != nil {
			res.Errors++
			// Never stamp a verdict whose marker did not fully land, or the row would
			// claim instrumental against an incomplete set of sidecars. The DB was not
			// touched, so removing what did land is unambiguously correct.
			res.MarkersWritten -= b.rollback(written, &res)
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeFailed})
			continue
		}

		// One guarded transaction: verdict, telemetry, outcome, and completion all
		// land together or not at all.
		outcome, err := b.store.SettleInstrumental(ctx, item.ID, tel, queue.OwnedByBackfill)
		if err != nil {
			// AMBIGUOUS: the error may have come from Commit itself, so the settle may
			// or may not have landed. Deleting the marker could destroy a committed
			// result, so keep it and report the error -- an orphan marker is recoverable
			// by a later run, a deleted valid marker is not. Errs toward the reversible
			// failure.
			res.Errors++
			slog.Warn("instrumentalbackfill: settle failed after the marker was written; leaving the marker in place because the commit outcome is unknown",
				"id", item.ID, "markers", written, "error", err)
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeFailed, Ambiguous: true})
			continue
		}

		switch outcome {
		case queue.Settled:
			res.RowsSettled++
			// Only on Settled. SettleAlreadyInstrumental means a PEER settled the row
			// and recorded its own attempt, so recording one here would attribute the
			// same track to the detector twice -- harmless for the (idempotent) row
			// itself, but it is the peer's classification, not this run's.
			b.recordAttempt(ctx, item.ID, true)
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeApplied})
		case queue.SettleAlreadyInstrumental:
			// A PEER BACKFILL settled this row first with the same verdict. The marker
			// on disk is correct -- it is the peer's, and ours is byte-identical. Do
			// NOT remove it: that would delete a valid result over a race we lost
			// harmlessly.
			//
			// OutcomeApplied reflects the end STATE (row done+instrumental), not
			// authorship: the DB verdict was established by a DIFFERENT actor
			// (classifyNoSettle keys only on status='done' AND instrumental_result=1).
			// A future restore reverting OutcomeApplied rows will also revert a
			// peer-established verdict -- sound as a state reversal, but if actor
			// attribution ever matters this arm is where a distinct status belongs.
			// No restore tool exists yet (#515).
			res.SkippedAlreadySettled++
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeApplied})
		case queue.SettleClaimed, queue.SettleRowGone:
			// A worker owns the row, or it is gone. Nothing was written to the DB, so
			// our marker is an orphan the database has no record of: take it back.
			res.MarkersWritten -= b.rollback(written, &res)
			res.SkippedClaimed++
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeSkipped})
		case queue.SettleFailed:
			// Unreachable: a failure returns a non-nil error, handled above.
			res.Errors++
			b.reportOutcome(opts, Outcome{QueueID: item.ID, Status: OutcomeFailed})
		}
	}

	return res, nil
}

// maxGatherPages bounds how many Limit-sized pages one Run reads looking for
// rows worth a detector call (#1149). A skipped row costs a stat and one
// primary-key lookup, never an ffmpeg run, so reading past them is cheap; the
// bound keeps a cycle's stats finite when the remembered population is huge.
const maxGatherPages = 10

// candidate is a row worth a detector call, with the file version observed when
// it was checked against the remembered failures.
type candidate struct {
	item            queue.WorkItem
	src             string
	mtimeNano, size int64
	haveVersion     bool
}

// gather returns up to opts.Limit rows worth a detector call. ListUnclassified
// applies Limit in SQL, but a remembered-unsampleable row is only known to be
// skippable after a stat, so a page whose rows were skipped would hand the same
// rows to every cycle and the rows behind them would never be reached. gather
// therefore reads further pages until Limit rows are collected, the backlog
// ends, or maxGatherPages is spent. Nothing is mutated while gathering, so
// offset paging is stable within the run; rows are de-duplicated by id in case
// a concurrent writer reorders the set between pages.
//
// The page cap alone would re-read the same remembered prefix every cycle, so a
// changed file further back in a large remembered tail would never be reached.
// Page 0 is therefore always read from offset 0 (never-attempted rows sort
// first and keep their priority), and later pages resume at b.cursor, where
// the previous capped gather stopped; on reaching the end of the backlog the
// tail wraps back to offset Limit and reads up to where this run started. The
// cursor is a position, not an identity: rows leaving the backlog shift it by
// at most a few rows, which only delays them until the next wrap. Concurrent
// Runs on one Backfiller may share a cursor value; that costs duplicate reads,
// never a missed or double mutation (the store's writes are guarded).
func (b *Backfiller) gather(ctx context.Context, opts Options, res *Result) ([]candidate, error) {
	var out []candidate
	seen := map[int64]bool{}
	limit := opts.Limit
	jump := limit
	if limit > 0 {
		b.cursorMu.Lock()
		jump = max(b.cursor, limit)
		b.cursorMu.Unlock()
	}
	wrapped := false
	for page, offset := 0, 0; ; page++ {
		items, err := b.store.ListUnclassified(ctx, queue.ListUnclassifiedOptions{
			LibraryID:           opts.LibraryID,
			Limit:               limit,
			Offset:              offset,
			GlobalDetectDefault: opts.GlobalDetectDefault,
		})
		if err != nil {
			return nil, fmt.Errorf("instrumentalbackfill: list unclassified: %w", err)
		}
		next := offset // position of the first row this page left unexamined
		for _, item := range items {
			if limit > 0 && len(out) == limit {
				break
			}
			next++
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			res.Candidates++
			if c, ok := b.admit(ctx, opts, item, res); ok {
				out = append(out, c)
			}
		}
		if limit <= 0 {
			return out, nil
		}
		ended := len(items) < limit
		switch {
		case len(out) == limit || (page+1 >= maxGatherPages && !ended):
			// Stopped early. A run that never left page 0 says nothing about the
			// tail, so it keeps the cursor for the next run.
			if page > 0 {
				b.setCursor(next)
			}
			return out, nil
		case page == 0 && !ended:
			offset = jump
		case ended && !wrapped && jump > limit:
			offset, wrapped = limit, true
		case ended:
			b.setCursor(0)
			return out, nil
		default:
			offset = next
		}
		if wrapped && offset >= jump {
			// The wrap reached where this run's tail read began: the whole
			// backlog has been read once.
			b.setCursor(0)
			return out, nil
		}
		if page+1 >= maxGatherPages {
			b.setCursor(offset)
			return out, nil
		}
	}
}

// setCursor records where the next capped gather resumes its tail read.
func (b *Backfiller) setCursor(pos int) {
	b.cursorMu.Lock()
	b.cursor = pos
	b.cursorMu.Unlock()
}

// admit applies the per-row skips, counting each in res.
func (b *Backfiller) admit(ctx context.Context, opts Options, item queue.WorkItem, res *Result) (candidate, bool) {
	// Honor the per-item decision stamped at enqueue, falling back to the global
	// default, exactly as the worker resolves it. A row explicitly opted out
	// stays opted out: this is a backfill for rows nobody looked at, not an
	// override of a decision already made.
	detect := opts.GlobalDetectDefault
	if item.DetectInstrumental != nil {
		detect = *item.DetectInstrumental
	}
	if !detect {
		res.SkippedDetectOff++
		return candidate{}, false
	}

	c := candidate{item: item, src: strings.TrimSpace(item.Inputs.SourcePath)}
	if c.src == "" {
		res.SkippedNoSource++
		return candidate{}, false
	}

	// A file ffmpeg already failed on at this exact (mtime, size) would fail
	// identically: skip it without touching the audio again (#1149). A stat or
	// store failure just falls through to the attempt.
	if b.failures != nil {
		if fi, statErr := os.Stat(c.src); statErr == nil {
			c.mtimeNano, c.size, c.haveVersion = fi.ModTime().UnixNano(), fi.Size(), true
			if skip, skipErr := b.failures.ShouldSkip(ctx, c.src, c.mtimeNano, c.size); skipErr == nil && skip {
				res.SkippedUnsampleable++
				return candidate{}, false
			}
		}
	}
	return c, true
}

// stampNotInstrumental applies a not-instrumental change: backup record first,
// then instrumental_result=0 on the still-deferred row, then its outcome.
//
// A negative verdict is a MUTATION too: it stamps instrumental_result=0, which
// removes the row from every future backfill's candidate set. So it gets the
// same backup-first treatment as a positive one -- otherwise --yes could
// quietly retire rows with no recoverable record of having done so.
func (b *Backfiller) stampNotInstrumental(ctx context.Context, opts Options, change Change, res *Result) {
	if opts.Report != nil {
		if err := opts.Report(change); err != nil {
			res.Errors++
			b.reportOutcome(opts, Outcome{QueueID: change.QueueID, Status: OutcomeFailed})
			return
		}
	}
	// The row stays deferred: a provider may still find lyrics for it.
	stamped, err := b.store.StampUnclassifiedMiss(ctx, change.QueueID, change.Telemetry)
	if err != nil {
		res.Errors++
		b.reportOutcome(opts, Outcome{QueueID: change.QueueID, Status: OutcomeFailed})
		return
	}
	if !stamped {
		res.SkippedClaimed++
		b.reportOutcome(opts, Outcome{QueueID: change.QueueID, Status: OutcomeSkipped})
		return
	}
	res.RowsStamped++
	// Recorded only after the stamp APPLIED: a row a worker claimed mid-flight
	// was not classified by this run, so attributing an attempt to it would
	// credit the detector with work it did not land.
	b.recordAttempt(ctx, change.QueueID, false)
	b.reportOutcome(opts, Outcome{QueueID: change.QueueID, Status: OutcomeApplied})
}

// reportOutcome hands a realized Outcome to Options.Outcome when set. An error
// from the callback is best-effort logged and swallowed: the mutation already
// happened, so failing to append its outcome record must not abort the Run or
// change the row's fate -- it only degrades the backup trail's completeness (#515).
func (b *Backfiller) reportOutcome(opts Options, o Outcome) {
	if opts.Outcome == nil {
		return
	}
	if err := opts.Outcome(o); err != nil {
		slog.Warn("instrumentalbackfill: could not record change outcome to the backup trail",
			"id", o.QueueID, "status", o.Status, "error", err)
	}
}

// recordAttempt persists the detector attempt that produced a verdict, so the
// backfill's classifications appear in the per-track hit-rate report (#282).
//
// WHY THIS EXISTS. lane_attempts is written by the worker via recordLaneAttempts
// on every dispatch, but the backfill reaches its verdict through an entirely
// different path and recorded nothing at all. The report's detector tile
// therefore counted ONLY worker-side detections and sat frozen while a backfill
// classified thousands of tracks -- the numerator and denominator both stalled,
// so the tile did not merely undercount, it looked broken.
//
// BOTH VERDICTS, NOT HITS-ONLY. hit=1 for instrumental, hit=0 for
// not-instrumental. The tile renders a hit RATE, so recording only the positives
// would drive the detector toward a meaningless 100%. This is the same rule
// migration 029 states for the historical backfill, and it is easy to get wrong
// in the opposite direction here because only the positive path feels like a
// "result".
//
// BEST-EFFORT. A failure is logged and swallowed: the verdict is already durably
// recorded on the row, and losing a reporting attempt must not fail a row whose
// mutation succeeded. Idempotent per (queue_id, lane), so a later re-run
// upserts rather than double-counting.
func (b *Backfiller) recordAttempt(ctx context.Context, queueID int64, instrumental bool) {
	if err := b.store.RecordLaneAttempts(ctx, queueID, []models.LaneAttempt{{
		Lane: detectorbackfill.LaneName,
		Hit:  instrumental,
		// The detector resolves a track with no outbound provider request.
		Local: true,
	}}); err != nil {
		slog.Warn("instrumentalbackfill: could not record the detector lane attempt; the per-track hit-rate report will undercount this row",
			"id", queueID, "error", err)
	}
}

// writeMarkers writes the instrumental marker to every output path for item,
// returning the paths that ACTUALLY landed -- including on the error path, so a
// partial write can be rolled back precisely rather than guessed at.
func (b *Backfiller) writeMarkers(item queue.WorkItem) (written []string, err error) {
	song := models.Song{Track: models.Track{
		ArtistName:   item.Inputs.Track.ArtistName,
		TrackName:    item.Inputs.Track.TrackName,
		AlbumName:    item.Inputs.Track.AlbumName,
		Instrumental: 1,
	}}
	for _, p := range outputPaths(item.Inputs) {
		if werr := b.w.WriteLRC(song, p.Filename, p.Outdir); werr != nil {
			return written, fmt.Errorf("instrumentalbackfill: write marker for %d: %w", item.ID, werr)
		}
		name, nerr := lyrics.SidecarName(item.Inputs.Track.ArtistName, item.Inputs.Track.TrackName, p.Filename, false)
		if nerr != nil {
			return written, fmt.Errorf("instrumentalbackfill: resolve marker name for %d: %w", item.ID, nerr)
		}
		written = append(written, filepath.Join(p.Outdir, name))
	}
	return written, nil
}

// rollback removes markers this run wrote and reports how many came off, so the
// caller can keep MarkersWritten honest. A rollback failure is counted as an
// error rather than swallowed: it means a sidecar the database has no record of
// survived on disk, which an operator needs to know about.
func (b *Backfiller) rollback(written []string, res *Result) int {
	if err := removeMarkers(written); err != nil {
		res.Errors++
		slog.Warn("instrumentalbackfill: could not remove an orphaned marker; a sidecar the database has no record of remains on disk",
			"markers", written, "error", err)
		return 0
	}
	return len(written)
}

// outputPaths mirrors the worker's resolution: the enqueued OutputPaths when
// present, else the single Outdir/Filename pair, so the CLI and the worker agree
// on where a marker lands.
func outputPaths(inputs models.Inputs) []models.OutputPath {
	if len(inputs.OutputPaths) > 0 {
		return inputs.OutputPaths
	}
	return []models.OutputPath{{Outdir: inputs.Outdir, Filename: inputs.Filename}}
}

// MarkerPaths returns the sidecar paths a marker write actually lands on, derived
// with lyrics.SidecarName -- the same function the writer uses -- rather than the
// raw enqueued filename.
//
// This distinction is load-bearing, not pedantry: an instrumental marker is
// unsynced, so SidecarName rewrites an enqueued "song.lrc" to "song.txt". Naming
// the raw filename in the backup record produced a restorable record pointing at a
// path that never existed, which silently voided the backup contract. The sibling
// `scan reconcile` derives its marker paths the same way for the same reason.
func MarkerPaths(inputs models.Inputs) []string {
	paths := outputPaths(inputs)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		name, err := lyrics.SidecarName(inputs.Track.ArtistName, inputs.Track.TrackName, p.Filename, false)
		if err != nil {
			continue
		}
		out = append(out, filepath.Join(p.Outdir, name))
	}
	return out
}

// removeMarkers deletes markers this run wrote, used when the settle is refused
// because a worker claimed the row: the DB write did not happen, so the sidecar
// must not survive either. An already-absent file is not an error.
func removeMarkers(paths []string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("instrumentalbackfill: remove orphaned marker %s: %w", p, err)
		}
	}
	return nil
}
