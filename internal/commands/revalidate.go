package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/timing"
)

// RevalidateCmd re-judges .lrc files ALREADY ON DISK against the duration of the
// audio they sit beside, and remediates the ones whose timing does not fit: the
// backlog counterpart to the accept-time guard, which only ever sees new fetches.
//
// NOT TO BE CONFUSED WITH `realign`, which is about a sidecar's LOCATION -- it
// re-attaches an orphan to renamed or moved audio and never inspects timing.
// This command is about a sidecar's CONTENT: the file is next to the right audio,
// but its cues run past the end of it. A file can need both, and they are
// independent passes.
//
// Dry-run by default: without --apply it reports counts and writes nothing at
// all. Removal is reversible by default (the .lrc is moved under a quarantine
// root, not deleted); --purge opts into a hard delete.
type RevalidateCmd struct {
	Roots         []string `arg:"positional" help:"directory roots to walk (default: every configured library root)"`
	Library       string   `arg:"--library" help:"limit to a single library (name or numeric id)" default:""`
	Apply         bool     `arg:"--apply" help:"actually remediate (without it, only counts are reported and nothing is written)"`
	OnFail        string   `arg:"--on-fail" help:"what to do with a MisSynced .lrc: demote (keep the words as .txt) or delete" default:"demote"`
	Purge         bool     `arg:"--purge" help:"hard-delete a removed .lrc instead of quarantining it (NOT reversible)"`
	QuarantineDir string   `arg:"--quarantine-dir" help:"root that removed .lrc files are moved under (default: <db-dir>/quarantine)" default:""`
	Tail          string   `arg:"--tail" help:"append per-file offender paths to this LOCAL file for your own inspection (never printed to stdout)" default:""`
	Backup        string   `arg:"--backup" help:"path for the JSONL backup of applied actions (default: <db-dir>/revalidate-backup-<ts>.jsonl)" default:""`
	ConfigPath    string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// runRevalidate walks the resolved roots, classifies every .lrc through the
// shared internal/timing predicate, and (under --apply) remediates through
// internal/realign's one apply path.
//
// STDOUT IS AGGREGATE-ONLY, BY CONSTRUCTION. A sidecar path carries the artist,
// album, and track title in its directory structure, and this is a tool an
// operator runs against a private library and then pastes the output of into an
// issue. So no per-file path is ever written to out -- not on success, not on
// error (per-file failures go to slog, which the operator controls the sink of),
// and not in a summary. The optional --tail file is the ONE place per-file
// detail lands, and it is a local file the operator asked for by name.
func runRevalidate(ctx context.Context, out io.Writer, args RevalidateCmd) int {
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

	roots, libID, code := revalidateRoots(ctx, out, sqlDB, args)
	if code != 0 {
		return code
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "revalidate: no roots to scan; add a library with 'canticle library add' or pass one as an argument")
		return 0
	}

	quarantineDir := strings.TrimSpace(args.QuarantineDir)
	if quarantineDir == "" {
		quarantineDir = filepath.Join(filepath.Dir(cfg.DB.Path), "quarantine")
	}
	opts := revalidate.Options{
		Roots:         roots,
		OnFail:        revalidate.OnFail(strings.TrimSpace(args.OnFail)),
		Purge:         args.Purge,
		QuarantineDir: quarantineDir,
		LibraryID:     libID,
	}
	if verr := opts.Validate(); verr != nil {
		_, _ = fmt.Fprintln(out, verr)
		return 2
	}

	durations := audiodur.New(sqlDB, scanner.DurationReaderVersion)
	plan, err := revalidate.New(durations.Lookup, opts).Plan(ctx)
	if err != nil {
		slog.Error("revalidate: scan failed", "error", err)
		return 1
	}

	// The tail file is written for BOTH a dry run and an apply: seeing which
	// files a run would touch, before touching them, is the entire point of the
	// dry run for an operator who intends to spot-check a few by hand.
	if terr := writeRevalidateTail(args.Tail, plan.Findings); terr != nil {
		slog.Error("revalidate: could not write the tail file", "error", terr)
		return 1
	}

	printRevalidateCounts(out, plan.Counts, args.Apply)
	if !args.Apply {
		_, _ = fmt.Fprintf(out, "revalidate: %d file(s) would be remediated%s\n", len(plan.Moves), suffixRevalidateDryRun(args.Apply))
		return 0
	}
	return applyRevalidate(out, cfg, args, plan)
}

// revalidateRoots resolves the roots to walk: explicit positionals win, then a
// --library scope, then every configured library.
func revalidateRoots(ctx context.Context, out io.Writer, sqlDB *sql.DB, args RevalidateCmd) ([]string, int64, int) {
	if len(args.Roots) > 0 {
		return args.Roots, 0, 0
	}
	repo := library.New(sqlDB)
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, repo, args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return nil, 0, 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return nil, 0, 1
		}
		return []string{lib.Path}, lib.ID, 0
	}
	libs, lerr := repo.List(ctx)
	if lerr != nil {
		slog.Error("failed to list libraries", "error", lerr)
		return nil, 0, 1
	}
	roots := make([]string, 0, len(libs))
	for _, l := range libs {
		roots = append(roots, l.Path)
	}
	return roots, 0, 0
}

