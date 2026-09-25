package commands

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
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
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// lyricsInjectEditorTag is lyrics.InjectEditorTag, indirected so a test can
// substitute a race (ErrChangedDuringRewrite) without needing to reach
// package lyrics' own unexported injectEditorTagPreRenameHook seam (which is
// not reachable from this package).
var lyricsInjectEditorTag = lyrics.InjectEditorTag

// injectEditorTag wraps lyricsInjectEditorTag: ErrChangedDuringRewrite (a
// concurrent writer won this file, #483 finding 3) is reported via
// changed=true rather than folded into stamped/err. A caller MUST treat
// changed as "never stamped, retry next pass" (Copilot 4101172948) -- an
// earlier version of this wrapper collapsed it to (false, nil), which is
// indistinguishable from "nothing to do" and let the one-shot startup pass
// mark itself done even though this file was never actually tagged.
func injectEditorTag(path string) (stamped, changed bool, err error) {
	stamped, err = lyricsInjectEditorTag(path)
	if errors.Is(err, lyrics.ErrChangedDuringRewrite) {
		return false, true, nil
	}
	return stamped, false, err
}

// editorTagBackupRecord is one JSONL line per stamped file. Original holds
// the file's exact pre-mutation bytes (base64), so the record is restorable,
// not just a "this path was touched" audit trail (CodeRabbit 4101188299,
// Copilot 4101172972): InjectEditorTag's rewrite is a single additive header
// line, but without the original bytes an operator recovering from a bad
// rewrite (or a race InjectEditorTag's best-effort guard missed) has nothing
// to restore from.
type editorTagBackupRecord struct {
	FilePath string `json:"file_path"`
	Original string `json:"original"`
}

// appendEditorTagBackup writes and fsyncs one JSONL record BEFORE the caller
// mutates path, so the record is durable ahead of the rewrite it protects
// (write-ahead, matching every other reconcile-family command).
func appendEditorTagBackup(f *os.File, path string, original []byte) error {
	b, err := json.Marshal(editorTagBackupRecord{
		FilePath: path,
		Original: base64.StdEncoding.EncodeToString(original),
	})
	if err != nil {
		return fmt.Errorf("marshal reconcile-editor-tag backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-editor-tag backup record: %w", err)
	}
	return f.Sync()
}

// editorTagWalkResult tallies one walkEditorTagRoot call.
type editorTagWalkResult struct {
	Scanned int // .lrc/.elrc files examined
	Stamped int // eligible files actually tagged (or would be, in dry run)
	// Changed counts a file fn reported as raced (Copilot 4101172948): the
	// rewrite lost a concurrency race with another writer, so it was never
	// stamped. Kept separate from Errored (not a failure the operator needs
	// to act on) and from Stamped (it was NOT tagged), so a caller deciding
	// whether a pass "cleanly covered every file" can tell a genuine race
	// apart from a clean stamp.
	Changed  int
	Errored  int
	Degraded int
	// Media counts entries that look like actual library content -- an audio
	// file (scanner.IsAudioFile) or a lyric sidecar of EITHER active kind
	// (sidecar.KindLineSynced or sidecar.KindWordSynced, i.e. .lrc or .elrc).
	// Computed inline during this same walk (Copilot 4101172998, CodeRabbit
	// 4101188315): an earlier version derived this signal from a SEPARATE
	// lrcbackfill.Run dry-run preflight walk, which both doubled traversal
	// and undercounted, since lrcbackfill's own MediaEntries only recognizes
	// audio files and ".lrc" -- never ".elrc" -- so a root holding only
	// eligible .elrc sidecars read as having zero media and was skipped
	// entirely (never even reaching this walk).
	Media int
}

