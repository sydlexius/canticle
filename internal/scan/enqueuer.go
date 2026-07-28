package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/timing"
)

// PendingResultStore reads and updates scan results eligible for queuing.
type PendingResultStore interface {
	ListPendingByLibrary(ctx context.Context, libraryID int64) ([]models.ScanResult, error)
	SetStatus(ctx context.Context, ids []int64, status string) error
}

// LyricsCache reports whether lyrics already exist for a scanned track.
type LyricsCache interface {
	// Lookup checks the cache for (artist, title, durationBucket).
	// Pass durationBucket=0 when the recording duration is not yet known.
	Lookup(ctx context.Context, artist, title string, durationBucket int) (string, error)
}

// WorkQueue enqueues durable lyrics work.
type WorkQueue interface {
	Enqueue(ctx context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error)
}

// TimingVerdict is a track's persisted accept-time timing decision, together
// with the provider generation that produced it.
type TimingVerdict struct {
	// Outcome is the stored work_queue.timing_outcome value (#440).
	Outcome timing.TimingOutcome
	// ProvidersVersion is the provider-set generation in effect when the verdict
	// was reached. It is what makes the suppression expire: see shouldSuppress.
	ProvidersVersion int
}

// TimingVerdictStore reads the durable timing verdict recorded for a track.
//
// This is the consumer that migration 034's columns never had: the worker has
// stamped timing_outcome since #440, but nothing read it back, so the pipeline
// had no memory of having already rejected a track (#679).
//
// found is false when no verdict was ever recorded, which is the normal case for
// a track that has simply not been fetched yet.
type TimingVerdictStore interface {
	LookupTiming(ctx context.Context, artist, title string) (TimingVerdict, bool, error)
}

// TimingVerdictReader is the raw queue-side read, in primitives.
//
// *queue.DBQueue satisfies it directly. Keeping the queue package free of a
// scan-package type preserves the existing dependency direction (scan imports
// queue, never the reverse); TimingVerdicts bridges the two shapes.
type TimingVerdictReader interface {
	LookupTiming(ctx context.Context, artist, title string) (outcome string, providersVersion int, found bool, err error)
}

// TimingVerdicts adapts a TimingVerdictReader to TimingVerdictStore.
type TimingVerdicts struct {
	Reader TimingVerdictReader
}

// LookupTiming converts the stored outcome string into a typed verdict. The
// column holds an internal/timing TimingOutcome verbatim (see migration 034), so
// the conversion is a cast rather than a parse; an unrecognized value simply
// never matches Categorical and therefore never suppresses.
func (t TimingVerdicts) LookupTiming(ctx context.Context, artist, title string) (TimingVerdict, bool, error) {
	if t.Reader == nil {
		return TimingVerdict{}, false, nil
	}
	outcome, version, found, err := t.Reader.LookupTiming(ctx, artist, title)
	if err != nil || !found {
		return TimingVerdict{}, false, err
	}
	return TimingVerdict{
		Outcome:          timing.TimingOutcome(outcome),
		ProvidersVersion: version,
	}, true, nil
}

// Enqueuer bridges pending scan results to the durable work queue.
type Enqueuer struct {
	Results  PendingResultStore
	Cache    LyricsCache
	Queue    WorkQueue
	Priority int
	// DetectOverride is the scan-CLI override for instrumental detection
	// (--detect-instrumental/--no-detect-instrumental); nil means no override.
	// GlobalDetectDefault is the global config default used when neither the
	// override nor the per-library setting is set. EnqueuePending resolves the
	// per-item decision from these via config.ResolveBool and stamps it onto each
	// enqueued item (stamp-on-insert; the worker reads it back later).
	DetectOverride      *bool
	GlobalDetectDefault bool
	// Timing reads the durable timing verdict so a Categorical track is not
	// re-fetched on every scan (#679). Optional: a nil Timing disables the
	// suppression entirely and preserves the pre-#679 behavior, so a caller that
	// does not wire it enqueues exactly as before.
	Timing TimingVerdictStore
	// ProvidersVersion is the CURRENT provider-set generation, compared against
	// the generation stored with a verdict to decide whether the verdict still
	// speaks for today's provider set. Zero means unknown, which never suppresses.
	ProvidersVersion int
}

// shouldSuppress reports whether a track's stored timing verdict means this scan
// must not re-enqueue it.
//
// ONLY Categorical suppresses. That verdict means the lyric was timed to a
// different, longer recording -- the words themselves are suspect -- so the
// accept-time guard (#439) deliberately writes NOTHING. With no sidecar on disk,
// the row is indistinguishable from a track that was never fetched, so it is
// re-enqueued, re-fetched, re-judged and re-rejected on every scan forever.
//
// MisSynced is deliberately NOT suppressed. It writes a .txt, and #439's
// settled-sidecar check makes a later re-fetch a no-op: wasteful, never
// destructive. Suppressing it would hide a recoverable track for no gain.
//
// THE SUPPRESSION EXPIRES WITH THE PROVIDER GENERATION, and that is a deliberate
// choice rather than a default. A Categorical verdict is a statement about what
// the providers served at ONE MOMENT; permanent suppression would mean never
// finding correctly-timed lyrics that a provider starts serving later. Tying
// expiry to providers_version reuses the generation counter that already retires
// stale cache entries, instead of inventing a second, parallel expiry scheme.
//
// A zero current generation (unknown) never suppresses: without a trustworthy
// comparison the safe direction is to re-examine, since the cost is one refetch
// while the cost of wrongly suppressing is losing the track indefinitely.
func (e *Enqueuer) shouldSuppress(v TimingVerdict) bool {
	if v.Outcome != timing.Categorical {
		return false
	}
	if e.ProvidersVersion == 0 {
		return false
	}
	return v.ProvidersVersion == e.ProvidersVersion
}

