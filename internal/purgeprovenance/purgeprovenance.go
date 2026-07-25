// Package purgeprovenance bulk-deletes LRC/TXT sidecars matching a provenance
// filter (an exact [source:] tag, or the no-tag "inherited/foreign" cohort) and
// requeues the coupled work_queue/scan_results rows so the next scan re-fetches
// from current or better providers (issue #474).
//
// Unlike every other reconcile command to date, this one deletes real files
// from disk (not just database rows), so it follows a stricter discipline:
// dry-run by default, a caller-supplied Report invoked BEFORE each delete so a
// backup can be written and fsynced write-ahead, symlinked sidecars are never
// touched, and enumeration never leaves the configured library roots.
package purgeprovenance

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
)

// timeFormat matches the RFC3339-ish stamp the queue package writes for
// next_attempt_at, so a reset row sorts and compares consistently with rows
// written by the rest of the system.
const timeFormat = "2006-01-02T15:04:05Z"

func formatNow() string {
	return time.Now().UTC().Format(timeFormat)
}

// Filter selects which sidecars are in scope for a purge run. Exactly one of
// Source (non-empty) or NoSource should be set by the caller; matches checks
// whichever is configured.
type Filter struct {
	// Source, when non-empty, matches sidecars whose [source:] tag equals this
	// value exactly.
	Source string
	// NoSource matches sidecars carrying no [source:] tag at all -- the
	// inherited/foreign cohort canticle never wrote.
	NoSource bool
}

// matches reports whether a sidecar's read [source:] value is in scope.
func (f Filter) matches(source string) bool {
	if f.NoSource {
		return source == ""
	}
	return source == f.Source
}

// Options configures a purge run.
type Options struct {
	// Roots are the library directory trees to walk for *.lrc / *.txt sidecars.
	// Confines every candidate to these roots; walking never leaves them.
	Roots []string
	// LibraryID, when non-nil, narrows the database lookup (linking a sidecar to
	// its scan_results/work_queue rows) to one library.
	LibraryID *int64
	// Filter selects which sidecars are in scope.
	Filter Filter
	// DryRun computes and reports the purge set without deleting anything or
	// mutating the database.
	DryRun bool
	// Report, when set, is invoked once per matched, non-in-flight sidecar. In a
	// dry run it is a preview (no mutation follows). Under apply it is invoked
	// BEFORE the file is deleted, so a Report error aborts that sidecar's
	// deletion (backup-first): the caller writes and fsyncs a restorable JSONL
	// record here.
	Report func(Record) error
}

// Record describes one matched sidecar and the database rows coupled to it, for
// preview output and the restorable JSONL backup.
type Record struct {
	// Path is the matched sidecar file (.lrc or .txt).
	Path string
	// ScanResultIDs are the scan_results rows whose expected output is this
	// sidecar (usually one; more than one only when distinct scan_results rows
	// happen to share the exact same output path, e.g. two audio formats of the
	// same track).
	ScanResultIDs []int64
	// WorkItemIDs are the work_queue rows linked to those scan_results rows,
	// deduplicated.
	WorkItemIDs []int64
}

// Result tallies a purge run.
type Result struct {
	Scanned           int // .lrc/.txt sidecars examined (symlinks excluded)
	Matched           int // sidecars whose provenance matched the filter
	Deleted           int // sidecars actually removed from disk (apply only)
	ScanResultsReset  int // scan_results rows reset to 'pending'
	WorkItemsRequeued int // work_queue rows reset to 'deferred' for re-fetch
	SkippedSymlink    int // symlinked sidecars never followed or touched
	SkippedProcessing int // matched sidecars left alone: a linked work_queue row is in-flight
	Errors            int // per-file failures (read, report, delete, or reset); the run continues past them
}

// Purger locates and purges provenance-matched sidecars against db.
type Purger struct {
	db *sql.DB
}

// New returns a Purger backed by db.
func New(db *sql.DB) *Purger {
	return &Purger{db: db}
}

// srInfo is one scan_results row's identity, indexed for expected-sidecar
// lookup.
type srInfo struct {
	id int64
}

// wqLink is one work_queue row linked to a scan_results row.
type wqLink struct {
	id     int64
	status string
}

