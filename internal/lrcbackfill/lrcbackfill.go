// Package lrcbackfill rewrites existing .lrc sidecars that carry compressed
// multi-timestamp lines into the expanded, one-cue-per-line form, backing up the
// pristine original alongside each rewrite. It is the backfill half of issue
// #470; the pure transform lives in internal/lrcnormalize.
package lrcbackfill

import (
	"context"
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
	"github.com/sydlexius/canticle/internal/scanner"
)

// Options configures a backfill run over one or more library roots.
type Options struct {
	Roots  []string  // directory trees to walk for *.lrc files
	Apply  bool      // false = dry run (report only, write nothing)
	Backup io.Writer // optional JSONL sink; one {path,backup} line per applied normalization

	// Quiet suppresses the four per-file path-bearing logs (symlink skip,
	// BLOCKED, per-file error, and the peer-expanded Debug) that otherwise name
	// the offending .lrc path. Not all four are WARN -- the peer-expanded site
	// logs at Debug -- so "path-bearing", not level, is what Quiet gates. A
	// sidecar path encodes <root>/<Artist>/<Album>/<Title>.lrc -- exactly the
	// private library metadata that must never reach a log an operator did not
	// ask for. The operator-invoked `scan reconcile-lrc` CLI leaves this false:
	// the path detail is its whole purpose, and an operator reading their own
	// CLI output is not a leak. The unattended, marker-gated serve-startup check
	// sets it true. Either way Summary's Scanned/Skipped/Blocked/Errors counts
	// are always populated, so a quiet caller still has the tallies -- just not
	// the paths.
	Quiet bool
}

