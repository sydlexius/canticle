package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

// PendingResultStore reads and updates scan results eligible for queueing.
type PendingResultStore interface {
	ListPendingByLibrary(ctx context.Context, libraryID int64) ([]models.ScanResult, error)
	SetStatus(ctx context.Context, ids []int64, status string) error
}

// LyricsCache reports whether lyrics already exist for a scanned track.
type LyricsCache interface {
	LookupFallback(ctx context.Context, artist, title string) (string, error)
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
}

// EnqueuePending reads pending scan results for libraryID, skips cache hits,
// and enqueues cache misses for worker processing.
func (e *Enqueuer) EnqueuePending(ctx context.Context, libraryID int64) error {
	if e.Results == nil {
		return fmt.Errorf("scan: enqueuer results dependency is nil")
	}
	if e.Cache == nil {
		return fmt.Errorf("scan: enqueuer cache dependency is nil")
	}
	if e.Queue == nil {
		return fmt.Errorf("scan: enqueuer queue dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	results, err := e.Results.ListPendingByLibrary(ctx, libraryID)
	if err != nil {
		return fmt.Errorf("scan: list pending for enqueue: %w", err)
	}

	for _, res := range results {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := e.Cache.LookupFallback(ctx, res.Track.ArtistName, res.Track.TrackName)
		switch {
		case err == nil:
			if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusDone); err != nil {
				return fmt.Errorf("scan: mark cache hit done %d: %w", res.ID, err)
			}
			continue
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("scan: cache lookup %d: %w", res.ID, err)
		}

		if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusProcessing); err != nil {
			return fmt.Errorf("scan: reserve result %d: %w", res.ID, err)
		}
		inputs, err := scanInputs(res)
		if err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return fmt.Errorf("scan: build inputs for result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return fmt.Errorf("scan: build inputs for result %d: %w", res.ID, err)
		}
		if _, err := e.Queue.Enqueue(ctx, inputs, e.Priority); err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return fmt.Errorf("scan: enqueue result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return fmt.Errorf("scan: enqueue result %d: %w", res.ID, err)
		}
	}
	return nil
}

// OnScanComplete adapts EnqueuePending to Scheduler.OnScanComplete.
func (e *Enqueuer) OnScanComplete(ctx context.Context, lib models.Library, _ []models.ScanResult) error {
	return e.EnqueuePending(ctx, lib.ID)
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
