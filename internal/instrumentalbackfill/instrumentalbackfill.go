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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/doxazo-net/canticle/internal/detector"
	"github.com/doxazo-net/canticle/internal/lyrics"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

// Store is the durable-queue surface a backfill needs. Satisfied by
// *queue.DBQueue; narrowed to a seam so every failure path is testable.
//
// Both writes are single guarded transactions reporting whether they applied,
// because the backfill does not own its rows: it leaves them 'deferred' while the
// detector runs, and a serve-mode worker can claim one mid-classification. A
// false return means a worker took the row and nothing was written.
type Store interface {
	CountUnclassified(ctx context.Context, libraryID *int64) (int, error)
	ListUnclassified(ctx context.Context, opts queue.ListUnclassifiedOptions) ([]queue.WorkItem, error)
	SettleInstrumental(ctx context.Context, id int64, tel queue.InstrumentalTelemetry) (bool, error)
	StampUnclassifiedMiss(ctx context.Context, id int64, tel queue.InstrumentalTelemetry) (bool, error)
}

// Detector classifies a track from its audio. Satisfied by detector.Detector.
type Detector interface {
	Detect(ctx context.Context, path string) (detector.Result, error)
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
	QueueID     int64
	Artist      string
	Title       string
	SourcePath  string
	MarkerPaths []string
	Telemetry   queue.InstrumentalTelemetry
}

// Result counts one Run.
type Result struct {
	Total            int // rows in the backlog, before Limit
	Candidates       int // rows this run considered (Total capped by Limit)
	Checked          int // rows the detector actually classified
	Instrumental     int // detector agreed
	NotInstrumental  int // detector disagreed
	MarkersWritten   int // marker sidecars written
	RowsSettled      int // rows moved to done
	SkippedDetectOff int // rows whose detect decision was off
	SkippedNoSource  int // rows with no readable source path
	SkippedClaimed   int // rows a serve-mode worker claimed mid-classification
	Errors           int // non-fatal per-row failures
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
	// Preview is invoked once per instrumental change in a dry run instead of
	// mutating. Nil disables it.
	Preview func(Change)
}

// Backfiller classifies never-scored rows.
type Backfiller struct {
	store Store
	det   Detector
	w     Writer
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

	total, err := b.store.CountUnclassified(ctx, opts.LibraryID)
	if err != nil {
		return res, fmt.Errorf("instrumentalbackfill: count unclassified: %w", err)
	}
	res.Total = total

	candidates, err := b.store.ListUnclassified(ctx, queue.ListUnclassifiedOptions{
		LibraryID: opts.LibraryID,
		Limit:     opts.Limit,
	})
	if err != nil {
		return res, fmt.Errorf("instrumentalbackfill: list unclassified: %w", err)
	}
	res.Candidates = len(candidates)

	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}

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
			continue
		}

		src := strings.TrimSpace(item.Inputs.SourcePath)
		if src == "" {
			res.SkippedNoSource++
			continue
		}

		verdict, err := b.det.Detect(ctx, src)
		if err != nil {
			res.Errors++
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

		if !verdict.Instrumental {
			res.NotInstrumental++
			if opts.DryRun {
				continue
			}
			// Stamp the negative verdict so the row is distinguishable from "never
			// detected" and a later run does not re-pay the inference. The row stays
			// deferred: a provider may still find lyrics for it.
			stamped, err := b.store.StampUnclassifiedMiss(ctx, item.ID, tel)
			if err != nil {
				res.Errors++
				continue
			}
			if !stamped {
				res.SkippedClaimed++
			}
			continue
		}

		res.Instrumental++
		change := Change{
			QueueID:     item.ID,
			Artist:      item.Inputs.Track.ArtistName,
			Title:       item.Inputs.Track.TrackName,
			SourcePath:  src,
			MarkerPaths: MarkerPaths(item.Inputs),
			Telemetry:   tel,
		}

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
				continue
			}
		}

		if err := b.writeMarkers(item, &res); err != nil {
			res.Errors++
			// Never stamp a verdict whose marker did not land, or the row would claim
			// instrumental with nothing on disk. Same ordering rule the worker uses.
			continue
		}

		// One guarded transaction: verdict, telemetry, outcome, and completion all
		// land together or not at all.
		settled, err := b.store.SettleInstrumental(ctx, item.ID, tel)
		if err != nil {
			res.Errors++
			continue
		}
		if !settled {
			// A worker claimed the row while the detector was running, so nothing was
			// written to the DB. Take the marker back rather than leave a sidecar the
			// database has no record of -- and leave the row entirely to its new owner.
			if rerr := removeMarkers(change.MarkerPaths); rerr != nil {
				res.Errors++
				continue
			}
			res.MarkersWritten -= len(change.MarkerPaths)
			res.Instrumental--
			res.SkippedClaimed++
			continue
		}
		res.RowsSettled++
	}

	return res, nil
}

// writeMarkers writes the instrumental marker to every output path for item.
func (b *Backfiller) writeMarkers(item queue.WorkItem, res *Result) error {
	song := models.Song{Track: models.Track{
		ArtistName:   item.Inputs.Track.ArtistName,
		TrackName:    item.Inputs.Track.TrackName,
		AlbumName:    item.Inputs.Track.AlbumName,
		Instrumental: 1,
	}}
	for _, p := range outputPaths(item.Inputs) {
		if err := b.w.WriteLRC(song, p.Filename, p.Outdir); err != nil {
			return fmt.Errorf("instrumentalbackfill: write marker for %d: %w", item.ID, err)
		}
		res.MarkersWritten++
	}
	return nil
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