// Summary tallies a backfill run.
type Summary struct {
	Visited int // every non-directory entry the walk saw, regardless of extension

	// MediaEntries counts only entries that look like actual library content: an
	// audio file (per scanner.IsAudioFile) or a .lrc sidecar. Unlike Visited, a
	// single stray file that is neither -- a mount-checker's .mountcheck
	// sentinel, Syncthing's .stfolder, macOS's .DS_Store, a stray README, or any
	// other one-off dotfile or metadata file -- does not move this counter. It
	// is a necessary-not-sufficient heuristic, not proof that a root is
	// genuinely mounted: a fully reliable check would need statfs/device-id
	// comparison against the parent, which is out of scope here. See the doc
	// comment on its zero-check in commands.runLRCStackedCheck for what this
	// proves and does not prove about a root being mounted.
	MediaEntries int

	Scanned    int // .lrc files examined (a subset of Visited and of MediaEntries)
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
//
// ctx is checked once per directory entry in the WalkDir callback, and that is
// the ONLY way to stop a walk in progress -- for every caller, including the
// CLI. There is no signal-handling escape hatch anywhere: cmd/mxlrcgo-svc wires
// its root context through signal.NotifyContext, so Ctrl-C CANCELS THE CONTEXT
// rather than killing the process outright, and a walk that ignored ctx would
// keep running until it finished the tree. (An earlier version of this comment
// claimed the CLI could rely on Ctrl-C to kill the process; that was wrong
// about the actual wiring.)
//
// The stakes differ by caller even though the mechanism does not. A CLI run
// that ignores cancellation merely feels unresponsive; a caller embedded inside
// a longer-lived process can block that process's shutdown for minutes on an
// unbounded walk over a large NAS library.
//
// A canceled context aborts the walk promptly via WalkDir's own
// early-termination contract (a non-nil, non-SkipDir/SkipAll callback error
// stops the walk and is returned by WalkDir itself).
func Run(ctx context.Context, opts Options) (Summary, error) {
	var s Summary
	for _, root := range opts.Roots {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if d.IsDir() {
				return nil
			}
			// Visited counts every non-directory entry the walk actually
			// reached, regardless of extension. It is NOT, by itself, reliable
			// evidence that a root is genuinely mounted: a single stray file
			// unrelated to the library -- a mount-checker's .mountcheck
			// sentinel, Syncthing's .stfolder, macOS's .DS_Store, a stray
			// README -- makes Visited nonzero long before any real library
			// content exists. MediaEntries (below) is the field actually
			// consulted for that purpose.
			name := d.Name()
			ext := filepath.Ext(name)
			s.Visited++
			if scanner.IsAudioFile(name) || strings.EqualFold(ext, ".lrc") {
				// A necessary-not-sufficient signal, not proof: it still
				// cannot rule out a mount landing an unrelated audio/.lrc
				// file by coincidence, and a fully reliable check would need
				// statfs/device-id comparison against the parent, which is
				// more machinery than this warrants. It is materially harder
				// to trip by accident than "any entry at all" because it
				// requires content that specifically resembles a music
				// library rather than any single stray file.
				s.MediaEntries++
			}
			if !strings.EqualFold(ext, ".lrc") {
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
				// Quiet suppresses this entirely rather than substituting a
				// path-scrubbed message: the wrapped error itself (e.g. an
				// os.PathError from lstat/read) carries the path in its own
				// text, so there is no safe partial log here -- only the
				// count, which the caller reports from Summary.Errors.
				if !opts.Quiet {
					slog.Warn("lrcbackfill: file failed; continuing", "path", path, "error", ferr)
				}
				return nil
			}
			switch res.Status {
			case StatusNormalized:
				s.Normalized++
			case StatusClean:
				s.Clean++
				// A StatusClean reached via classifyBackupExists' re-read (a
				// pre-existing .orig turned out to be a peer run's legitimate
				// backup of an already-expanded file) names both paths -- the
				// fourth path-bearing log site round 1 missed (issue #470
				// round 2, Important 3). Gated on Quiet exactly like the
				// Skipped/Blocked WARNs below.
				if res.ViaBackupCheck && !opts.Quiet {
					slog.Debug("lrcbackfill: .orig backup exists and the .lrc is already expanded; nothing to do",
						"path", path, "backup", res.Backup)
				}
			case StatusSkipped:
				s.Skipped++
				if !opts.Quiet {
					slog.Warn("lrcbackfill: skipping symlink", "path", path)
				}
			case StatusBlocked:
				s.Blocked++
				if !opts.Quiet {
					slog.Warn("lrcbackfill: BLOCKED -- .lrc is still stacked but a pre-existing .orig bars a verifiable rewrite; operator action required (compare the two, then remove or rename the .orig and re-run)",
						"path", path, "backup", path+".orig")
				}
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
	Backup string // path of the .lrc.orig backup; set for StatusNormalized, and for a StatusClean verdict reached via ViaBackupCheck

	// ViaBackupCheck is true when this Result came from classifyBackupExists'
	// re-read (a pre-existing .orig triggered the #487 gate), as opposed to the
	// plain "never was stacked" StatusClean returned directly by load +
	// NormalizeBody. Run()'s status switch uses it to decide whether the
	// path-bearing "already expanded by a peer run" Debug log applies -- that
	// log names a specific backup path and would be meaningless (and
	// path-leaking) noise for the common case of a file that was simply never
	// stacked. Logging lives in Run(), not here, so it can be gated on
	// opts.Quiet the same way the Skipped/Blocked WARNs already are (issue #470
	// round 2, Important 3: this was the fourth path-bearing log site still
	// unconditional after round 1's fix).
	ViaBackupCheck bool
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
		// The Debug log naming path/backupPath is emitted by Run()'s status
		// switch, not here, so it can be gated on opts.Quiet like every other
		// path-bearing log in this package (see Result.ViaBackupCheck).
		return Result{Status: StatusClean, Backup: backupPath, ViaBackupCheck: true}, nil
	}
	// The BLOCKED WARN (path-bearing, so Quiet-gated) is logged once by Run()'s
	// status switch, not here -- classifyBackupExists is reachable only through
	// NormalizeFile/inspect, both of which are reachable only through Run(), so
	// logging here too would double the WARN for every apply-mode blocked file.
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
// maxLRCFileSize bounds the read via io.LimitReader on the open handle, so the
// guard is enforced DURING the read rather than by a prior os.Lstat size check.
// A stat-then-read pair each resolves path independently, so a file that grows
// or is replaced between the two is a TOCTOU (time-of-check to time-of-use)
// race a prior-check guard cannot close; reading through a bounded reader on
// one already-open handle closes it by construction. A real .lrc sidecar is a
// few KB of text; this pass may run in-process inside a longer-lived server, so
// an implausibly large ".lrc" -- corrupt, wrongly named, or hostile -- must not
// be read wholesale and risk taking that process down. 16 MiB is generous
// relative to any real lyric file while still bounding worst case memory to
// something a server can absorb without incident.
const maxLRCFileSize = 16 * 1024 * 1024

func load(path string) (body []byte, mode os.FileMode, skip bool, err error) {
	fi, err := os.Lstat(path) // Lstat (not Stat) so a symlink is detected, not followed.
	if err != nil {
		return nil, 0, false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// Logging (path-bearing, so Quiet-gated) is the Run() caller's job, not
		// this helper's: load is also invoked from classifyBackupExists' re-read,
		// where a symlink here is a defensive, not live, case (see that function's
		// doc comment) and would otherwise double-log.
		return nil, 0, true, nil
	}

	f, err := os.Open(path) //nolint:gosec // reason: path is caller-controlled library enumeration
	if err != nil {
		return nil, 0, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// mode comes from the already-open handle, not the earlier Lstat, so a
	// concurrent permission change between the two calls cannot leave the
	// caller preserving a mode that no longer matches the bytes it read.
	openFi, statErr := f.Stat()
	if statErr != nil {
		return nil, 0, false, fmt.Errorf("stat %s: %w", path, statErr)
	}

	// Read at most maxLRCFileSize+1 bytes: if the file is larger, this reads one
	// byte past the guard and the length check below catches it -- the bound is
	// enforced by the reader itself, not by a size observed before the read.
	raw, err := io.ReadAll(io.LimitReader(f, maxLRCFileSize+1))
	if err != nil {
		return nil, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(raw)) > maxLRCFileSize {
		return nil, 0, false, fmt.Errorf("%s: read more than %d bytes, exceeds the .lrc size guard", path, maxLRCFileSize)
	}
	return raw, openFi.Mode().Perm(), false, nil
}

// writeBackup preserves content to backupPath with exclusive-create semantics,
// fsyncing the data and chmod'ing to mode (umask-proof) before returning. The
// caller has already confirmed backupPath does not exist, so an O_EXCL collision
// here is a race with another process and is returned as an error rather than
// silently overwriting the .lrc without a fresh backup.
func writeBackup(backupPath string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // reason: backupPath derived from a caller-controlled path
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
	if err := os.Chmod(backupPath, mode); err != nil { //nolint:gosec // reason: mode copied from the original file
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
	if err := os.Chmod(tmpPath, mode); err != nil { //nolint:gosec // reason: mode copied from the original file
		return fmt.Errorf("chmod temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	committed = true
	return nil
}
