package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lrcbackfill"
)

// runReconcileLRC walks the configured library roots and rewrites .lrc sidecars
// that stack multiple timestamps on one line into the expanded one-cue-per-line
// form, backing up each pristine original to <file>.lrc.orig. Dry-run by default;
// --yes applies and writes a JSONL record of every rewritten file. Files that are
// already clean are left untouched (needs-work gate), so the run is safely
// re-runnable.
func runReconcileLRC(ctx context.Context, out io.Writer, args ScanReconcileLRCCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	libRepo := library.New(sqlDB)
	var roots []string
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, libRepo, args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		roots = []string{lib.Path}
	} else {
		libs, lerr := libRepo.List(ctx)
		if lerr != nil {
			slog.Error("failed to list libraries", "error", lerr)
			return 1
		}
		for _, l := range libs {
			roots = append(roots, l.Path)
		}
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "reconcile-lrc: no library roots configured")
		return 0
	}

	backupPath := args.Backup
	if backupPath == "" {
		// Sub-second precision so two runs in the same wall-clock second cannot
		// share (and clobber) a default backup filename.
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-lrc-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	}
	// Lazy backup: the file is created only on the first record written, so a
	// zero-rewrite run never creates (and never has to delete) it -- a --yes run
	// against an all-clean library, or against an operator-named --backup path,
	// leaves the filesystem untouched. Appends rather than truncates.
	var lb *lazyBackup
	var backupW io.Writer
	if args.Yes {
		lb = &lazyBackup{path: backupPath}
		backupW = lb
		defer func() {
			if cerr := lb.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-lrc backup file", "path", backupPath, "error", cerr)
			}
		}()
	}

	summary, err := lrcbackfill.Run(ctx, lrcbackfill.Options{Roots: roots, Apply: args.Yes, Backup: backupW})
	if err != nil {
		slog.Error("reconcile-lrc failed", "error", err)
		return 1
	}

	verb := "would rewrite"
	if args.Yes {
		verb = "rewrote"
	}
	_, _ = fmt.Fprintf(out, "reconcile-lrc: %s %d stacked .lrc file(s) (%d scanned, %d already clean, %d skipped, %d blocked, %d errors)%s\n",
		verb, summary.Normalized, summary.Scanned, summary.Clean, summary.Skipped, summary.Blocked, summary.Errors, suffixDryRun(args.Yes))
	if summary.Blocked > 0 {
		// Blocked files are the only tally that demands follow-up, and the count
		// alone is useless without the paths -- which are in the WARN lines.
		_, _ = fmt.Fprintf(out, "%d file(s) still stacked but blocked by a pre-existing .orig; see the BLOCKED warnings above for paths\n", summary.Blocked)
	}
	if lb != nil && lb.opened() {
		_, _ = fmt.Fprintf(out, "backup records written to %s\n", backupPath)
	}
	if summary.Errors > 0 || summary.Blocked > 0 {
		// A partial reconciliation is not success: some files failed or were left
		// untouched, so callers/scripts must see a non-zero exit.
		//
		// Blocked counts here deliberately (#487). A blocked file is still stacked,
		// was left untouched, and its WARN says "operator action required" -- that is
		// the same partial reconciliation the errors rule already covers. Exiting 0
		// would tell a script "all clear" while telling a human "you must act", which
		// is precisely the benign/actionable conflation this issue removes, just at
		// the exit-code level. Before #487 blocked files were tallied as Skipped and
		// exited 0; promoting them to their own actionable tally without moving the
		// machine-readable signal would leave scripts blind to the one state that
		// demands attention.
		return 1
	}
	return 0
}

// lazyBackup is an io.Writer that opens (create-or-append) its file on the first
// Write, so a run that writes no records never creates the file.
type lazyBackup struct {
	path string
	f    *os.File
}

