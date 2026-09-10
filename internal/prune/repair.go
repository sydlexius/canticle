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
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/models"
)

// RepairedRow reports one work_queue row whose single output_paths entry was
// rewritten because it named a directory that no longer exists while the row's
// own outdir/filename named one that does.
type RepairedRow struct {
	WorkItemID     int64
	OldOutputPaths []models.OutputPath
	NewOutputPaths []models.OutputPath
}

// RepairOptions configures RepairOutputPaths.
type RepairOptions struct {
	// LibraryID scopes the repair to one library's rows, through the same
	// work_queue_scan_results junction Sweep's library scoping uses. nil repairs
	// every library.
	LibraryID *int64
	// DryRun previews without writing: candidates are still gathered and
	// classified, and Report still fires for every row that WOULD be repaired,
	// but no transaction or UPDATE runs.
	DryRun bool
	// Report fires once per repaired (or, in dry-run, would-repair) row. On a
	// real run it fires INSIDE the row's transaction, after the UPDATE and before
	// COMMIT, so a Report error rolls that row back (backup-first) and aborts
	// the pass.
	Report func(RepairedRow) error

	// beforeWrite, when set, runs between gather and a row's write. Test-only
	// seam for simulating a row that changes underneath the repair.
	beforeWrite func(id int64)
}

// RepairResult totals a repair pass.
type RepairResult struct {
	Repaired []RepairedRow
	// SkippedAmbiguous counts rows whose output_paths holds more than one entry.
	// Such rows come from identityrepair's output_paths union, where the other
	// entry is usually a different file in a different directory; the row alone
	// cannot say which entry (if any) a relink left stale, so none is touched.
	SkippedAmbiguous int
	// SkippedUnfixable counts rows outside the relink shape (see relinkShape)
	// and rows whose own outdir does not exist (or is not a directory): there is
	// nothing in the row to repair output_paths TO.
	SkippedUnfixable int
	// SkippedStatError counts rows where statting the entry's dir or the row's
	// outdir failed with anything other than not-exist (EACCES, ESTALE, EIO,
	// ...). An unreadable directory is not proof of a missing one.
	SkippedStatError int
	// SkippedMalformed counts rows whose output_paths is not a JSON array of
	// output paths; they are left for a human.
	SkippedMalformed int
	// SkippedRaced counts rows whose status, output_paths, outdir, or filename
	// changed between gather and write, so the compare-and-set UPDATE matched
	// nothing.
	SkippedRaced int
}

// repairStatusFilter is the worker's dequeue-eligible set. 'processing' is
// owned by the worker; 'done' is excluded by design: a done row is never
// dequeued, so its output_paths is never written to. A row later revived out of
// 'done' is not repaired until reconcile-paths runs again.
const repairStatusFilter = `status IN ('pending', 'failed', 'deferred')`

// repairHealthyFilter excludes, in SQL and before any stat, rows whose
// output_paths is exactly one entry equal to the row's own {outdir, filename},
// so healthy rows cost no filesystem call; it is the only healthy-row check.
// It compares by value with json_extract rather than by string, so for valid
// UTF-8 it does not depend on the encoder's escaping (invalid UTF-8 is skipped
// in Go, see relinkShape), and maps a missing key to empty via
// COALESCE (as Go's decoder does) so a NULL never silently drops a row. Invalid JSON is kept
// (CASE short-circuits json_valid) and counted as malformed in Go.
const repairHealthyFilter = `NOT (CASE WHEN json_valid(output_paths) THEN
    json_array_length(output_paths) = 1
    AND COALESCE(json_extract(output_paths, '$[0].outdir'), '') = outdir
    AND COALESCE(json_extract(output_paths, '$[0].filename'), '') = filename
  ELSE 0 END)`