// walkEditorTagRoot walks root for .lrc/.elrc files; fn owns each file's
// decision/mutation. A non-root directory error does NOT abort (#483
// finding 2): it counts in Degraded and fs.SkipDir resumes at siblings.
func walkEditorTagRoot(ctx context.Context, root string, fn func(path string) (stamped, changed bool, err error)) (editorTagWalkResult, error) {
	var res editorTagWalkResult
	_, canonRoot := pathutil.CanonicalRoot(root)
	if _, statErr := os.Stat(canonRoot); statErr != nil {
		return res, fmt.Errorf("walk %s: %w", root, statErr)
	}
	walkErr := filepath.WalkDir(canonRoot, func(path string, d fs.DirEntry, walkFileErr error) error {
		if walkFileErr != nil {
			if path == canonRoot {
				return walkFileErr
			}
			res.Degraded++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		k := sidecar.KindOf(name)
		isSidecar := k == sidecar.KindLineSynced || k == sidecar.KindWordSynced
		if scanner.IsAudioFile(name) || isSidecar {
			res.Media++
		}
		if !isSidecar {
			return nil
		}
		res.Scanned++
		ok, changed, ferr := fn(path)
		if ferr != nil {
			res.Errored++
			return nil
		}
		if changed {
			res.Changed++
		}
		if ok {
			res.Stamped++
		}
		return nil
	})
	if walkErr != nil {
		return res, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return res, nil
}

// runReconcileEditorTag backfills [re:canticle] onto canticle-written
// .lrc/.elrc sidecars (#483). Dry-run by default; --yes applies and backs
// up. Stdout is aggregate-only, matching `revalidate`'s privacy convention.
func runReconcileEditorTag(ctx context.Context, out io.Writer, args ScanReconcileEditorTagCmd) int {
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
		_, _ = fmt.Fprintln(out, "reconcile-editor-tag: no library roots configured")
		return 0
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path),
			fmt.Sprintf("reconcile-editor-tag-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-editor-tag backup file", "path", backupPath, "error", cerr)
			}
		}
	}()

	perFile := func(path string) (bool, bool, error) {
		if !args.Yes {
			eligible, eerr := lyrics.EditorTagEligible(path)
			return eligible, false, eerr
		}
		eligible, eerr := lyrics.EditorTagEligible(path)
		if eerr != nil || !eligible {
			return false, false, eerr
		}
		// The pre-mutation bytes are captured and recorded BEFORE the file is
		// touched (write-ahead, matching every other reconcile-family
		// command), so the backup record is restorable (CodeRabbit
		// 4101188299, Copilot 4101172972), not just an audit trail of which
		// paths were touched.
		original, rerr := os.ReadFile(path) //nolint:gosec // reason: path is caller-controlled library enumeration
		if rerr != nil {
			return false, false, fmt.Errorf("read %s for backup: %w", path, rerr)
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir
			if ferr != nil {
				return false, false, ferr
			}
			backupFile = f
		}
		if berr := appendEditorTagBackup(backupFile, path, original); berr != nil {
			return false, false, berr
		}
		return injectEditorTag(path)
	}

	var scanned, stamped, changed, errored, degraded int
	for _, root := range roots {
		res, werr := walkEditorTagRoot(ctx, root, perFile)
		scanned += res.Scanned
		stamped += res.Stamped
		changed += res.Changed
		errored += res.Errored
		degraded += res.Degraded
		if werr != nil {
			// classifyWalkError strips the path (#483 finding 6: private).
			slog.Error("reconcile-editor-tag: walk failed", "cause", classifyWalkError(werr))
			return 1
		}
	}

	verb := "would stamp"
	if args.Yes {
		verb = "stamped"
	}
	_, _ = fmt.Fprintf(out, "reconcile-editor-tag: scanned %d .lrc/.elrc file(s); %s %d, changed %d, errored %d, degraded %d%s\n",
		scanned, verb, stamped, changed, errored, degraded, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup written to %s\n", backupPath)
	}
	// degraded (unreadable subdir) is "partial, not clean", per #487.
	if errored > 0 || degraded > 0 {
		return 1
	}
	return 0
}

