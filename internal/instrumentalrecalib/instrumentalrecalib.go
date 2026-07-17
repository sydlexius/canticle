// Package instrumentalrecalib re-decides vocal-gate rejections from stored
// telemetry, without re-scanning audio.
//
// It exists because a track the OLD (too-tight) detector thresholds buried as
// "not instrumental" is never revisited: instrumentalbackfill only ever looks
// at rows the detector has NEVER scored, and `scan reconcile` only re-checks
// verdicts the detector already confirmed positive. A row stamped
// instrumental_result=0 sits in that gap forever, even after the thresholds
// are loosened, unless something re-applies the new thresholds to what the
// detector already measured.
//
// Every candidate row already carries its five telemetry scores from the
// original detection pass (music_sum/vocal_peak/speech_mean/vocal_class/
// detector_version), so re-deciding needs no audio and no detector sidecar --
// it is pure arithmetic over queue.ListVocalGateRejections' stored numbers
// against the caller's (presumably loosened) Options thresholds.
package instrumentalrecalib

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/doxazo-net/canticle/internal/detector"
	"github.com/doxazo-net/canticle/internal/lyrics"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

// Resetter clears a stamped not-instrumental verdict back to "never
// classified" so a version-mismatched row is picked up by the next
// reconcile/backfill pass instead of being settled on stale telemetry.
// Satisfied by *queue.DBQueue.
type Resetter interface {
	ResetInstrumentalToUnclassified(ctx context.Context, id int64) (bool, error)
}

// Store is the durable-queue surface a recalibration run needs. Satisfied by
// *queue.DBQueue; narrowed to a seam so every path is testable without a live
// database beyond the in-memory/temp-file SQLite the queue package already
// uses in its own tests.
type Store interface {
	Resetter
	ListVocalGateRejections(ctx context.Context, opts queue.ListVocalGateRejectionsOptions) ([]queue.StampedRejection, error)
	SettleInstrumental(ctx context.Context, id int64, tel queue.InstrumentalTelemetry) (queue.SettleOutcome, error)
}

// Writer writes the instrumental marker sidecar. Satisfied by lyrics.Writer.
//
// A vocal-gate rejection carries only its SourcePath, not the enqueued
// Outdir/Filename/OutputPaths (queue.StampedRejection is a narrow projection
// of work_queue, not the full Inputs) -- so the marker lands next to the
// source audio file, exactly like directory-mode fetch output. A fake Writer
// in a test only needs to record that it was called; the marker PATH is
// computed by this package's markerPath, mirroring
// instrumentalbackfill.MarkerPaths' use of lyrics.SidecarName.
type Writer interface {
	WriteLRC(song models.Song, filename, outdir string) error
}

// Change is one row this run acted on. It is the unit handed to
// Options.Report (the durable backup record) and Options.Preview (the
// dry-run line), so both describe exactly the same thing.
type Change struct {
	QueueID    int64
	Artist     string
	Title      string
	SourcePath string
	VocalPeak  float64
	// Action is "settle" (version-matched pass: marker written, row
	// completed) or "reset-stale" (version-mismatched pass: row dropped back
	// to never-classified for a real re-scan).
	Action string
	// MarkerPaths is the sidecar this change writes. Empty for a reset-stale
	// change: no marker exists until the row is re-scanned and re-decided
	// under the current detector version.
	MarkerPaths []string
}

// Result counts one Run.
type Result struct {
	Total          int // candidate vocal-gate rejections considered
	Settled        int // rows settled instrumental and completed
	MarkersWritten int // marker sidecars written and still on disk
	ResetStale     int // rows reset to never-classified (cross-version pass)
	SkippedClaimed int // rows a serve-mode worker claimed mid-recalibration
	Errors         int // non-fatal per-row failures
}

// Options controls a Run.
type Options struct {
	// LibraryID, when non-nil, limits the run to one library's rows.
	LibraryID *int64
	// Limit caps the candidate set when > 0.
	Limit int
	// DryRun previews without mutating anything.
	DryRun bool
	// MinConfidence, VocalMax, SpeechMax are the (presumably loosened)
	// re-decision thresholds fed to detector.Instrumental alongside each
	// row's stored telemetry.
	MinConfidence, VocalMax, SpeechMax float64
	// CurrentVersion is the running detector version. A row scored by any
	// other version is not settled directly on its stale telemetry -- it is
	// reset for a real re-scan instead.
	CurrentVersion string
	// Report is invoked once per change BEFORE the row is mutated, so an
	// applied change always has its restorable record first. A Report error
	// aborts that row's mutation and counts an error; it never aborts the
	// Run. Nil disables it.
	Report func(Change) error
	// Preview is invoked once per change in a dry run instead of mutating.
	// Nil disables it.
	Preview func(Change)
}

// Recalibrator re-decides vocal-gate rejections from stored telemetry.
type Recalibrator struct {
	store Store
	w     Writer
}

// New builds a Recalibrator over store, writing markers with w.
func New(store Store, w Writer) *Recalibrator {
	return &Recalibrator{store: store, w: w}
}