// Run walks every root for .lrc/.txt sidecars, matches each against opts.Filter
// via its [source:] tag, and -- for matches not blocked by an in-flight linked
// work_queue row -- deletes the sidecar (apply only) and resets its coupled
// database rows so the next scan re-fetches. Per-file errors are counted and
// logged but never abort the run.
func (p *Purger) Run(ctx context.Context, opts Options) (Result, error) {
	idx, wqIdx, err := p.buildIndex(ctx, opts.LibraryID)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, root := range opts.Roots {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".lrc" && ext != ".txt" {
				return nil
			}
			// Never follow a symlinked sidecar: skip it entirely, for both
			// matching and deletion. d.Type() is a Lstat-derived mode (no
			// symlink-following stat), matching the filesystem-tree WalkDir
			// contract.
			if d.Type()&fs.ModeSymlink != 0 {
				res.SkippedSymlink++
				return nil
			}
			p.processSidecar(ctx, path, idx, wqIdx, opts, &res)
			return nil
		})
		if walkErr != nil {
			return res, fmt.Errorf("purgeprovenance: walk %s: %w", root, walkErr)
		}
	}
	return res, nil
}

// processSidecar examines one on-disk sidecar and, if it matches the filter and
// is not blocked by an in-flight linked row, deletes it (apply) and resets its
// coupled database rows. Failures are counted into res.Errors and logged; they
// never abort the walk.
func (p *Purger) processSidecar(ctx context.Context, path string, idx map[string][]srInfo, wqIdx map[int64][]wqLink, opts Options, res *Result) {
	pt, rerr := lyrics.ReadProvenanceTags(path)
	if rerr != nil {
		res.Errors++
		slog.Warn("purge-provenance: read provenance failed; skipping", "path", path, "error", rerr)
		return
	}
	res.Scanned++
	if !opts.Filter.matches(pt.Source) {
		return
	}
	res.Matched++

	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	key := canonicalDir(filepath.Dir(path)) + "\x00" + stem
	srs := idx[key]

	var scanResultIDs, workItemIDs []int64
	seenWQ := make(map[int64]bool)
	processing := false
	for _, sr := range srs {
		scanResultIDs = append(scanResultIDs, sr.id)
		for _, link := range wqIdx[sr.id] {
			if link.status == "processing" {
				processing = true
			}
			if !seenWQ[link.id] {
				seenWQ[link.id] = true
				workItemIDs = append(workItemIDs, link.id)
			}
		}
	}
	if processing {
		res.SkippedProcessing++
		return
	}

	rec := Record{Path: path, ScanResultIDs: scanResultIDs, WorkItemIDs: workItemIDs}

	if opts.DryRun {
		if opts.Report != nil {
			if rerr := opts.Report(rec); rerr != nil {
				res.Errors++
				slog.Warn("purge-provenance: report failed", "path", path, "error", rerr)
			}
		}
		return
	}

	// Backup-first / write-ahead: the caller's Report writes and fsyncs the
	// restorable JSONL record before anything is deleted. A Report failure
	// skips this sidecar entirely -- it is never deleted without its record.
	if opts.Report != nil {
		if rerr := opts.Report(rec); rerr != nil {
			res.Errors++
			slog.Warn("purge-provenance: backup failed; leaving sidecar untouched", "path", path, "error", rerr)
			return
		}
	}

	if rerr := os.Remove(path); rerr != nil {
		if !os.IsNotExist(rerr) {
			res.Errors++
			slog.Warn("purge-provenance: delete failed; leaving row untouched", "path", path, "error", rerr)
			return
		}
		// Already gone (removed out-of-band since matching) -- proceed to reset
		// the coupled rows anyway, since the sidecar is confirmed absent either way.
	}
	res.Deleted++

	if len(scanResultIDs) == 0 && len(workItemIDs) == 0 {
		return
	}
	srReset, wqReset, rerr := p.resetRows(ctx, scanResultIDs, workItemIDs)
	if rerr != nil {
		res.Errors++
		slog.Warn("purge-provenance: reset rows failed (sidecar already deleted)", "path", path, "error", rerr)
		return
	}
	res.ScanResultsReset += srReset
	res.WorkItemsRequeued += wqReset
}

