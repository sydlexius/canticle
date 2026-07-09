// Package prune reconciles the durable work queue and scan-result cache against
// the filesystem: rows whose source audio file no longer exists on disk are
// deleted so a renamed/merged/deleted track cannot leave a permanently-failing
// or wedged row behind (#453).
//
// The filesystem is the sole authority for "gone": a source path is pruned only
// when os.Stat of that path (Exact granularity) or its directory (Directory
// granularity) fails. The same primitive backs three callers -- a watcher-
// reactive prune on Remove/Rename events, a lazy periodic sweep, and the
// `scan reconcile-paths` CLI -- so the reconciliation rule lives in exactly one
// place.
package prune

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/pathutil"
)

// Granularity selects how a candidate source path is tested for existence.
type Granularity int

const (
	// Directory stats only the parent directory of each candidate source path.
	// A source is considered gone when its directory is gone. This is the cheap
	// strategy for the unattended periodic sweep (one stat per directory rather
	// than per file), matching the ticket's disk-I/O constraint. Single-file
	// renames within a surviving directory are NOT caught at this granularity.
	Directory Granularity = iota
	// Exact stats every candidate source path individually, so a single-file
	// rename within a still-existing directory is caught. Used by the reactive
	// prune and the operator-invoked CLI.
	Exact
)

// PrunedRow describes one gone source path and the rows removed for it, for
// summaries and JSONL backups.
type PrunedRow struct {
	// SourcePath is the vanished audio file path (scan_results.file_path /
	// work_queue.source_path).
	SourcePath string
	// ScanResultIDs are the scan_results rows removed for this source.
	ScanResultIDs []int64
	// WorkItemIDs are the work_queue rows removed for this source.
	WorkItemIDs []int64
	// Inputs carries the removed work_queue rows' restorable payload for backup.
	Inputs []models.Inputs
}

// Result reports the totals and per-row detail of a prune (or, in dry-run, what
// would be pruned).
type Result struct {
	ScanResults int
	WorkItems   int
	Pruned      []PrunedRow
}

// SweepOptions controls a whole-scope reconciliation sweep.
type SweepOptions struct {
	// LibraryID, when non-nil, restricts candidates to that library's scan_results
	// (and their linked work_queue rows). Nil sweeps every configured library and
	// also catches link-less work_queue rows by source_path.
	LibraryID *int64
	// Granularity selects the directory-cheap or exact stat strategy.
	Granularity Granularity
	// DryRun computes and reports the prune set without mutating the database.
	DryRun bool
	// Report, when set, is invoked once per pruned source path before the delete
	// commits, so a caller can back up or log each row.
	Report func(PrunedRow) error
}

// Pruner reconciles work_queue and scan_results against the filesystem.
type Pruner struct {
	db *sql.DB
}

// New returns a Pruner backed by db.
func New(db *sql.DB) *Pruner {
	return &Pruner{db: db}
}

// PrunePath reconciles the rows whose source path is at or under path, statting
// each candidate source file individually (Exact granularity). It is the
// disk-free reactive entry point: the caller already learned path vanished from
// a filesystem event, so this only touches the database. A removed directory is
// handled naturally -- every candidate source under it fails os.Stat.
func (p *Pruner) PrunePath(ctx context.Context, path string) (Result, error) {
	return p.reconcile(ctx, scope{prefix: path, scoped: true}, nil, Exact, false, nil)
}

// Sweep reconciles every candidate source path in scope. Directory granularity
// is the cheap backstop; Exact is the thorough operator-invoked pass. With
// DryRun set it reports without mutating.
func (p *Pruner) Sweep(ctx context.Context, opts SweepOptions) (Result, error) {
	return p.reconcile(ctx, scope{}, opts.LibraryID, opts.Granularity, opts.DryRun, opts.Report)
}

// scope narrows candidate source paths to those at or under prefix. A zero scope
// (scoped=false) matches every candidate.
type scope struct {
	prefix string
	scoped bool
}

func (s scope) matches(p string) bool {
	if !s.scoped {
		return true
	}
	return p == s.prefix || pathutil.WithinRoot(s.prefix, p)
}

// candidate is a source path with the row detail needed to prune and back it up.
type candidate struct {
	scanResultIDs []int64
	workItems     []workRow
	// processing is true when any linked work_queue row is still 'processing',
	// so the whole source is deferred (the worker owns it) to avoid a half-prune.
	processing bool
}

// workRow is a work_queue row's restorable detail.
type workRow struct {
	id     int64
	inputs models.Inputs
}