func (l *lazyBackup) Write(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: path is operator-supplied (--backup) or derived from the configured db dir
		if err != nil {
			return 0, fmt.Errorf("open reconcile-lrc backup %q: %w", l.path, err)
		}
		l.f = f
	}
	return l.f.Write(p)
}

func (l *lazyBackup) opened() bool { return l.f != nil }

// Sync flushes the backup file so writeBackupRecord can make each record durable
// before the .lrc it protects is rewritten.
func (l *lazyBackup) Sync() error {
	if l.f != nil {
		return l.f.Sync()
	}
	return nil
}

func (l *lazyBackup) Close() error {
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}

// lrcStackedCheckMarker names the one-shot serve-mode discovery pass in
// maintenance_markers (migration 027). Its presence means the pass has already
// run against this database and is skipped on subsequent startups. The name is
// effectively permanent: renaming it would re-run the walk on every existing
// deployment, so it is fixed at first commit.
const lrcStackedCheckMarker = "lrc_stacked_check_470"

// runStackedWalk is lrcbackfill.Run, indirected so a test can substitute a
// deterministic fake that blocks until its context is canceled. That lets a
// test exercise the mid-walk shutdown branch (errors.Is(err, context.Canceled))
// by canceling only once the fake signals the walk has genuinely started --
// i.e. strictly after the marker-check/library-list preamble above has already
// run -- rather than racing a goroutine sleep against that preamble, which is
// what let an earlier version of that test pass without ever reaching the walk.
var runStackedWalk = lrcbackfill.Run

