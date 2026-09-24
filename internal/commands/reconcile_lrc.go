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
	"github.com/sydlexius/canticle/internal/reports"
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
	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, args.Yes)
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

	// Record this pass for the web dashboard summary (#929), only when it
	// actually applied: --yes is the only mode that rewrites a file, so a
	// dry run recording itself here would let a projection be misread as a
	// count of files actually changed on disk. Best-effort and non-fatal --
	// the rewrite itself already succeeded, and a stale/missing dashboard
	// summary is far cheaper than failing a completed reconciliation over it.
	if args.Yes {
		if merr := markLRCNormalizeApply(ctx, sqlDB, summary.Normalized); merr != nil {
			slog.Warn("reconcile-lrc: failed to record normalization summary marker; dashboard summary may be stale", "error", merr)
		}
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

// maxDegradedAttempts bounds how many consecutive startups may return a
// degraded walk before this check gives up and stamps the marker anyway
// (#922). Without a ceiling, a single permanently-unreadable file (bad
// permissions, corruption, the 16MB size guard) or a stale/decommissioned
// library root disables the feature's terminal state forever: the marker
// never stamps, the full walk re-runs on every boot in perpetuity, and the
// operator never receives the one notification this check exists to deliver.
// 5 gives a genuinely transient condition (a NAS not yet mounted at container
// start, a brief permission hiccup) several restarts to clear on its own,
// while bounding the cost of a permanent one to a handful of extra walks
// rather than forever. A const, not config -- see the `timing` package's doc
// comment for why a threshold like this belongs fixed in code.
const maxDegradedAttempts = 5

// lrcStackedCheckDegradedAttemptsMarker durably counts consecutive degraded
// startup attempts (#922). It reuses maintenance_markers' existing
// detail_count column (migration 048) under a SEPARATE marker name, rather
// than adding a new column or table -- exactly the reuse that migration's own
// doc comment anticipated ("a future marker that does have one reuses this
// column"). It must be a different row than lrcStackedCheckMarker: that
// marker's mere presence means "done" (migration 027's invariant), so writing
// an in-progress attempt count under that same name would make
// lrcStackedCheckDone report done=true before the check has actually
// completed.
const lrcStackedCheckDegradedAttemptsMarker = "lrc_stacked_check_470_degraded_attempts"

// lrcStackedCheckDegradedStackedMarker keeps the largest stacked-file count any
// degraded attempt has seen (#922). Each startup rebuilds its tally from
// scratch, so without it a finding from an earlier degraded boot would be lost
// if the final, give-up boot happened not to see it (a root unmounted that
// time, say), and the stamp then stops the check for good.
const lrcStackedCheckDegradedStackedMarker = "lrc_stacked_check_470_degraded_stacked"

// lrcStackedCheckGiveUpDetail is the sentinel written to lrcStackedCheckMarker's
// detail_count when the marker is stamped only because maxDegradedAttempts was
// exhausted, as opposed to a genuine completion (which records a non-negative
// count of stacked .lrc files found). Every real count is >= 0, so this value
// is unambiguous to any future reader of the row: it can always tell "checked
// and clean/found N" apart from "gave up while degraded" -- the marker must
// never silently read as an all-clear for a walk that never actually finished.
const lrcStackedCheckGiveUpDetail = -1

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
	// unavailableRootIDs names, by library id only (never path or name), every
	// configured root this attempt could not visit at all (a walk error or an
	// empty/unmounted root). Populated only for those two cases -- never for a
	// root that walked fine but whose files individually errored/blocked/were
	// skipped -- so the eventual give-up message (#922) can tell an operator
	// "these specific roots are the problem" without leaking anything private.
	var unavailableRootIDs []int64
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
			slog.Warn("lrc check: failed to walk a configured library root; that root was not checked this startup",
				"roots_configured", len(roots), "root_index", i, "library_id", libs[i].ID, "cause", classifyWalkError(walkErr))
			degraded = true
			unavailableRootIDs = append(unavailableRootIDs, libs[i].ID)
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
			slog.Warn("lrc check: a configured library root appears empty or not mounted yet; that root was not checked this startup",
				"roots_configured", len(roots), "root_index", i, "library_id", libs[i].ID)
			degraded = true
			unavailableRootIDs = append(unavailableRootIDs, libs[i].ID)
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
	// fileDegraded mirrors the walk-error/empty-root `degraded` flag above, but
	// for problems discovered WITHIN an otherwise-successful walk: a file that
	// individually errored, was blocked by a pre-existing .orig, or was
	// skipped (unjudged) entirely. Named separately from `degraded` because
	// the two are logged differently below, but both feed the same
	// bounded-retry decision (#922) afterward.
	fileDegraded := total.Errors > 0 || total.Blocked > 0 || total.Skipped > 0

	// GOVERNING INVARIANT (unchanged from #470): never stamp on a degraded
	// walk as if it were clean. What #922 adds is a bounded CEILING on how
	// many consecutive degraded attempts get an unconditional retry -- past
	// that ceiling the marker is still stamped, but honestly, recording that
	// the check gave up rather than that it found a clean library.
	//
	// The ceiling is decided HERE, before the per-attempt report below, not
	// after it (#922 fix round, M1): reporting "will retry next startup" and
	// then immediately following it with "gave up ... will not run again"
	// reads as an outright contradiction on the very same attempt. Deciding
	// first lets the report below drop the retry wording -- and, on the
	// walk-completed-with-file-problems / roots-unavailable branches, skip
	// its own line entirely -- when this IS the attempt that gives up, since
	// the give-up line further down already carries every count those lines
	// would have reported.
	attemptDegraded := degraded || fileDegraded
	var (
		attempts       int
		incErr         error
		isFinalAttempt bool
		// bestStacked is the largest stacked count across this degraded
		// streak, this walk included; the give-up report uses it.
		bestStacked = total.Normalized
	)
	if attemptDegraded {
		attempts, incErr = incrementDegradedAttempts(ctx, sqlDB)
		// incErr is handled after the report below (never here): the report
		// describes what THIS walk found, which is knowable regardless of
		// whether the counter could be persisted. On incErr != nil,
		// isFinalAttempt stays false -- fail open on the ceiling decision the
		// same way the rest of this function fails open on infrastructure
		// errors, rather than guessing.
		if incErr == nil && attempts >= maxDegradedAttempts {
			isFinalAttempt = true
		}
		if incErr == nil {
			best, merr := recordDegradedStacked(ctx, sqlDB, total.Normalized)
			if merr != nil {
				slog.Warn("lrc check: failed to record the degraded stacked count; the give-up report may undercount", "error", merr)
			} else {
				bestStacked = best
			}
		}
	}

	if fileDegraded {
		// Errors/Blocked/Skipped make this walk's tally untrustworthy as an
		// all-clear: an unreadable file might be stacked, StatusBlocked is
		// specifically the tally that means "still stacked, needs an operator"
		// (issue #487), and a Skipped file (e.g. a symlinked .lrc) was never judged
		// at all (issue #470 round 2, Important 2) -- a stacked file behind a
		// symlink would otherwise read as "library clean". None of these is the walk
		// itself failing (WalkDir returned nil), so this branches separately from
		// the walkErr != nil case above.
		// total.Normalized is reported here too (#922 fix round, C1): a
		// file-level degradation (a bad/blocked/skipped file among otherwise-
		// successfully-walked ones) must not silently swallow a stacked-file
		// finding that the SAME walk already collected on this or an earlier
		// root. Without this, a single permanently-unreadable file sitting
		// alongside hundreds of legitimately stacked ones would age the
		// stacked count out of every log line once the ceiling is reached,
		// defeating the #470 invariant this whole check exists to serve.
		//
		// Suppressed on the final attempt (M1 above): the give-up line below
		// already carries scanned/stacked/errors/blocked/skipped, so this
		// would only add a "will retry" claim the give-up line then refutes.
		if !isFinalAttempt {
			slog.Warn("lrc check: walk completed but some .lrc files could not be judged; will retry on next startup",
				"scanned", total.Scanned, "stacked", total.Normalized, "errors", total.Errors, "blocked", total.Blocked, "skipped", total.Skipped, "partial", degraded)
		}
	} else {
		switch {
		case total.Normalized > 0:
			// Reported regardless of `degraded` (issue #470 round 4, Critical 1):
			// a stacked file found on a root that WAS successfully walked must
			// reach the operator even if some other configured root could not be
			// checked this startup. "partial" tells the operator this tally may
			// be an undercount, not the whole library.
			msg := "lrc check: stacked .lrc file(s) found; these render incorrectly in simple players. Run `canticle scan reconcile-lrc --yes` to expand them."
			switch {
			case isFinalAttempt:
				// The give-up line below is this deployment's terminal
				// statement about this check; do not also claim it will run
				// again next startup.
				msg += " One or more other configured roots were not available this startup, so this is a partial count."
			case !degraded:
				msg += " This check runs once."
			default:
				msg += " One or more other configured roots were not available this startup, so this is a partial count; the check will run again next startup."
			}
			slog.Info(msg, "stacked", total.Normalized, "scanned", total.Scanned, "partial", degraded)
		case degraded:
			// Nothing stacked was found among the roots that COULD be checked,
			// but at least one configured root was unavailable this startup --
			// this is not a real all-clear (there is no such thing as "clean" on
			// partial data), so this branch is deliberately worded to avoid
			// claiming the library is clean.
			//
			// Suppressed on the final attempt (M1 above), same reasoning as
			// the fileDegraded branch: the give-up line is the terminal
			// statement and already names the unavailable roots.
			if !isFinalAttempt {
				slog.Warn("lrc check: one or more configured library roots were not available this startup; no stacked .lrc files found among the roots checked; will retry the unavailable root(s) next startup",
					"scanned", total.Scanned, "partial", true)
			}
		default:
			slog.Info("lrc check: library clean, no stacked .lrc files found. This check runs once.", "scanned", total.Scanned)
		}
	}

	if !attemptDegraded {
		// A genuinely clean/completed attempt resets the counter: a deployment
		// that recovers from a transient blip (a root remounts, a permission
		// fix lands) must not carry a stale degraded-attempt count into some
		// unrelated future degradation.
		if cerr := clearDegradedAttempts(ctx, sqlDB); cerr != nil {
			slog.Warn("lrc check: completed but failed to clear the degraded-attempt counter", "error", cerr)
		}
		if err := markLRCStackedCheckDone(ctx, sqlDB); err != nil {
			slog.Warn("lrc check: completed but failed to record marker; it may re-run next startup", "error", err)
		}
		return
	}

	if incErr != nil {
		// The counter itself is unavailable; fail open the same way the
		// marker-check/library-list failures above do -- retry next startup,
		// without a ceiling decision this time, rather than either stamping
		// on unreliable state or panicking.
		slog.Warn("lrc check: failed to record the degraded-attempt count; will retry next startup", "error", incErr)
		return
	}
	if !isFinalAttempt {
		// Still within the bounded-retry budget: behave exactly as #470 did
		// before this ceiling existed, and leave the marker unset.
		return
	}

	// Ceiling reached (#922): stop retrying, but the stamp records a GAVE-UP
	// outcome, never a clean one. Counts only, no path/artist/title -- an
	// unavailable root is identified by its library id, which is safe to log
	// (the operator already has that id in their own library configuration).
	//
	// bestStacked is the largest stacked count any attempt in this degraded
	// streak saw, not just this walk's (#922): the give-up stamp is the LAST
	// thing this check will ever log on this deployment, so if it drops a
	// finding an earlier boot made, the operator never learns about it --
	// exactly the #470 failure mode this check exists to prevent. When there is a
	// count to report, the message also names the exact remediation command
	// (rather than a bare "investigate manually"), since the operator has
	// nothing else pointing them at it once this line has scrolled past.
	giveUpMsg := "lrc check: gave up after repeated degraded startup attempts; this check will not run again on this deployment. " +
		"If a library root above was reported unavailable, fix or remove it (see its library_id in the earlier warning)."
	if bestStacked > 0 {
		giveUpMsg += fmt.Sprintf(" %d stacked .lrc file(s) were found (a partial count, since this attempt gave up rather than"+
			" completing cleanly); run `canticle scan reconcile-lrc --yes` to expand them.", bestStacked)
	} else {
		giveUpMsg += " A persistent file-level problem can be investigated by running `canticle scan reconcile-lrc` manually."
	}
	slog.Warn(giveUpMsg,
		"degraded_attempts", attempts, "max_degraded_attempts", maxDegradedAttempts,
		"stacked", bestStacked,
		"errors", total.Errors, "blocked", total.Blocked, "skipped", total.Skipped,
		"unavailable_root_count", len(unavailableRootIDs), "unavailable_root_ids", unavailableRootIDs)

	if err := markLRCStackedCheckGaveUp(ctx, sqlDB); err != nil {
		slog.Warn("lrc check: gave up but failed to record the marker; will retry next startup", "error", err)
		return
	}
	if cerr := clearDegradedAttempts(ctx, sqlDB); cerr != nil {
		slog.Warn("lrc check: gave-up marker recorded but failed to clear the degraded-attempt counter", "error", cerr)
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

// markLRCStackedCheckGaveUp stamps lrcStackedCheckMarker the same way
// markLRCStackedCheckDone does (so the gate above skips every later startup),
// but additionally records lrcStackedCheckGiveUpDetail in detail_count (#922)
// so the row is honestly distinguishable from a genuine clean completion,
// which never writes a detail_count at all. INSERT OR IGNORE would silently
// no-op if the marker somehow already existed (it cannot on the code path
// that calls this -- lrcStackedCheckDone already gated entry -- but the
// ON CONFLICT keeps this correct even so, matching markLRCNormalizeApply's
// UPSERT shape).
func markLRCStackedCheckGaveUp(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name, detail_count)
         VALUES (?, ?)
         ON CONFLICT(name) DO UPDATE SET completed_at = excluded.completed_at, detail_count = excluded.detail_count`,
		lrcStackedCheckMarker, lrcStackedCheckGiveUpDetail); err != nil {
		return fmt.Errorf("record maintenance marker %q as gave-up: %w", lrcStackedCheckMarker, err)
	}
	return nil
}

// incrementDegradedAttempts records one more consecutive degraded startup
// attempt (#922) and returns the new total. It reuses maintenance_markers
// (migration 027) under lrcStackedCheckDegradedAttemptsMarker -- a distinct
// row from lrcStackedCheckMarker, whose mere PRESENCE means "done" -- so an
// in-progress attempt count never gets read as a completed check.
func incrementDegradedAttempts(ctx context.Context, sqlDB *sql.DB) (int, error) {
	var attempts int
	err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO maintenance_markers (name, detail_count)
         VALUES (?, 1)
         ON CONFLICT(name) DO UPDATE SET
             completed_at = excluded.completed_at,
             detail_count = COALESCE(maintenance_markers.detail_count, 0) + 1
         RETURNING detail_count`,
		lrcStackedCheckDegradedAttemptsMarker).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("increment degraded-attempt counter %q: %w", lrcStackedCheckDegradedAttemptsMarker, err)
	}
	return attempts, nil
}