// EnqueuePending reads pending scan results for libraryID, skips cache hits,
// and enqueues cache misses for worker processing. It returns the number of
// rows enqueued and the number short-circuited as cache hits so callers can log
// a per-scan summary; on error the partial counts so far are returned alongside.
func (e *Enqueuer) EnqueuePending(ctx context.Context, lib models.Library) (enqueued, cacheHits int, retErr error) {
	if e.Results == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer results dependency is nil")
	}
	if e.Cache == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer cache dependency is nil")
	}
	if e.Queue == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer queue dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	// Resolve the per-library instrumental-detection decision once (CLI override >
	// per-library setting > global default) and stamp it onto every item enqueued
	// for this library; the worker reads it back at fetch time.
	detect := config.ResolveBool(e.DetectOverride, lib.DetectInstrumental, e.GlobalDetectDefault)
	libraryID := lib.ID

	results, err := e.Results.ListPendingByLibrary(ctx, libraryID)
	if err != nil {
		return 0, 0, fmt.Errorf("scan: list pending for enqueue: %w", err)
	}

	// Tracks skipped this pass because a Categorical verdict quarantined them.
	// Logged once at the end rather than returned: a per-track line would be
	// noise on a library where the count is stable across scans, and the full
	// operator surface (listing and re-examining quarantined tracks) belongs to
	// #629 rather than a second mechanism built here.
	suppressed := 0

	for _, res := range results {
		if err := ctx.Err(); err != nil {
			return enqueued, cacheHits, err
		}
		_, err := e.Cache.Lookup(ctx, res.Track.ArtistName, res.Track.TrackName, normalize.DurationBucket(res.Track.TrackLength))
		switch {
		case err == nil:
			if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusDone); err != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: mark cache hit done %d: %w", res.ID, err)
			}
			cacheHits++
			continue
		case errors.Is(err, sql.ErrNoRows):
		default:
			return enqueued, cacheHits, fmt.Errorf("scan: cache lookup %d: %w", res.ID, err)
		}

		// A Categorical verdict wrote no sidecar, so nothing on disk stops this
		// track being re-fetched forever. Consult the durable verdict instead
		// (#679), placed here -- after the cache check, before the row is reserved
		// -- so a suppressed track stays pending and is re-examined the moment the
		// provider generation moves, rather than being consumed by this scan.
		//
		// FAILS OPEN by construction: a lookup error is logged and ignored, since
		// suppression is an optimization and a failed read must never silently
		// drop work. Losing a track looks identical to having fetched it.
		if e.Timing != nil {
			verdict, found, terr := e.Timing.LookupTiming(ctx, res.Track.ArtistName, res.Track.TrackName)
			switch {
			case terr != nil:
				slog.Debug("scan: timing verdict lookup failed; enqueueing anyway",
					"result_id", res.ID, "error", terr)
			case found && e.shouldSuppress(verdict):
				slog.Debug("scan: skipping track quarantined by a categorical timing verdict",
					"result_id", res.ID, "providers_version", verdict.ProvidersVersion)
				suppressed++
				continue
			}
		}

		if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusProcessing); err != nil {
			return enqueued, cacheHits, fmt.Errorf("scan: reserve result %d: %w", res.ID, err)
		}
		inputs, err := scanInputs(res)
		if err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: build inputs for result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return enqueued, cacheHits, fmt.Errorf("scan: build inputs for result %d: %w", res.ID, err)
		}
		inputs.DetectInstrumental = &detect
		if _, err := e.Queue.Enqueue(ctx, inputs, e.Priority); err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: enqueue result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return enqueued, cacheHits, fmt.Errorf("scan: enqueue result %d: %w", res.ID, err)
		}
		enqueued++
	}
	// Never leave the suppression silent: a track skipped here produces no work
	// item, no sidecar and no queue row, so without this line an operator has no
	// way to tell "nothing left to fetch" from "N tracks are quarantined".
	if suppressed > 0 {
		slog.Info("scan: skipped tracks quarantined by a categorical timing verdict",
			"library_id", libraryID, "suppressed", suppressed,
			"providers_version", e.ProvidersVersion)
	}
	return enqueued, cacheHits, nil
}

// OnScanComplete adapts EnqueuePending to Scheduler.OnScanComplete.
func (e *Enqueuer) OnScanComplete(ctx context.Context, lib models.Library, _ []models.ScanResult, _ string, _ Trigger) error {
	_, _, err := e.EnqueuePending(ctx, lib)
	return err
}

// ResultInputs converts a scan result into queue inputs using the same outdir,
// filename, source path, and output-path derivation as scan-created work. The
// webhook resolver uses this so inventory-matched work is enqueued identically
// to work the scheduler enqueues.
func ResultInputs(res models.ScanResult) (models.Inputs, error) {
	return scanInputs(res)
}

func scanInputs(res models.ScanResult) (models.Inputs, error) {
	outdir := res.Outdir
	if outdir == "" && res.FilePath != "" {
		outdir = filepath.Dir(res.FilePath)
	}
	filename := res.Filename
	if filename == "" && res.FilePath != "" {
		base := filepath.Base(res.FilePath)
		filename = strings.TrimSuffix(base, filepath.Ext(base)) + ".lrc"
	}
	if outdir == "" && filename == "" && res.FilePath == "" {
		return models.Inputs{}, fmt.Errorf("invalid scan result: missing file path and output destination")
	}
	return models.Inputs{
		Track:        res.Track,
		Outdir:       outdir,
		Filename:     filename,
		SourcePath:   res.FilePath,
		ScanResultID: res.ID,
		OutputPaths: []models.OutputPath{{
			Outdir:   outdir,
			Filename: filename,
		}},
	}, nil
}
