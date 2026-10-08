package prune

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
)

// EntryProvablyStale reports whether an output_paths entry whose directory is
// MISSING can be treated as a stale pre-relink path rather than a real second
// copy whose folder is momentarily gone (#1430). It is the one definition the
// unattended worker (to settle a row with the entry skipped) and the attended
// reconcile (to drop it) share, so the two cannot disagree. All three must hold:
//
//   - a configured library root that rootOnline reports online contains the
//     entry's directory (an unmounted share or an unconfigured path proves
//     nothing);
//   - the work item has at least one linked scan_results row, so the test below
//     has information (a row with no links, such as webhook-enqueued work, gives
//     no evidence either way); and
//   - no linked scan_results file, and not the row's own source, lies inside the
//     entry's directory (a file there means a real second copy).
//
// This proves "no tracked file lives in that directory and its library is
// mounted". It does NOT prove the folder was never a real copy that lost its
// files, or that an untracked file is absent from it. The caller must already
// have established that the directory is missing.
func (p *Pruner) EntryProvablyStale(ctx context.Context, id int64, sourcePath string, e models.OutputPath, rootOnline func(root string) bool) (bool, error) {
	roots, err := p.LibraryRoots(ctx)
	if err != nil {
		return false, err
	}
	files, links, err := p.linkedFiles(ctx, id, sourcePath)
	if err != nil {
		return false, err
	}
	return provablyStale(newRootSet(roots), rootOnline, files, links, e.Outdir), nil
}

// provablyStale is the predicate over already-loaded evidence: roots are the
// configured library roots, files the linked scan_results files plus the row's
// source, links the number of linked scan_results rows.
//
// Every doubt answers false. Paths are compared in the RESOLVED spelling of their
// root (rootSet.toResolved), so a linked file recorded under a symlinked root's
// configured spelling and an entry directory in its resolved spelling name the
// same place. A directory under a root whose symlink did not resolve on this call
// is refused outright: its resolved-spelling files cannot be recognized, and the
// caller's cached online answer may predate the mount dropping. A file that
// matches no root, or matches ambiguously, is kept as written. rootOnline is
// asked about the CONFIGURED root.
func provablyStale(roots rootSet, rootOnline func(string) bool, files []string, links int, dir string) bool {
	root, ok := roots.rootOf(dir)
	if !ok || links == 0 || roots.unresolved[root] || !rootOnline(root) {
		return false
	}
	rdir, ok := roots.toResolved(dir)
	if !ok {
		return false
	}
	spelling, ok := roots.match(dir)
	if !ok || !subtreeIntact(spelling, dir) {
		return false
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		rf, ok := roots.toResolved(f)
		if !ok {
			// Under no (or an ambiguous) root: compare as written, and an
			// ambiguous one under either spelling it could carry is unproven.
			if _, amb := pathutil.ContainingRoot(roots.spellings, f); amb {
				return false
			}
			rf = f
		}
		if pathutil.WithinRoot(rdir, rf) || pathutil.WithinRoot(dir, f) {
			return false
		}
	}
	return true
}

// subtreeIntact reports whether the path from root down to dir shows no sign
// that a sub-share went away under a populated root (#1430). RootOnline asks
// only about the root, so a dangling symlink or an unmounted mount point (which
// reads as an EMPTY directory) inside a live root would otherwise look like a
// stale path. Every component between root and dir is Lstat'ed, in the spelling
// the caller matched; the root's own symlink is not a component. It answers
// false (not proven) when:
//
//   - any component is a symlink, dangling or not;
//   - an Lstat fails with anything other than not-exist;
//   - the nearest existing ancestor is not a directory; or
//   - the nearest existing ancestor below the root is an empty directory (the
//     same "at least one entry" rule dirPopulated applies to a root).
//
// When the nearest existing ancestor is the root itself, rootOnline's answer
// stands. Nothing is cached: one Lstat chain per missing entry, and the
// populated probe reads at most one directory entry.
func subtreeIntact(root, dir string) bool {
	root = filepath.Clean(root)
	nearest := ""
	for cur := filepath.Clean(dir); cur != root; cur = filepath.Dir(cur) {
		if cur == filepath.Dir(cur) {
			return false // walked off the top without meeting the root
		}
		info, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return false
		case info.Mode()&fs.ModeSymlink != 0, !info.IsDir():
			return false
		}
		if nearest == "" {
			nearest = cur
		}
	}
	return nearest == "" || dirPopulated(nearest)
}

// linkedFiles lists the file paths of every scan_results row linked to the work
// item, plus the row's own source path, and the count of linked rows.
func (p *Pruner) linkedFiles(ctx context.Context, id int64, sourcePath string) (files []string, links int, _ error) {
	files = []string{sourcePath}
	if err := queryRows(ctx, p.db,
		`SELECT sr.file_path FROM work_queue_scan_results j
         JOIN scan_results sr ON sr.id = j.scan_result_id
         WHERE j.work_queue_id = ?`, []any{id}, func(rows *sql.Rows) error {
			var f string
			if err := rows.Scan(&f); err != nil {
				return err
			}
			links++
			if f != "" {
				files = append(files, f)
			}
			return nil
		}); err != nil {
		return nil, 0, fmt.Errorf("prune: load files linked to work_queue %d: %w", id, err)
	}
	return files, links, nil
}