// recordDegradedStacked keeps the largest stacked count seen across the
// current degraded streak (#922) and returns it, this walk's count included.
func recordDegradedStacked(ctx context.Context, sqlDB *sql.DB, stacked int) (int, error) {
	var best int
	err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO maintenance_markers (name, detail_count)
         VALUES (?, ?)
         ON CONFLICT(name) DO UPDATE SET
             completed_at = excluded.completed_at,
             detail_count = MAX(COALESCE(maintenance_markers.detail_count, 0), excluded.detail_count)
         RETURNING detail_count`,
		lrcStackedCheckDegradedStackedMarker, stacked).Scan(&best)
	if err != nil {
		return stacked, fmt.Errorf("record degraded stacked count %q: %w", lrcStackedCheckDegradedStackedMarker, err)
	}
	return best, nil
}

// clearDegradedAttempts removes the degraded-attempt counter row and the
// degraded stacked-count row, so a later
// degradation (after a genuine clean/completed run in between) starts counting
// from zero rather than compounding onto a stale prior streak. A no-op (no
// error) when the row is already absent -- the common case, since most
// deployments never degrade at all.
func clearDegradedAttempts(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx,
		`DELETE FROM maintenance_markers WHERE name IN (?, ?)`,
		lrcStackedCheckDegradedAttemptsMarker, lrcStackedCheckDegradedStackedMarker); err != nil {
		return fmt.Errorf("clear degraded-attempt counter %q: %w", lrcStackedCheckDegradedAttemptsMarker, err)
	}
	return nil
}

// markLRCNormalizeApply records the most recent `scan reconcile-lrc --yes`
// apply pass for the web dashboard summary (#929): how many stacked .lrc
// sidecars it rewrote, and when. It is the sole writer of
// reports.MaintenanceMarkerLRCNormalize; the read side lives in
// internal/reports so the dashboard queries it the same way every other
// dashboard figure is sourced (#929 design decision 1 -- see that package's
// LastLRCNormalization doc comment for why this reuses maintenance_markers
// rather than a new table).
//
// UNLIKE lrcStackedCheckMarker/markLRCStackedCheckDone above, this is NOT a
// run-once gate: `scan reconcile-lrc` is safely re-runnable (the backfill's
// own needs-work gate makes a second pass over an already-clean file a
// no-op), and each apply should overwrite the summary with its own fresh
// count and timestamp rather than being silently ignored by an INSERT OR
// IGNORE after the first ever run. So this UPSERTs.
func markLRCNormalizeApply(ctx context.Context, sqlDB *sql.DB, normalized int) error {
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name, completed_at, detail_count)
         VALUES (?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), ?)
         ON CONFLICT(name) DO UPDATE SET completed_at = excluded.completed_at, detail_count = excluded.detail_count`,
		reports.MaintenanceMarkerLRCNormalize, normalized); err != nil {
		return fmt.Errorf("record maintenance marker %q: %w", reports.MaintenanceMarkerLRCNormalize, err)
	}
	return nil
}