// resetRows resets the coupled scan_results and work_queue rows so the track
// re-fetches. Mirrors ResetInstrumental's reset shape (status='deferred',
// priority=-100 so the row is dequeue-eligible but strictly behind foreground
// work; next_attempt_at=now; last_error cleared): the same re-queue semantics
// scan reconcile already uses when it deletes a stale on-disk marker.
//
// Both updates guard on the row not already being in the target state /
// in-flight, so a race that claimed a row 'processing' between the caller's
// linkage check and this call leaves it untouched rather than corrupting an
// in-flight write -- the same residual TOCTOU window prune.deletePruned notes
// and accepts for the same reason (the alternative is holding a transaction
// open across the file delete, which this package deliberately does not do).
func (p *Purger) resetRows(ctx context.Context, scanResultIDs, workItemIDs []int64) (srReset, wqReset int, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("purgeprovenance: begin reset tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := formatNow()
	for _, id := range workItemIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE work_queue
             SET status = 'deferred',
                 priority = -100,
                 attempts = 0,
                 next_attempt_at = ?,
                 last_error = ''
             WHERE id = ? AND status != 'processing'`,
			now, id)
		if err != nil {
			return 0, 0, fmt.Errorf("purgeprovenance: reset work_queue %d: %w", id, err)
		}
		wqReset += rowsAffected(res)
	}
	for _, id := range scanResultIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE scan_results SET status = 'pending' WHERE id = ? AND status != 'pending'`,
			id)
		if err != nil {
			return 0, 0, fmt.Errorf("purgeprovenance: reset scan_results %d: %w", id, err)
		}
		srReset += rowsAffected(res)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("purgeprovenance: commit reset tx: %w", err)
	}
	return srReset, wqReset, nil
}

// buildIndex loads every in-scope scan_results row's expected-sidecar identity
// (keyed by canonical output directory + filename stem, extension-agnostic so a
// .lrc and a .txt sidecar for the same track both resolve to the same
// scan_results row) plus the work_queue rows linked to each, so per-file
// database lookups during the walk are pure in-memory map reads rather than a
// query per sidecar.
func (p *Purger) buildIndex(ctx context.Context, libraryID *int64) (map[string][]srInfo, map[int64][]wqLink, error) {
	idx := make(map[string][]srInfo)
	query := `SELECT id, outdir, filename FROM scan_results WHERE filename != ''`
	var args []any
	if libraryID != nil {
		query += ` AND library_id = ?`
		args = append(args, *libraryID)
	}
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: query scan_results: %w", err)
	}
	srIDs := make(map[int64]bool)
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var outdir, filename string
			if serr := rows.Scan(&id, &outdir, &filename); serr != nil {
				err = fmt.Errorf("purgeprovenance: scan scan_results row: %w", serr)
				return
			}
			stem := strings.TrimSuffix(filename, filepath.Ext(filename))
			key := canonicalDir(outdir) + "\x00" + stem
			idx[key] = append(idx[key], srInfo{id: id})
			srIDs[id] = true
		}
		if rerr := rows.Err(); rerr != nil {
			err = fmt.Errorf("purgeprovenance: iterate scan_results: %w", rerr)
		}
	}()
	if err != nil {
		return nil, nil, err
	}

	wqIdx := make(map[int64][]wqLink)
	wqRows, err := p.db.QueryContext(ctx,
		`SELECT j.scan_result_id, wq.id, wq.status
         FROM work_queue_scan_results j
         JOIN work_queue wq ON wq.id = j.work_queue_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: query work_queue links: %w", err)
	}
	defer func() { _ = wqRows.Close() }()
	for wqRows.Next() {
		var srID, wqID int64
		var status string
		if serr := wqRows.Scan(&srID, &wqID, &status); serr != nil {
			return nil, nil, fmt.Errorf("purgeprovenance: scan work_queue link: %w", serr)
		}
		if !srIDs[srID] {
			// Not an in-scope scan_results row (e.g. excluded by --library); skip
			// to keep the index scoped to what buildIndex actually loaded.
			continue
		}
		wqIdx[srID] = append(wqIdx[srID], wqLink{id: wqID, status: status})
	}
	if rerr := wqRows.Err(); rerr != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: iterate work_queue links: %w", rerr)
	}
	return idx, wqIdx, nil
}

// canonicalDir returns a canonical form of a directory path for comparison:
// made absolute (relative to the current working directory) then Cleaned.
// Symlinks are deliberately NOT resolved -- matching provenance.go's
// canonicalDir, which this mirrors -- so a stored outdir that no longer exists
// (or a symlinked component) still compares consistently rather than erroring.
func canonicalDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}
