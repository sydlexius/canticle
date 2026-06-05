package scan

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/pathutil"
	"github.com/sydlexius/mxlrcgo-svc/internal/scanner"
)

// LibraryLister lists configured library roots.
type LibraryLister interface {
	List(ctx context.Context) ([]models.Library, error)
}

// ResultStore persists scan results.
type ResultStore interface {
	Upsert(ctx context.Context, libraryID int64, results []models.ScanResult, opts UpsertOptions) error
}

// LibraryScanner scans a library path.
type LibraryScanner interface {
	ScanLibrary(ctx context.Context, root string, opts scanner.ScanOptions) ([]models.ScanResult, error)
}

// OnCompleteFunc is called after a library scan has been persisted.
type OnCompleteFunc func(ctx context.Context, lib models.Library, results []models.ScanResult) error

// Scheduler periodically scans configured libraries and persists results.
type Scheduler struct {
	Libraries      LibraryLister
	Results        ResultStore
	Scanner        LibraryScanner
	Options        scanner.ScanOptions
	Interval       time.Duration
	MaxRuntime     time.Duration
	OnScanComplete OnCompleteFunc
}

// RunOnce scans every configured library exactly once.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if s.Libraries == nil {
		return fmt.Errorf("scan: scheduler libraries dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	libs, err := s.Libraries.List(ctx)
	if err != nil {
		return fmt.Errorf("scan: list libraries: %w", err)
	}
	return s.RunOnceFor(ctx, libs)
}

// RunOnceFor scans the supplied libraries exactly once. Callers that need to
// restrict a scan to a subset of configured libraries resolve them upstream
// and pass the slice directly; RunOnce delegates here after listing all
// configured libraries.
func (s *Scheduler) RunOnceFor(ctx context.Context, libs []models.Library) error {
	if s.Results == nil {
		return fmt.Errorf("scan: scheduler results dependency is nil")
	}
	if s.Scanner == nil {
		return fmt.Errorf("scan: scheduler scanner dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, v := range libs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.scanAndPersist(ctx, v, v.Path); err != nil {
			return err
		}
	}
	return nil
}

// RunOnceForPath scans a single directory subtree and persists the results
// against lib, then runs the completion callback. The filesystem watcher uses
// this to react to events without rescanning every configured library. path
// should be a directory within lib.Path; because results are keyed by
// (library_id, file_path), only the touched files are upserted.
func (s *Scheduler) RunOnceForPath(ctx context.Context, lib models.Library, path string) error {
	if s.Results == nil {
		return fmt.Errorf("scan: scheduler results dependency is nil")
	}
	if s.Scanner == nil {
		return fmt.Errorf("scan: scheduler scanner dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The path originates from a filesystem watcher reacting to events; refuse to
	// scan or persist anything outside the owning library root so an unexpected
	// event path can never widen the scan beyond the configured library.
	if !pathutil.WithinRoot(lib.Path, path) {
		return fmt.Errorf("scan: path %q is outside library root %q", path, lib.Path)
	}
	return s.scanAndPersist(ctx, lib, path)
}

// scanAndPersist scans path, stamps results with lib.ID and a default status,
// upserts them, and invokes OnScanComplete.
func (s *Scheduler) scanAndPersist(ctx context.Context, lib models.Library, path string) error {
	results, err := s.Scanner.ScanLibrary(ctx, path, s.Options)
	if err != nil {
		return fmt.Errorf("scan: scan library %d: %w", lib.ID, err)
	}
	for i := range results {
		results[i].LibraryID = lib.ID
		if results[i].Status == "" {
			results[i].Status = StatusPending
		}
	}
	upsertOpts := UpsertOptions{ForceStatus: s.Options.Update || s.Options.Upgrade}
	if err := s.Results.Upsert(ctx, lib.ID, results, upsertOpts); err != nil {
		return fmt.Errorf("scan: persist library %d: %w", lib.ID, err)
	}
	if s.OnScanComplete != nil {
		if err := s.OnScanComplete(ctx, lib, results); err != nil {
			return fmt.Errorf("scan: complete library %d: %w", lib.ID, err)
		}
	}
	return nil
}

// Run scans immediately and then repeats at Interval until ctx is canceled or
// MaxRuntime elapses. If Interval is zero or negative, it performs one scan.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.MaxRuntime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.MaxRuntime)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.RunOnce(ctx); err != nil {
		return err
	}
	if s.Interval <= 0 {
		return nil
	}

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				return err
			}
		}
	}
}
