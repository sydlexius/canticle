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

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/lyrics"
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
	Clean      int // already expanded; nothing to do
	Skipped    int // symlinks and other benign, non-actionable skips
	Blocked    int // still stacked but a pre-existing .orig bars a safe rewrite
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
				// report writes the undo record before the rewrite; NormalizeFile
				// aborts (and rolls back the backup) if it fails.
				var report func(string) error
				if opts.Backup != nil {
					report = func(backupPath string) error {
						return writeBackupRecord(opts.Backup, path, backupPath)
					}
				}
				res, ferr = NormalizeFile(path, report)
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
			case StatusClean:
				s.Clean++
			case StatusSkipped:
				s.Skipped++
			case StatusBlocked:
				s.Blocked++
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
	if _, err := w.Write(append(line, '\n')); err != nil {
		return err
	}
	// Flush the record to stable storage before the caller replaces the .lrc, so
	// the undo trail is durable ahead of the mutation it protects.
	if s, ok := w.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
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
	// StatusBlocked means the file still needs work but a pre-existing .orig
	// prevents a verifiable rewrite. Unlike StatusSkipped this always warrants
	// operator attention, and is counted separately so a zero tally is a real
	// all-clear (issue #487).
	StatusBlocked
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
//
// report, if non-nil, is invoked once with the backup path after the .orig is
// durably written but BEFORE the .lrc is replaced, so a failure to record the
// undo trail aborts the rewrite (the file is left untouched and the just-created
// backup rolled back) rather than leaving a rewritten file absent from the trail.
func NormalizeFile(path string, report func(backupPath string) error) (Result, error) {
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
	// overwriting would leave the current bytes backed up nowhere. Decline
	// instead (conservative-on-uncertainty). In normal single-tool operation this
	// is unreachable -- after one expand the file is already clean and never
	// rewritten -- so a .orig here means a concurrent run or external
	// interference. classifyBackupExists re-reads to tell those apart.
	backupPath := path + ".orig"
	if _, statErr := os.Lstat(backupPath); statErr == nil {
		return classify(path, backupPath)
	} else if !os.IsNotExist(statErr) {
		return Result{}, fmt.Errorf("stat backup %s: %w", backupPath, statErr)
	}

	dir := filepath.Dir(path)
	// Backup-first: preserve the pristine original before touching the .lrc, then
	// fsync the directory so the .orig's entry is durable BEFORE the rename that
	// replaces the .lrc (a crash must never leave the rewrite without its backup).
	if err := writeBackup(backupPath, raw, origMode); err != nil {
		return Result{}, err
	}
	lyrics.FsyncDir(dir)

	// Record the undo trail while the backup exists but before the rewrite. If
	// recording fails, roll the backup back and abort so the .lrc is untouched
	// and no rewritten file is ever missing from the trail.
	if report != nil {
		if rerr := report(backupPath); rerr != nil {
			_ = os.Remove(backupPath)
			return Result{}, fmt.Errorf("record backup for %s: %w", path, rerr)
		}
	}

	// Atomic rewrite of the expanded body, then fsync the rename durable.
	if err := atomicWrite(path, []byte(out), origMode); err != nil {
		return Result{}, err
	}
	lyrics.FsyncDir(dir)
	return Result{Status: StatusNormalized, Backup: backupPath}, nil
}

// classifyBackupExists decides what a pre-existing .orig means for path, and is
// the fix for issue #487. The caller's needs-work verdict was computed from bytes
// read BEFORE the backup was observed, so a concurrent run may have expanded the
// file in between; those stale bytes cannot distinguish the two states. Re-read
// the current file and re-apply the gate:
//
//   - no longer stacked -> a peer run finished the rewrite and the .orig is its
//     legitimate backup. Benign, no work remains, and emphatically not a warning:
//     an operator who "cleared the blockage" here would delete a real backup.
//   - still stacked -> the .orig genuinely bars the rewrite. Warn; needs an operator.
//
// classify is classifyBackupExists, indirected so a test can pin that the
// .orig-exists paths actually ROUTE here rather than deciding from bytes they
// already hold.
//
// That routing is the entire fix for #487 and it cannot be reached from a
// fixture: NormalizeFile only consults the .orig gate when the file was stacked
// at load time, so the benign case (a peer expanded it in between) requires the
// file to change between load() and the Lstat. Without this seam a regression
// that classified from the stale bytes -- the exact pre-fix bug -- passes the
// whole suite, because testing classifyBackupExists directly can never
// distinguish a re-read from a stale read: on disk they are the same bytes.
var classify = classifyBackupExists

func classifyBackupExists(path, backupPath string) (Result, error) {
	cur, _, skip, err := load(path)
	if err != nil {
		// The re-read failed where the first read succeeded (the file vanished or
		// became unreadable mid-run). The row is counted as an error, NOT as
		// Blocked -- which is why a zero blocked tally is only an all-clear
		// alongside zero errors.
		return Result{}, err
	}
	if skip {
		// Defensive, not live: both callers already returned on skip before
		// consulting the .orig gate, so this only fires if the path became a symlink
		// between their load and this re-read. Kept rather than deleted because it is
		// the correct answer if a future caller reaches here without pre-checking.
		return Result{Status: StatusSkipped}, nil
	}
	if _, stacked := lrcnormalize.NormalizeBody(string(cur)); !stacked {
		slog.Debug("lrcbackfill: .orig backup exists and the .lrc is already expanded; nothing to do",
			"path", path, "backup", backupPath)
		return Result{Status: StatusClean}, nil
	}
	slog.Warn("lrcbackfill: BLOCKED -- .lrc is still stacked but a pre-existing .orig bars a verifiable rewrite; operator action required (compare the two, then remove or rename the .orig and re-run)",
		"path", path, "backup", backupPath)
	return Result{Status: StatusBlocked}, nil
}

// inspect reports what NormalizeFile would do, without writing anything (dry run).
// It mirrors NormalizeFile's .orig gate so a dry run never promises a rewrite that
// apply would decline -- the count is the only output some callers have (#470 AC2).
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
	backupPath := path + ".orig"
	if _, statErr := os.Lstat(backupPath); statErr == nil {
		return classify(path, backupPath)
	} else if !os.IsNotExist(statErr) {
		return Result{}, fmt.Errorf("stat backup %s: %w", backupPath, statErr)
	}
	return Result{Status: StatusNormalized, Backup: backupPath}, nil
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
	// Remove a partially-written backup on any failure, so a later run does not
	// mistake a truncated/incomplete .orig for a valid one and skip the source.
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(backupPath)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync backup %s: %w", backupPath, err)
	}
	if err := os.Chmod(backupPath, mode); err != nil { //nolint:gosec // mode copied from the original file
		return fmt.Errorf("chmod backup %s: %w", backupPath, err)
	}
	committed = true
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