// runLRCStackedCheck runs the stacked-.lrc discovery pass once per database in
// serve mode: if the marker is absent, it walks the configured library roots
// (dry-run only -- it never calls NormalizeFile and writes no files) and logs
// how many .lrc sidecars still carry compressed multi-timestamp lines, then
// stamps the marker so later startups skip the walk.
//
// This is deliberately a REPORT, not a backfill, despite #470 AC 2's wording.
// The `identityrepair` precedent this mirrors (runIdentityBackfill) mutates only
// canticle's own database rows; an unattended rewrite of the operator's .lrc
// files (plus a same-count .orig sidecar per file) is a different category of
// action and, at prod scale, is not something a container-image pull should
// trigger without a deliberate `--yes`. The CLI (`scan reconcile-lrc --yes`)
// remains the only thing that ever rewrites a file; this pass exists purely to
// tell an operator the problem exists. Also dropped from the AC: skipping
// `work_queue` rows that are `processing` -- that phrasing was inherited from
// identityrepair's DB-row-coupled shape (where a live worker is a real
// conflict); this pass is a pure filesystem walk with no DB coupling, and under
// report-only there is nothing to conflict with anyway.
//
// GOVERNING INVARIANT: "stamp always" means regardless of the COUNT found,
// never regardless of OUTCOME. The marker is a one-way stamp -- there is no
// unmark -- so a marker written on a run that did not actually complete a
// trustworthy walk over EVERY configured root silently disables the feature
// FOREVER on that deployment, with no error and no retry.
//
// REPORTING and STAMPING are deliberately split (issue #470 round 4, Critical
// 1). That split is the fix for a real, reproduced conflation: a marker-check
// failure, a library-list failure, a per-root walk failure, a shutdown, or an
// unvisited (absent/unmounted) root must all leave the marker unset so a
// later startup retries -- but a problem on ONE configured root must never
// suppress the REPORT of what OTHER, successfully-walked roots found.
// Reporting is advisory and safe on partial data; only STAMPING needs the
// all-roots-clean precondition. The bug this replaces: two roots, the first
// genuinely holding a stacked .lrc, the second an empty/unmounted stub walked
// after it -- the old code aggregated the first root's real finding into
// `total` and then, on reaching the second root's zero-entries case, returned
// immediately without ever logging that finding, so the operator was never
// told a stacked file existed, on top of the walk repeating every boot
// forever. The loop below never returns early on a per-root problem; it sets
// a `degraded` flag and CONTINUES, so every successfully-walked root still
// contributes to the report, and only the STAMP (never the report) is
// skipped when `degraded` ends up true. A degraded report is tagged
// `partial=true` so the operator can tell it apart from a complete one.
//
// The reverse failure mode is real too (#470 round 2): refusing to stamp too
// aggressively means a full library walk on every boot forever, which is its
// own defect. The MediaEntries check below (issue #470 round 4, Critical 2 --
// see lrcbackfill.Summary's doc comment) is scoped as narrowly as it can be
// while still meaning something -- "did this root have anything that looks
// like actual library content", not "did it have .lrc files" and not merely
// "did it have ANY entry at all" (a single stray sentinel file defeated that
// looser check) -- specifically so a legitimately clean, fully-populated
// library (audio present, no stacked lyrics yet) stamps normally instead of
// retrying forever, without a mount-checker's own sentinel file masquerading
// as "mounted".
func runLRCStackedCheck(ctx context.Context, sqlDB *sql.DB) {
	done, err := lrcStackedCheckDone(ctx, sqlDB)
	if err != nil {
		slog.Warn("lrc check: marker check failed; skipping this startup", "error", err)
		return
	}
	if done {
		return
	}

	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		slog.Warn("lrc check: failed to list libraries; skipping this startup", "error", err)
		return
	}
	if len(libs) == 0 {
		// Nothing configured to check yet; leave the marker unset so a later
		// startup (once a library exists) actually performs the check.
		return
	}
	roots := make([]string, 0, len(libs))
	for _, l := range libs {
		roots = append(roots, l.Path)
	}

	// Quiet: this is the unattended startup path, not the operator-invoked CLI.
	// The four per-file path-bearing logs lrcbackfill.Run would otherwise emit
	// (symlink skip, BLOCKED, per-file error, and the peer-expanded Debug) name
	// the sidecar's full path, which encodes <root>/<Artist>/<Album>/<Title>.lrc
	// -- private library metadata that must never land in an unattended server
	// log. The tallies in Summary (Skipped, Blocked, Errors) survive Quiet; only
	// the paths are suppressed.
	//
	// Walked one root at a time, rather than a single lrcbackfill.Run call
	// carrying every root, so "was this root actually there" (MediaEntries) can
	// be checked PER ROOT. A single call aggregates MediaEntries across all
	// roots, which cannot tell "root A had files, root B was an empty unmounted
	// stub" apart from "both roots together had some files" -- that aggregate
	// blindness is exactly what let a real library on one root paper over an
	// unmounted second root and stamp permanently (issue #470 round 2,
	// Critical 1). The CLI (`scan reconcile-lrc`) is unaffected and keeps
	// calling Run with every root in one pass; it reports per-file BLOCKED
	// paths already, so it does not share this hazard.
	var total lrcbackfill.Summary
	degraded := false
	for i, root := range roots {
		summary, walkErr := runStackedWalk(ctx, lrcbackfill.Options{Roots: []string{root}, Apply: false, Quiet: true})
		if walkErr != nil {
			if errors.Is(walkErr, context.Canceled) {
				// A shutdown is not "this root is unavailable" -- it is the
				// whole process going away, so there is nothing left to
				// finish reporting on; return immediately rather than
				// continuing the loop.
				slog.Info("lrc check: interrupted by shutdown; will resume on next startup")
				return
			}
			// The wrapped error carries this root's path (Run returns
			// fmt.Errorf("walk %s: %w", root, ...)); never log walkErr itself
			// here. classifyWalkError extracts a path-free cause (permission
			// denied / not found / the bare, path-free *fs.PathError.Err) so the
			// operator can tell EACCES from ENOENT from EIO -- with the
			// every-boot retry this is the line they will read most often.
			//
			// Continue rather than return (issue #470 round 4, Critical 1):
			// this root's problem must not suppress the report of what other,
			// successfully-walked roots already found. `degraded` gates the
			// STAMP below; the REPORT still runs over whatever total the other
			// roots contributed.
			slog.Warn("lrc check: failed to walk a configured library root; that root will be retried next startup",
				"roots_configured", len(roots), "root_index", i, "cause", classifyWalkError(walkErr))
			degraded = true
			// lrcbackfill.Run returns the Summary it had already accumulated
			// BEFORE WalkDir hit the error (per-file errors never abort the
			// walk, so a root can legitimately find a stacked file and THEN
			// fail on a later, unrelated unreadable subdirectory). Accumulate
			// it into total before continuing, same as the successful-walk
			// path below -- otherwise an earlier finding on this exact root is
			// silently discarded and the final report can wrongly claim
			// nothing stacked was found. `degraded` (already set above) still
			// gates the STAMP; this changes only what gets REPORTED.
			total.Visited += summary.Visited
			total.MediaEntries += summary.MediaEntries
			total.Scanned += summary.Scanned
			total.Normalized += summary.Normalized
			total.Clean += summary.Clean
			total.Skipped += summary.Skipped
			total.Blocked += summary.Blocked
			total.Errors += summary.Errors
			continue
		}

		// A configured root that is not yet mounted (e.g. the bind-mount-before-
		// NAS race at container start) walks successfully but has no media
		// content -- not even a stray file that resembles part of a library.
		// Scanned (a count of ONLY .lrc files) cannot tell that apart from a
		// mounted root that legitimately has no .lrc sidecars yet, which is the
		// normal shape of every fresh install before the first fetch;
		// MediaEntries (an audio file or a .lrc, per lrcbackfill.Summary's doc
		// comment) is the signal actually consulted here. It is a
		// necessary-not-sufficient heuristic, not proof of mounting -- a
		// single stray sentinel file (a mount-checker's own .mountcheck,
		// Syncthing's .stfolder, macOS's .DS_Store, a stray README) does NOT
		// count, which is what makes it meaningfully harder to trip by
		// accident than "any entry at all" (the pre-round-4 Visited check,
		// which that exact sentinel-file shape defeated); a fully reliable
		// mount check would need statfs/device-id comparison against the
		// parent, which is more machinery than this warrants.
		//
		// Absent-or-empty is the same epistemic state as "no libraries
		// configured" above -- nothing was actually checked -- so it gets
		// similar treatment: warn and mark the run degraded. Continue rather
		// than return (issue #470 round 4, Critical 1), for the same reason as
		// the walk-error branch above: this root's problem must not swallow a
		// real finding already collected from an earlier root in this loop.
		if summary.MediaEntries == 0 {
			slog.Warn("lrc check: a configured library root appears empty or not mounted yet; that root will be retried next startup",
				"roots_configured", len(roots), "root_index", i)
			degraded = true
			// Same shape as the walk-error branch above: accumulate whatever
			// this root's summary holds (near-empty by construction here,
			// since MediaEntries==0 -- but consistent, and harmless, to fold
			// in rather than special-case) before continuing.
			total.Visited += summary.Visited
			total.MediaEntries += summary.MediaEntries
			total.Scanned += summary.Scanned
			total.Normalized += summary.Normalized
			total.Clean += summary.Clean
			total.Skipped += summary.Skipped
			total.Blocked += summary.Blocked
			total.Errors += summary.Errors
			continue
		}

		total.Visited += summary.Visited
		total.MediaEntries += summary.MediaEntries
		total.Scanned += summary.Scanned
		total.Normalized += summary.Normalized
		total.Clean += summary.Clean
		total.Skipped += summary.Skipped
		total.Blocked += summary.Blocked
		total.Errors += summary.Errors
	}

	// Errors/Blocked/Skipped make this walk's tally untrustworthy as an
	// all-clear: an unreadable file might be stacked, StatusBlocked is
	// specifically the tally that means "still stacked, needs an operator"
	// (issue #487), and a Skipped file (e.g. a symlinked .lrc) was never judged
	// at all (issue #470 round 2, Important 2) -- a stacked file behind a
	// symlink would otherwise read as "library clean". None of these is the walk
	// itself failing (WalkDir returned nil), so this branches separately from
	// the walkErr != nil case above, but the governing invariant is the same:
	// never stamp on a degraded walk. Retrying costs one more startup walk;
	// wrongly stamping costs the operator this notification forever.
	if total.Errors > 0 || total.Blocked > 0 || total.Skipped > 0 {
		slog.Warn("lrc check: walk completed but some .lrc files could not be judged; will retry on next startup",
			"scanned", total.Scanned, "errors", total.Errors, "blocked", total.Blocked, "skipped", total.Skipped, "partial", degraded)
		return
	}

	switch {
	case total.Normalized > 0:
		// Reported regardless of `degraded` (issue #470 round 4, Critical 1):
		// a stacked file found on a root that WAS successfully walked must
		// reach the operator even if some other configured root could not be
		// checked this startup. "partial" tells the operator this tally may
		// be an undercount, not the whole library.
		msg := "lrc check: stacked .lrc file(s) found; these render incorrectly in simple players. Run `canticle scan reconcile-lrc --yes` to expand them."
		if !degraded {
			msg += " This check runs once."
		} else {
			msg += " One or more other configured roots were not available this startup, so this is a partial count; the check will run again next startup."
		}
		slog.Info(msg, "stacked", total.Normalized, "scanned", total.Scanned, "partial", degraded)
	case degraded:
		// Nothing stacked was found among the roots that COULD be checked,
		// but at least one configured root was unavailable this startup --
		// this is not a real all-clear (there is no such thing as "clean" on
		// partial data), so this branch is deliberately worded to avoid
		// claiming the library is clean.
		slog.Warn("lrc check: one or more configured library roots were not available this startup; no stacked .lrc files found among the roots checked; will retry the unavailable root(s) next startup",
			"scanned", total.Scanned, "partial", true)
		return
	default:
		slog.Info("lrc check: library clean, no stacked .lrc files found. This check runs once.", "scanned", total.Scanned)
	}

	if degraded {
		// GOVERNING INVARIANT: never stamp on a degraded walk. The report
		// above is complete for the roots that were checked, but the marker
		// is a one-way, whole-deployment stamp -- it must only be written
		// once EVERY configured root has actually been walked successfully.
		return
	}

	if err := markLRCStackedCheckDone(ctx, sqlDB); err != nil {
		slog.Warn("lrc check: completed but failed to record marker; it may re-run next startup", "error", err)
	}
}

// classifyWalkError reports a coarse, path-free cause for a failed root walk.
// The error lrcbackfill.Run returns wraps the failing root's path
// (fmt.Errorf("walk %s: %w", root, ...)), so it must never be logged directly;
// this extracts only the underlying syscall-level cause. *fs.PathError's own
// Err field carries no path (the path lives in the sibling Path field, which
// this deliberately never touches), so surfacing pathErr.Err is safe.
func classifyWalkError(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	default:
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return pathErr.Err.Error()
		}
		return "unknown"
	}
}

// lrcStackedCheckDone reports whether the one-shot discovery marker is present.
func lrcStackedCheckDone(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	var one int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM maintenance_markers WHERE name = ?`, lrcStackedCheckMarker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query maintenance marker %q: %w", lrcStackedCheckMarker, err)
	}
	return true, nil
}

// markLRCStackedCheckDone records the marker so the pass is skipped hereafter.
func markLRCStackedCheckDone(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_markers (name) VALUES (?)`, lrcStackedCheckMarker); err != nil {
		return fmt.Errorf("record maintenance marker %q: %w", lrcStackedCheckMarker, err)
	}
	return nil
}
