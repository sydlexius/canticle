// Package lrcbackfill rewrites existing .lrc sidecars that carry compressed
// multi-timestamp lines into the expanded, one-cue-per-line form, backing up the
// pristine original alongside each rewrite. It is the backfill half of issue
// #470; the pure transform lives in internal/lrcnormalize.
package lrcbackfill

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/doxazo-net/canticle/internal/lrcnormalize"
	"github.com/doxazo-net/canticle/internal/lyrics"
)

// Options configures a backfill run over one or more library roots.
type Options struct {
	Roots  []string  // directory trees to walk for *.lrc files
	Apply  bool      // false = dry run (report only, write nothing)
	Backup io.Writer // optional JSONL sink; one {path,backup} line per applied normalization
}

// Summary tallies a backfill run.
type Summary struct {
	Scanned    int // .lrc files examined
	Normalized int // rewritten (apply) or would-be-rewritten (dry run)
	Clean      int // already expanded; skipped
	Skipped    int // symlinks and other intentional skips
	Errors     int // per-file failures (the run continues past them)
}

// backupRecord is one line of the JSONL undo trail.
type backupRecord struct {
	Path   string `json:"path"`
	Backup string `json:"backup"`
}

// Run walks each root for .lrc sidecars and normalizes stacked ones. In dry-run
// mode (Apply=false) it only reports what would change; in apply mode it rewrites
// each stacked file (backup-first) and emits a JSONL record to Backup. Per-file
// errors are counted and logged but never abort the run.
func Run(opts Options) (Summary, error) {
	var s Summary
	for _, root := range opts.Roots {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".lrc") {
				return nil
			}
			s.Scanned++
			var res Result
			var ferr error
			if opts.Apply {
				res, ferr = NormalizeFile(path)
			} else {
				res, ferr = inspect(path)
			}
			if ferr != nil {
				s.Errors++
				slog.Warn("lrcbackfill: file failed; continuing", "path", path, "error", ferr)
				return nil
			}
			switch res.Status {
			case StatusNormalized:
				s.Normalized++
				if opts.Apply && opts.Backup != nil {
					if err := writeBackupRecord(opts.Backup, path, res.Backup); err != nil {
						return fmt.Errorf("write backup record: %w", err)
					}
				}
			case StatusClean:
				s.Clean++
			case StatusSkipped:
				s.Skipped++
			}
			return nil
		})
		if walkErr != nil {
			return s, fmt.Errorf("walk %s: %w", root, walkErr)
		}
	}
	return s, nil
}

func writeBackupRecord(w io.Writer, path, backup string) error {
	line, err := json.Marshal(backupRecord{Path: path, Backup: backup})
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// Status is the outcome of processing a single .lrc file.
type Status int

const (
	// StatusClean means the file carried no stacked line; nothing was written.
	StatusClean Status = iota
	// StatusNormalized means the file was rewritten and a .lrc.orig backup exists.
	StatusNormalized
	// StatusSkipped means the file was intentionally not processed (e.g. symlink).
	StatusSkipped
)

// Result reports what happened to a single file.
type Result struct {
	Status Status
	Backup string // path of the .lrc.orig backup, set when StatusNormalized
}

// NormalizeFile expands stacked timestamps in the .lrc at path. It is the
// needs-work-gated, backup-first primitive: a file with no stacked line is left
// untouched (StatusClean); otherwise the pristine original is preserved to
// "<path>.orig" (never overwritten) and fsynced before the expanded body is
// atomically written over path.
func NormalizeFile(path string) (Result, error) {
	raw, origMode, skip, err := load(path)
	if err != nil {
		return Result{}, err
	}
	if skip {
		return Result{Status: StatusSkipped}, nil
	}
	out, changed := lrcnormalize.NormalizeBody(string(raw))
	if !changed {
		return Result{Status: StatusClean}, nil // needs-work gate: nothing to do
	}

	// Refuse to overwrite a .lrc whose .orig backup already exists: we cannot
	// verify that pre-existing backup is the true pristine original, and
	// overwriting would leave the current bytes backed up nowhere. Skip instead
	// (conservative-on-uncertainty). In normal single-tool operation this is
	// unreachable -- after one expand the file is already clean and never
	// rewritten -- so a .orig here means external interference.
	backupPath := path + ".orig"
	if _, statErr := os.Lstat(backupPath); statErr == nil {
		slog.Warn("lrcbackfill: .orig backup already exists; skipping to avoid an unverified overwrite", "path", path, "backup", backupPath)
		return Result{Status: StatusSkipped}, nil
	} else if !os.IsNotExist(statErr) {
		return Result{}, fmt.Errorf("stat backup %s: %w", backupPath, statErr)
	}

	// Backup-first: preserve the pristine original before touching the .lrc.
	if err := writeBackup(backupPath, raw, origMode); err != nil {
		return Result{}, err
	}
	// Atomic rewrite of the expanded body.
	if err := atomicWrite(path, []byte(out), origMode); err != nil {
		return Result{}, err
	}
	// One parent-dir fsync makes both new directory entries (.orig and the
	// renamed .lrc) crash-durable.
	lyrics.FsyncDir(filepath.Dir(path))
	return Result{Status: StatusNormalized, Backup: backupPath}, nil
}

// inspect reports what NormalizeFile would do, without writing anything (dry run).
func inspect(path string) (Result, error) {
	raw, _, skip, err := load(path)
	if err != nil {
		return Result{}, err
	}
	if skip {
		return Result{Status: StatusSkipped}, nil
	}
	if _, changed := lrcnormalize.NormalizeBody(string(raw)); !changed {
		return Result{Status: StatusClean}, nil
	}
	return Result{Status: StatusNormalized, Backup: path + ".orig"}, nil
}

// load reads path, returning its body and permission bits. skip is true (with a
// nil error) when the path is a symlink, which is never followed or rewritten.
func load(path string) (body []byte, mode os.FileMode, skip bool, err error) {
	fi, err := os.Lstat(path) // Lstat (not Stat) so a symlink is detected, not followed.
	if err != nil {
		return nil, 0, false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		slog.Warn("lrcbackfill: skipping symlink", "path", path)
		return nil, 0, true, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is caller-controlled library enumeration
	if err != nil {
		return nil, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, fi.Mode().Perm(), false, nil
}

// writeBackup preserves content to backupPath with exclusive-create semantics,
// fsyncing the data and chmod'ing to mode (umask-proof) before returning. The
// caller has already confirmed backupPath does not exist, so an O_EXCL collision
// here is a race with another process and is returned as an error rather than
// silently overwriting the .lrc without a fresh backup.
func writeBackup(backupPath string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // backupPath derived from a caller-controlled path
	if err != nil {
		return fmt.Errorf("create backup %s: %w", backupPath, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync backup %s: %w", backupPath, err)
	}
	if err := os.Chmod(backupPath, mode); err != nil { //nolint:gosec // mode copied from the original file
		return fmt.Errorf("chmod backup %s: %w", backupPath, err)
	}
	return nil
}

// atomicWrite writes content over path via a same-directory temp file that is
// synced, chmod'd to mode, and renamed into place, so a partial write can never
// become the canonical file.
func atomicWrite(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil { //nolint:gosec // mode copied from the original file
		return fmt.Errorf("chmod temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	committed = true
	return nil
}