// RepairOutputPaths fixes the write-step defect #921 describes: prune's
// heuristic relink (relinkOne) updated a work_queue row's source_path/outdir/
// filename to a file's new location but, before the #921 forward fix, left
// output_paths naming the pre-move directory. internal/worker prefers a
// non-empty output_paths over outdir/filename, so such a row kept writing to a
// vanished directory, failing, and re-fetching on every backoff expiry. Sweep
// cannot re-select it, because its source_path already resolves.
//
// Only the shape that bug produces is repaired: a dequeue-eligible row (see
// repairStatusFilter) whose output_paths has EXACTLY ONE entry, that entry
// differs from {outdir, filename}, its directory does not exist
// (fs.ErrNotExist), and the row's own outdir does exist. That entry is
// rewritten to {outdir, filename}. The relink keeps the row's fields in step,
// so relinkShape must also hold. Everything else is counted and left alone.
//
// Known limitation: the row's outdir is not checked against a configured
// library root, so a row left on a since-removed library whose directory still
// exists is repaired to point there.
//
// Each repair is its own transaction: a compare-and-set UPDATE (status plus the
// output_paths/outdir/filename values read at gather), then Report, then
// COMMIT, so a row is never committed without its backup record.
func (p *Pruner) RepairOutputPaths(ctx context.Context, opts RepairOptions) (RepairResult, error) {
	query := `SELECT id, outdir, filename, source_path, output_paths FROM work_queue
              WHERE ` + repairStatusFilter + ` AND output_paths != '' AND outdir != '' AND ` + repairHealthyFilter
	var args []any
	if opts.LibraryID != nil {
		query = `SELECT id, outdir, filename, source_path, output_paths FROM work_queue
                 WHERE ` + repairStatusFilter + ` AND output_paths != '' AND outdir != '' AND ` + repairHealthyFilter + `
                   AND id IN (SELECT j.work_queue_id FROM work_queue_scan_results j
                              JOIN scan_results sr ON sr.id = j.scan_result_id WHERE sr.library_id = ?)`
		args = append(args, *opts.LibraryID)
	}

	type candidateRow struct {
		id               int64
		outdir, filename string
		sourcePath       string
		outputPaths      string
	}
	var candidates []candidateRow
	if err := queryRows(ctx, p.db, query, args, func(rows *sql.Rows) error {
		var c candidateRow
		if err := rows.Scan(&c.id, &c.outdir, &c.filename, &c.sourcePath, &c.outputPaths); err != nil {
			return err
		}
		candidates = append(candidates, c)
		return nil
	}); err != nil {
		return RepairResult{}, fmt.Errorf("prune: gather output_paths repair candidates: %w", err)
	}

	var res RepairResult
	for _, c := range candidates {
		var paths []models.OutputPath
		if err := json.Unmarshal([]byte(c.outputPaths), &paths); err != nil {
			res.SkippedMalformed++
			continue
		}
		if len(paths) > 1 {
			res.SkippedAmbiguous++
			continue
		}
		if len(paths) == 0 {
			continue
		}
		if !relinkShape(c.outdir, c.filename, c.sourcePath, paths[0]) {
			res.SkippedUnfixable++
			continue
		}
		newEntry := models.OutputPath{Outdir: c.outdir, Filename: c.filename}
		entryState, rowState := statDir(paths[0].Outdir), statDir(c.outdir)
		switch {
		case entryState == dirStatError || rowState == dirStatError:
			res.SkippedStatError++
			continue
		case entryState != dirMissing:
			continue // the entry's path exists (dir or not); not the #921 shape
		case rowState != dirPresent:
			res.SkippedUnfixable++
			continue
		}
		fixed := []models.OutputPath{newEntry}
		row := RepairedRow{WorkItemID: c.id, OldOutputPaths: paths, NewOutputPaths: fixed}
		if opts.DryRun {
			res.Repaired = append(res.Repaired, row)
			if opts.Report != nil {
				if err := opts.Report(row); err != nil {
					return res, fmt.Errorf("prune: report repaired work_queue %d: %w", c.id, err)
				}
			}
			continue
		}
		if opts.beforeWrite != nil {
			opts.beforeWrite(c.id)
		}
		applied, err := p.applyRepair(ctx, c.id, c.outputPaths, c.outdir, c.filename, row, opts.Report)
		if err != nil {
			return res, err
		}
		if !applied {
			res.SkippedRaced++
			continue
		}
		res.Repaired = append(res.Repaired, row)
	}
	return res, nil
}

// applyRepair runs one row's BEGIN -> compare-and-set UPDATE -> Report ->
// COMMIT. It returns false (nothing committed) when the row changed since
// gather; a Report or database error rolls the row back and is returned.
func (p *Pruner) applyRepair(ctx context.Context, id int64, oldJSON, outdir, filename string, row RepairedRow, report func(RepairedRow) error) (applied bool, retErr error) {
	newJSON, err := json.Marshal(row.NewOutputPaths)
	if err != nil {
		return false, fmt.Errorf("prune: marshal repaired output_paths for work_queue %d: %w", id, err)
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("prune: begin output_paths repair for work_queue %d: %w", id, err)
	}
	defer func() {
		if !applied || retErr != nil {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx,
		`UPDATE work_queue SET output_paths = ?
		 WHERE id = ? AND `+repairStatusFilter+` AND output_paths = ? AND outdir = ? AND filename = ?`,
		string(newJSON), id, oldJSON, outdir, filename)
	if err != nil {
		return false, fmt.Errorf("prune: repair output_paths for work_queue %d: %w", id, err)
	}
	if rowsAffected(result) == 0 {
		return false, nil
	}
	if report != nil {
		if err := report(row); err != nil {
			return false, fmt.Errorf("prune: report repaired work_queue %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("prune: commit output_paths repair for work_queue %d: %w", id, err)
	}
	return true, nil
}

// relinkShape reports whether a row and its single stale entry look like what
// relinkOne leaves behind: the entry names the row's filename under an absolute
// directory, the row's outdir is its source file's directory, and every string
// is valid UTF-8. A row failing it (e.g. queue.cancelByLibrary filtering a
// merged row down to another library's entry) is not proven to be #921's.
func relinkShape(outdir, filename, sourcePath string, e models.OutputPath) bool {
	for _, s := range []string{outdir, filename, e.Outdir, e.Filename} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	return e.Filename == filename && filepath.IsAbs(e.Outdir) &&
		filepath.Clean(outdir) == filepath.Dir(sourcePath)
}

type dirState int

const (
	dirPresent   dirState = iota
	dirMissing            // stat returned fs.ErrNotExist
	dirNotDir             // exists but is not a directory
	dirStatError          // any other stat error: state unknown
)

// statDir classifies path. Only fs.ErrNotExist counts as missing; every other
// stat error is reported as dirStatError so a permission or mount failure is
// never mistaken for a vanished directory.
func statDir(path string) dirState {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return dirPresent
	case err == nil:
		return dirNotDir
	case errors.Is(err, fs.ErrNotExist):
		return dirMissing
	default:
		return dirStatError
	}
}