// reconcile is the shared core behind PrunePath and Sweep. It gathers candidate
// source paths, uses os.Stat (per granularity) as the sole authority for gone,
// applies the in-flight guard, and atomically deletes the gone rows across
// work_queue and scan_results (the junction is cleaned by ON DELETE CASCADE).
func (p *Pruner) reconcile(ctx context.Context, sc scope, libraryID *int64, g Granularity, dryRun bool, report func(PrunedRow) error) (Result, error) {
	bySource, err := p.gatherCandidates(ctx, sc, libraryID)
	if err != nil {
		return Result{}, err
	}
	// Load the set of library roots that currently exist on disk. A source is
	// only ever judged "gone" when it sits under an available root, so an entire
	// library that is merely unmounted (its mountpoint present but empty, making
	// every child os.Stat return ENOENT) cannot be mass-deleted. A root that is
	// genuinely removed is left to `library remove`, not this reconciler.
	roots, err := p.availableRoots(ctx)
	if err != nil {
		return Result{}, err
	}

	statCache := make(map[string]bool) // directory -> exists (Directory granularity)
	var res Result
	for _, src := range sortedKeys(bySource) {
		c := bySource[src]
		if !underAvailableRoot(src, roots) {
			continue
		}
		if !gone(src, g, statCache) {
			continue
		}
		if c.processing {
			// The worker still owns this source; deleting its scan_results row now
			// would null work_queue.scan_result_id (migration 009, ON DELETE SET
			// NULL) and cascade away the junction row mid-flight. Defer to a later
			// pass once the worker finishes or fails the item.
			continue
		}
		row := PrunedRow{SourcePath: src, ScanResultIDs: c.scanResultIDs}
		for _, w := range c.workItems {
			row.WorkItemIDs = append(row.WorkItemIDs, w.id)
			row.Inputs = append(row.Inputs, w.inputs)
		}
		res.Pruned = append(res.Pruned, row)
	}

	if dryRun {
		// Dry-run reports the intended prune set (gather-time counts), since no
		// delete runs to measure.
		for _, row := range res.Pruned {
			res.ScanResults += len(row.ScanResultIDs)
			res.WorkItems += len(row.WorkItemIDs)
			if report != nil {
				if err := report(row); err != nil {
					return Result{}, fmt.Errorf("prune: report: %w", err)
				}
			}
		}
		return res, nil
	}
	if len(res.Pruned) == 0 {
		return res, nil
	}
	scanDeleted, workDeleted, err := p.deletePruned(ctx, res.Pruned, report)
	if err != nil {
		return Result{}, err
	}
	// Report actual deletions, not the intended set, so a row that raced into
	// 'processing' (and was therefore skipped) is not counted as pruned.
	res.ScanResults = scanDeleted
	res.WorkItems = workDeleted
	return res, nil
}