// editorTagBackfillMarker names the one-shot serve-mode backfill in
// maintenance_markers; its presence means the pass already ran.
const editorTagBackfillMarker = "editor_tag_backfill_483"

// editorTagBackfillDegradedAttemptsMarker durably counts this pass's own
// consecutive degraded startup attempts (#1084), under the #1084-generalized
// incrementDegradedAttempts/clearDegradedAttempts (see reconcile_lrc.go). It
// is a distinct maintenance_markers row from editorTagBackfillMarker, whose
// mere PRESENCE means "done" -- writing an in-progress attempt count under
// that same name would make editorTagBackfillDone report done=true before the
// pass has actually completed.
const editorTagBackfillDegradedAttemptsMarker = "editor_tag_backfill_483_degraded_attempts"

// runEditorTagBackfill runs the [re:canticle] backfill once per database in
// serve mode, touching files unattended (a tiny idempotent header line;
// `scan reconcile-editor-tag --yes` is the manual backstop). Quiet (no
// path-bearing logs), but NOT backup-less: it writes the same restorable
// JSONL record the CLI does, to a file beside the database (CodeRabbit
// 4101188299, Copilot 4101172972) -- see the backupPath comment below for why
// that location, since neither sibling startup pass (runIdentityBackfill,
// runLRCStackedCheck) offers a file-backup precedent to follow.
//
// CALLED SYNCHRONOUSLY, BEFORE THE WORKER STARTS (#483 finding 3b): the
// caller (runServe) runs this to completion on the startup goroutine itself,
// ahead of launching the worker, the scan scheduler, the word-recheck sweep,
// and the timing-revalidation sweep -- every in-process writer of a .lrc
// file. lyrics.InjectEditorTag's own pre-rename guard is only BEST-EFFORT
// (Copilot 4100857291, CodeRabbit 4100866202): it narrows, but does not
// close, the window between its read and its rename, so running this pass
// concurrently with another writer could still lose a write on either side.
// Unlike #470's stacked-.lrc startup check (runLRCStackedCheck), which
// launches as an ordinary background goroutine alongside the worker's and
// never touches a file, this pass DOES mutate files unattended, so it is the
// first startup pass that actually needs to run ahead of the worker rather
// than beside it. The one-time cost is bounded: this func returns
// immediately once editorTagBackfillDone reports true, so only the single
// boot that performs the backfill pays a startup delay -- every later boot
// on that database returns on the marker lookup before touching a library
// root. The separate-process CLI, `scan reconcile-editor-tag --yes`, has no
// such ordering guarantee against a running serve process and relies on the
// same best-effort guard; run it only while serve is stopped or idle.
//
// GOVERNING INVARIANT (#483 finding 1, per #470): never stamp on a walk that
// did not cover every root or hit a per-file error -- retried next boot. A
// raced file (Changed>0, Copilot 4101172948) is treated the same way: it was
// never actually stamped, so it must not be silently folded into "clean".
//
// SINGLE WALK, NO SEPARATE PREFLIGHT (Copilot 4101172998, CodeRabbit
// 4101188315): an earlier version ran a full lrcbackfill.Run dry-run walk per
// root ONLY to obtain MediaEntries before running this walk again to tag --
// doubling traversal and sidecar reads before serve starts, and (worse)
// lrcbackfill's own MediaEntries never recognizes ".elrc", so a root holding
// only eligible .elrc sidecars read as empty and was skipped in its entirety.
// walkEditorTagRoot now derives the same "did this root have anything that
// looks like actual library content" signal (editorTagWalkResult.Media)
// DURING the one tagging walk itself, using the same audio-extension
// definition (scanner.IsAudioFile) plus BOTH active sidecar kinds
// (sidecar.KindLineSynced, sidecar.KindWordSynced) -- see
// editorTagWalkResult's doc comment. A root with zero media entries still
// counts as unavailable (the #470 invariant); it is just computed correctly
// now, and without a second walk.
//
// BOUNDED DEGRADED RETRY (#1084): a permanently degraded root (bad
// permissions, a stale/decommissioned library), a per-file error, or a
// persistently raced file that never clears would otherwise re-walk the
// whole library on every boot forever -- the same failure mode #922 fixed
// for the #470 stacked-.lrc check. This pass now keeps its OWN counter
// (editorTagBackfillDegradedAttemptsMarker) under the shared
// incrementDegradedAttempts/clearDegradedAttempts helpers and the same
// maxDegradedAttempts ceiling: past that many consecutive degraded startups,
// the done-marker is stamped anyway (a give-up, logged once at Warn with
// counts only, never a path) instead of retrying forever. A clean,
// fully-covered run clears the counter. An earlier version of this comment
// argued the per-boot cost of retrying forever was acceptable here (a cheap
// read-mostly walk, unlike #922's rewrite-plus-.orig-backup); that rebuttal
// was rejected -- a permanently degraded root still means an unbounded walk
// on every boot for the life of the deployment, which is itself the defect
// #1084 tracks.
func runEditorTagBackfill(ctx context.Context, sqlDB *sql.DB, cfg config.Config, selfWrites *selfwrite.Registry) {
	done, err := editorTagBackfillDone(ctx, sqlDB)
	if err != nil {
		slog.Warn("editor-tag backfill: marker check failed; skipping this startup", "error", err)
		return
	}
	if done {
		return
	}

	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		slog.Warn("editor-tag backfill: failed to list libraries; skipping this startup", "error", err)
		return
	}
	if len(libs) == 0 {
		// Nothing configured yet; leave the marker unset so a later startup
		// (once a library exists) actually performs the backfill.
		return
	}

	// backupPath lives beside the database, like the CLI's own default (and
	// every other reconcile-family backup), NOT as a per-file ".orig" (the
	// lrcbackfill pattern): neither sibling startup pass offers a "beside the
	// DB" vs "per-file .orig" precedent to follow one way or the other --
	// runIdentityBackfill (the closest analog: an unattended, marker-gated,
	// mutating startup pass) writes no backup at all for its DB-row changes,
	// and #470's own startup check (runLRCStackedCheck) never mutates a file
	// in the first place, so it never needed one. Absent a mutating-startup
	// precedent either way, this follows the CLI sibling within the SAME
	// file (runReconcileEditorTag's backupPath default) rather than
	// lrcbackfill's ".orig"-per-file shape, because a single JSONL beside the
	// DB is one artifact to point an operator at instead of one stray file
	// scattered per library directory. Logged once at Info: it is an
	// operator-facing, DB-adjacent artifact path, not library metadata.
	backupPath := filepath.Join(filepath.Dir(cfg.DB.Path),
		fmt.Sprintf("editor-tag-backfill-startup-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("editor-tag backfill: failed to close startup backup file", "path", backupPath, "error", cerr)
			}
		}
	}()

	perFile := func(path string) (bool, bool, error) {
		eligible, eerr := lyrics.EditorTagEligible(path)
		if eerr != nil || !eligible {
			return false, false, eerr
		}
		// Write-ahead, same as the CLI: the restorable backup record (exact
		// pre-mutation bytes) is durable before the file is touched.
		original, rerr := os.ReadFile(path) //nolint:gosec // reason: path is derived from a configured library root, not untrusted input
		if rerr != nil {
			return false, false, fmt.Errorf("read %s for backup: %w", path, rerr)
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is derived from the configured db dir, not untrusted input
			if ferr != nil {
				return false, false, ferr
			}
			backupFile = f
			slog.Info("editor-tag backfill: writing a restorable backup before rewriting sidecars", "path", backupPath)
		}
		if berr := appendEditorTagBackup(backupFile, path, original); berr != nil {
			return false, false, berr
		}
		if selfWrites != nil {
			selfWrites.Record(path)
		}
		return injectEditorTag(path)
	}

	var scanned, stamped, changed, errored, degraded int
	rootUnavailable := false
	for _, l := range libs {
		res, werr := walkEditorTagRoot(ctx, l.Path, perFile)
		scanned += res.Scanned
		stamped += res.Stamped
		changed += res.Changed
		errored += res.Errored
		degraded += res.Degraded
		if werr != nil {
			if errors.Is(werr, context.Canceled) {
				return
			}
			slog.Warn("editor-tag backfill: root walk failed; will retry on next startup", "library_id", l.ID, "cause", classifyWalkError(werr))
			rootUnavailable = true
			continue
		}
		if res.Media == 0 {
			slog.Warn("editor-tag backfill: root appears empty or not mounted yet; will retry on next startup", "library_id", l.ID)
			rootUnavailable = true
		}
	}

	if rootUnavailable || degraded > 0 || errored > 0 || changed > 0 {
		// The counter itself is unavailable; fail open the same way
		// runLRCStackedCheck does on an incrementDegradedAttempts error --
		// retry next startup, without a ceiling decision this time, rather
		// than stamping on unreliable state.
		attempts, incErr := incrementDegradedAttempts(ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker)
		if incErr != nil {
			slog.Warn("editor-tag backfill: failed to record the degraded-attempt count; will retry next startup", "error", incErr)
			return
		}
		if attempts < maxDegradedAttempts {
			slog.Warn("editor-tag backfill: walk did not cleanly cover every configured root this startup; will retry next startup",
				"scanned", scanned, "stamped", stamped, "changed", changed, "errored", errored, "degraded", degraded,
				"degraded_attempts", attempts, "max_degraded_attempts", maxDegradedAttempts)
			return
		}

		// Ceiling reached (#1084, mirroring #922): stop retrying, but the
		// stamp records a GAVE-UP outcome via a Warn, never silence -- an
		// operator must still learn the backfill never completed cleanly.
		// Counts only, no path/library-root identifying detail.
		slog.Warn("editor-tag backfill: gave up after repeated degraded startup attempts; this pass will not run again on this deployment",
			"degraded_attempts", attempts, "max_degraded_attempts", maxDegradedAttempts,
			"scanned", scanned, "stamped", stamped, "changed", changed, "errored", errored, "degraded", degraded)
		if err := markEditorTagBackfillDone(ctx, sqlDB); err != nil {
			slog.Warn("editor-tag backfill: gave up but failed to record the marker; will retry next startup", "error", err)
			return
		}
		if cerr := clearDegradedAttempts(ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); cerr != nil {
			slog.Warn("editor-tag backfill: gave-up marker recorded but failed to clear the degraded-attempt counter", "error", cerr)
		}
		return
	}

	slog.Info("editor-tag backfill: complete", "scanned", scanned, "stamped", stamped)
	if err := markEditorTagBackfillDone(ctx, sqlDB); err != nil {
		slog.Warn("editor-tag backfill: completed but failed to record marker; it may re-run next startup", "error", err)
		return
	}
	// A genuinely clean/completed run resets the counter: a deployment that
	// recovers from a transient blip (a root remounts, a permission fix
	// lands) must not carry a stale degraded-attempt count into some
	// unrelated future degradation.
	if cerr := clearDegradedAttempts(ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); cerr != nil {
		slog.Warn("editor-tag backfill: completed but failed to clear the degraded-attempt counter", "error", cerr)
	}
}

// editorTagBackfillDone reports whether the one-shot backfill marker is present.
func editorTagBackfillDone(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	var one int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM maintenance_markers WHERE name = ?`, editorTagBackfillMarker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query maintenance marker %q: %w", editorTagBackfillMarker, err)
	}
	return true, nil
}

// markEditorTagBackfillDone records the marker so the pass is skipped hereafter.
func markEditorTagBackfillDone(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_markers (name) VALUES (?)`, editorTagBackfillMarker); err != nil {
		return fmt.Errorf("record maintenance marker %q: %w", editorTagBackfillMarker, err)
	}
	return nil
}
