package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

// PendingResultStore reads and updates scan results eligible for queueing.
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
	return enqueued, cacheHits, nil
}

// OnScanComplete adapts EnqueuePending to Scheduler.OnScanComplete.
func (e *Enqueuer) OnScanComplete(ctx context.Context, lib models.Library, _ []models.ScanResult) error {
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