// Run re-decides the vocal-gate-rejection backlog under opts' thresholds.
// Per-row failures are counted in Result.Errors and do not abort the run;
// only a failure to enumerate the backlog returns an error.
func (r *Recalibrator) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result

	rows, err := r.store.ListVocalGateRejections(ctx, queue.ListVocalGateRejectionsOptions{
		LibraryID: opts.LibraryID,
		Limit:     opts.Limit,
	})
	if err != nil {
		return res, fmt.Errorf("instrumentalrecalib: list vocal-gate rejections: %w", err)
	}
	res.Total = len(rows)

	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("instrumentalrecalib: stop recalibration: %w", err)
		}

		pass := detector.Instrumental(row.Tel.MusicSum, row.Tel.VocalPeak, row.Tel.SpeechMean,
			opts.MinConfidence, opts.VocalMax, opts.SpeechMax)
		if !pass {
			// Still correctly not-instrumental under the new thresholds: nothing to
			// do, and no mutation means no Report/backup is warranted either.
			continue
		}

		versionMatch := row.Tel.DetectorVersion == opts.CurrentVersion
		change := Change{
			QueueID:    row.ID,
			Artist:     row.Artist,
			Title:      row.Title,
			SourcePath: row.SourcePath,
			VocalPeak:  row.Tel.VocalPeak,
		}
		if versionMatch {
			change.Action = "settle"
			change.MarkerPaths = markerPaths(row)
		} else {
			change.Action = "reset-stale"
		}

		if opts.DryRun {
			if opts.Preview != nil {
				opts.Preview(change)
			}
			continue
		}

		// Backup first: an applied change must always have its restorable record
		// on disk before the change exists.
		if opts.Report != nil {
			if err := opts.Report(change); err != nil {
				res.Errors++
				continue
			}
		}

		if !versionMatch {
			reset, err := r.store.ResetInstrumentalToUnclassified(ctx, row.ID)
			if err != nil {
				res.Errors++
				continue
			}
			if !reset {
				// A worker claimed the row (it is no longer 'deferred') between the
				// list and here: nothing to reset.
				res.SkippedClaimed++
				continue
			}
			res.ResetStale++
			continue
		}

		written, werr := r.writeMarkers(row)
		res.MarkersWritten += len(written)
		if werr != nil {
			res.Errors++
			// Never settle a verdict whose marker did not fully land.
			res.MarkersWritten -= r.rollback(written, &res)
			continue
		}

		outcome, err := r.store.SettleInstrumental(ctx, row.ID, row.Tel)
		if err != nil {
			// AMBIGUOUS: the error may have come from Commit itself, so the settle
			// may or may not have landed. Keep the marker and report the error --
			// an orphan marker is recoverable by a later run, a deleted valid
			// result is not.
			res.Errors++
			slog.Warn("instrumentalrecalib: settle failed after the marker was written; leaving the marker in place because the commit outcome is unknown",
				"id", row.ID, "markers", written, "error", err)
			continue
		}

		switch outcome {
		case queue.Settled:
			res.Settled++
		case queue.SettleAlreadyInstrumental:
			// A PEER settled this row first with the same verdict; the marker on
			// disk is correct (byte-identical). Do not remove it.
		case queue.SettleClaimed, queue.SettleRowGone:
			// A worker owns the row, or it is gone. Nothing was written to the DB,
			// so our marker is an orphan: take it back.
			res.MarkersWritten -= r.rollback(written, &res)
			res.SkippedClaimed++
		case queue.SettleFailed:
			// Unreachable: a failure returns a non-nil error, handled above.
			res.Errors++
		}
	}

	return res, nil
}

// writeMarkers writes the instrumental marker for row next to its source
// audio file, returning the paths that ACTUALLY landed.
//
// A StampedRejection carries only SourcePath (see the Writer doc comment), so
// the output directory and filename are derived from it directly -- the
// same "next to the audio file" placement directory-mode fetch output uses.
func (r *Recalibrator) writeMarkers(row queue.StampedRejection) (written []string, err error) {
	outdir, filename := outputPath(row)
	song := models.Song{Track: models.Track{
		ArtistName:   row.Artist,
		TrackName:    row.Title,
		Instrumental: 1,
	}}
	if werr := r.w.WriteLRC(song, filename, outdir); werr != nil {
		return nil, fmt.Errorf("instrumentalrecalib: write marker for %d: %w", row.ID, werr)
	}
	name, nerr := lyrics.SidecarName(row.Artist, row.Title, filename, false)
	if nerr != nil {
		return nil, fmt.Errorf("instrumentalrecalib: resolve marker name for %d: %w", row.ID, nerr)
	}
	return []string{filepath.Join(outdir, name)}, nil
}

// rollback removes markers this run wrote and reports how many came off, so
// the caller can keep MarkersWritten honest. A rollback failure is counted as
// an error rather than swallowed: it means a sidecar the database has no
// record of survived on disk, which an operator needs to know about.
func (r *Recalibrator) rollback(written []string, res *Result) int {
	for _, p := range written {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			res.Errors++
			slog.Warn("instrumentalrecalib: could not remove an orphaned marker; a sidecar the database has no record of remains on disk",
				"marker", p, "error", err)
			return 0
		}
	}
	return len(written)
}

// outputPath derives the marker's output directory and base filename from
// row.SourcePath, mirroring the "write next to the audio file" placement of
// directory-mode fetch output.
func outputPath(row queue.StampedRejection) (outdir, filename string) {
	return filepath.Dir(row.SourcePath), filepath.Base(row.SourcePath)
}

// markerPaths returns the sidecar path a settle change will write, derived
// with lyrics.SidecarName -- the same function the writer uses -- rather
// than the raw source filename, so the backup record names a path that
// actually exists after the write (see
// instrumentalbackfill.MarkerPaths for the identical reasoning).
func markerPaths(row queue.StampedRejection) []string {
	outdir, filename := outputPath(row)
	name, err := lyrics.SidecarName(row.Artist, row.Title, filename, false)
	if err != nil {
		return nil
	}
	return []string{filepath.Join(outdir, name)}
}