// applyRevalidate runs the planned actions through realign.Apply and prints the
// aggregate outcome.
func applyRevalidate(out io.Writer, cfg config.Config, args RevalidateCmd, plan revalidate.Plan) int {
	if len(plan.Moves) == 0 {
		_, _ = fmt.Fprintln(out, "revalidate: nothing to remediate")
		return 0
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("revalidate-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	applied, aerr := realign.New(nil, cfg.Realign).Apply(plan.Moves, backupPath, realign.Policy{AllowHeuristic: true})
	if aerr != nil {
		slog.Error("revalidate: apply failed", "error", aerr)
		return 1
	}

	var demoted, quarantined, purged, failed int
	for _, a := range applied {
		if a.Err != nil {
			failed++
			// Per-file detail goes to the structured log, never to stdout.
			slog.Warn("revalidate: action failed; leaving the file in place", "path", a.Move.Orphan, "kind", a.Move.Kind, "error", a.Err)
			continue
		}
		switch a.Move.Kind {
		case realign.KindDemote:
			demoted++
		case realign.KindQuarantine:
			quarantined++
		case realign.KindPurge:
			purged++
		}
	}
	_, _ = fmt.Fprintf(out, "revalidate applied: demoted=%d quarantined=%d purged=%d failed=%d\n", demoted, quarantined, purged, failed)
	_, _ = fmt.Fprintf(out, "backup of applied actions written to %s\n", backupPath)
	if failed > 0 {
		return 1
	}
	return 0
}

// printRevalidateCounts emits the aggregate distribution. Counts only: no path,
// no artist, no title, no lyric text.
func printRevalidateCounts(out io.Writer, c revalidate.Counts, apply bool) {
	_, _ = fmt.Fprintf(out, "revalidate: scanned=%d ok=%d MisSynced=%d categorical=%d unknown-duration=%d no-audio=%d errored=%d%s\n",
		c.Scanned, c.Ok, c.MisSynced, c.Categorical, c.UnknownDuration, c.NoAudio, c.Errored, suffixRevalidateDryRun(apply))
	if c.UnknownDuration > 0 {
		// Name a remedy that actually works for THESE files. Before #684 a scan
		// short-circuited on any file that already had a sidecar before it ever
		// probed a duration, so the previous "run a scan" advice provably did
		// not fill the entries this command needs -- an operator who followed it
		// got the identical count back.
		//
		// Deliberately NOT pinned to a release number: the fix's version is not
		// known when this string is written, and a hardcoded one silently goes
		// wrong if the release slips. "Older builds" is both accurate and
		// stable.
		//
		// The older-build skip is the LIKELIEST cause, not the only one.
		// revalidate.durationOf returns unknown for three distinct reasons: no
		// duration store wired at all, a companion audio file that cannot be
		// stat'ed, and a cache miss. Only the last is what a re-scan fixes, so
		// the advice names a remedy AND a next step for when it does not help,
		// rather than asserting a single cause for every count.
		_, _ = fmt.Fprintf(out, "revalidate: %d file(s) had no exact audio duration and were left untouched (most likely cause: older builds skipped files that already had a sidecar, so re-scan the library to populate the duration cache; if the count persists after a fresh scan, check that the audio files are readable and that the duration cache is configured)\n", c.UnknownDuration)
	}
}

// writeRevalidateTail appends one line per non-compliant finding to the
// operator's local tail file. A path is only ever written here, never to stdout.
func writeRevalidateTail(path string, findings []revalidate.Finding) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the tail path is operator-supplied (--tail), not untrusted input
	if err != nil {
		return fmt.Errorf("open revalidate tail %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, fd := range findings {
		if fd.Outcome != timing.MisSynced && fd.Outcome != timing.Categorical {
			continue
		}
		if _, werr := fmt.Fprintf(f, "%s\t%s\tduration=%ds\toverrun=%.2fs\tratio=%.3f\taction=%s\n",
			fd.Outcome, fd.Path, fd.Duration, fd.Overrun, fd.Ratio, fd.Action); werr != nil {
			return fmt.Errorf("write revalidate tail: %w", werr)
		}
	}
	if serr := f.Sync(); serr != nil {
		return fmt.Errorf("sync revalidate tail: %w", serr)
	}
	return nil
}

// suffixRevalidateDryRun mirrors suffixDryRun for this command's --apply gate
// (rather than the --yes the reconcile family uses).
func suffixRevalidateDryRun(apply bool) string {
	if apply {
		return ""
	}
	return " [dry run; pass --apply to remediate]"
}