// availableRoots returns the configured library root paths that currently exist
// on disk. Deletion is confined to sources under these roots so an unmounted or
// unavailable library cannot trigger a mass prune.
func (p *Pruner) availableRoots(ctx context.Context) ([]string, error) {
	var roots []string
	if err := queryRows(ctx, p.db, `SELECT path FROM libraries WHERE path != ''`, nil, func(rows *sql.Rows) error {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if pathExists(path) {
			roots = append(roots, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: load library roots: %w", err)
	}
	return roots, nil
}

// underAvailableRoot reports whether src lies within one of the available roots.
func underAvailableRoot(src string, roots []string) bool {
	for _, root := range roots {
		if src == root || pathutil.WithinRoot(root, src) {
			return true
		}
	}
	return false
}

// gatherCandidates returns, keyed by source path, the scan_results and
// work_queue rows to consider, plus whether any linked work_queue row is
// 'processing'. Candidates come from scan_results (the library-file authority,
// library-scoped when requested) and, when unscoped, also from work_queue
// source paths so link-less rows are covered.
func (p *Pruner) gatherCandidates(ctx context.Context, sc scope, libraryID *int64) (map[string]*candidate, error) {
	bySource := make(map[string]*candidate)

	srQuery := `SELECT id, file_path FROM scan_results WHERE file_path != ''`
	var srArgs []any
	if libraryID != nil {
		srQuery += ` AND library_id = ?`
		srArgs = append(srArgs, *libraryID)
	}
	if err := queryRows(ctx, p.db, srQuery, srArgs, func(rows *sql.Rows) error {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return err
		}
		if !sc.matches(path) {
			return nil
		}
		c := ensureCandidate(bySource, path)
		c.scanResultIDs = append(c.scanResultIDs, id)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: gather scan_results: %w", err)
	}

	wqQuery := `SELECT id, artist, title, source_path, output_paths, status FROM work_queue WHERE source_path != ''`
	var wqArgs []any
	if libraryID != nil {
		// Library-scope work_queue through the junction so a scoped sweep only
		// prunes queue rows belonging to that library.
		wqQuery = `SELECT DISTINCT wq.id, wq.artist, wq.title, wq.source_path, wq.output_paths, wq.status
                   FROM work_queue wq
                   JOIN work_queue_scan_results j ON j.work_queue_id = wq.id
                   JOIN scan_results sr ON sr.id = j.scan_result_id
                   WHERE wq.source_path != '' AND sr.library_id = ?`
		wqArgs = append(wqArgs, *libraryID)
	}
	if err := queryRows(ctx, p.db, wqQuery, wqArgs, func(rows *sql.Rows) error {
		var id int64
		var artist, title, source, outputPaths, status string
		if err := rows.Scan(&id, &artist, &title, &source, &outputPaths, &status); err != nil {
			return err
		}
		if !sc.matches(source) {
			return nil
		}
		c := ensureCandidate(bySource, source)
		if status == "processing" {
			c.processing = true
			return nil
		}
		var paths []models.OutputPath
		if outputPaths != "" {
			if err := json.Unmarshal([]byte(outputPaths), &paths); err != nil {
				return fmt.Errorf("unmarshal output_paths for work_queue %d: %w", id, err)
			}
		}
		c.workItems = append(c.workItems, workRow{
			id: id,
			inputs: models.Inputs{
				Track:       models.Track{ArtistName: artist, TrackName: title},
				SourcePath:  source,
				OutputPaths: paths,
			},
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: gather work_queue: %w", err)
	}
	return bySource, nil
}

// deletePruned deletes the pruned rows in a single transaction, invoking report
// for each row before the commit so a backup is durable ahead of deletion.
//
// Both deletes guard on the SAME condition -- the linked work_queue row is not
// 'processing' -- so a row that raced into 'processing' between gather and delete
// (a worker claiming a pending row) is skipped on BOTH tables, never half-pruned.
// The work_queue guard is `status != 'processing'` (NOT an allow-list): a moved
// track whose lyrics already completed is 'done', and a reconciler for vanished
// sources must delete those too -- excluding 'done' would leak the queue row
// forever while still deleting its scan_results row. It returns the actual rows
// deleted (via RowsAffected) so the caller's totals reflect what happened, not
// just what was intended.
func (p *Pruner) deletePruned(ctx context.Context, pruned []PrunedRow, report func(PrunedRow) error) (scanDeleted, workDeleted int, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("prune: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, row := range pruned {
		if report != nil {
			if err := report(row); err != nil {
				return 0, 0, fmt.Errorf("prune: report %q: %w", row.SourcePath, err)
			}
		}
		for _, id := range row.WorkItemIDs {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM work_queue WHERE id = ? AND status != 'processing'`, id)
			if err != nil {
				return 0, 0, fmt.Errorf("prune: delete work_queue %d: %w", id, err)
			}
			workDeleted += rowsAffected(res)
		}
		for _, id := range row.ScanResultIDs {
			// Skip a scan_results row still linked to an in-flight (processing)
			// work_queue row, so a worker never has its scan_result_id nulled
			// (migration 009) and junction cascaded (010) out from under it.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM scan_results WHERE id = ?
                 AND NOT EXISTS (
                     SELECT 1 FROM work_queue_scan_results j
                     JOIN work_queue wq ON wq.id = j.work_queue_id
                     WHERE j.scan_result_id = ? AND wq.status = 'processing')`,
				id, id)
			if err != nil {
				return 0, 0, fmt.Errorf("prune: delete scan_results %d: %w", id, err)
			}
			scanDeleted += rowsAffected(res)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("prune: commit tx: %w", err)
	}
	return scanDeleted, workDeleted, nil
}

// rowsAffected returns the affected-row count, treating a driver that does not
// report it as 0 (SQLite always reports, so this is defensive).
func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

func ensureCandidate(m map[string]*candidate, src string) *candidate {
	c, ok := m[src]
	if !ok {
		c = &candidate{}
		m[src] = c
	}
	return c
}

// gone reports whether src's source file is absent, per granularity. Directory
// granularity caches directory existence so a large album is statted once.
func gone(src string, g Granularity, dirCache map[string]bool) bool {
	if g == Directory {
		dir := filepath.Dir(src)
		exists, cached := dirCache[dir]
		if !cached {
			exists = pathExists(dir)
			dirCache[dir] = exists
		}
		return !exists
	}
	return !pathExists(src)
}

// pathExists reports whether p exists. Only a definitive not-exist result counts
// as gone; any other stat error (permissions, I/O) is treated as "exists" so a
// transient error never triggers a destructive prune.
func pathExists(p string) bool {
	if _, err := os.Stat(p); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	return true
}

func queryRows(ctx context.Context, db *sql.DB, query string, args []any, scan func(*sql.Rows) error) (retErr error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func sortedKeys(m map[string]*candidate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic order keeps prune output and backups stable across runs.
	sort.Strings(keys)
	return keys
}
