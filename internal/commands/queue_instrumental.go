package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/instrumentalmark"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
)

// QueueMarkInstrumentalCmd marks tracks instrumental by hand (#1404). Dry-run
// unless --yes. Select tracks with --id and/or --path; both repeat.
type QueueMarkInstrumentalCmd struct {
	IDs        []int64  `arg:"--id,separate" help:"work_queue row id; repeat for more than one"`
	Paths      []string `arg:"--path,separate" help:"audio file path; resolves to its work_queue row(s). Repeat for more than one"`
	Yes        bool     `arg:"--yes" help:"actually apply (without it, prints what would change)"`
	Backup     string   `arg:"--backup" help:"path for the JSONL backup of the lyric files replaced (default: <db-dir>/instrumental-<mark|unmark>-backup-<ts>.jsonl)" default:""`
	ConfigPath string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueUnmarkInstrumentalCmd withdraws a hand-made instrumental mark and
// re-queues the track. Same flags as QueueMarkInstrumentalCmd.
type QueueUnmarkInstrumentalCmd QueueMarkInstrumentalCmd

// instrumentalCounts is the whole of what the command prints: counts only. A
// sidecar path, artist, title or lyric text must never reach stdout.
type instrumentalCounts struct {
	done, dry, already, notFound, inFlight, failed, files int
}

// runQueueInstrumental drives Marker.Mark (unmark=false) or Marker.Unmark over
// the selected rows. One row failing does not stop the others; the exit status
// is 1 if any row failed, was not found or was in flight. Stdout is aggregate-only.
func runQueueInstrumental(ctx context.Context, out io.Writer, unmark bool, args QueueMarkInstrumentalCmd) int {
	verb := "mark"
	if unmark {
		verb = "unmark"
	}
	if len(args.IDs) == 0 && len(args.Paths) == 0 {
		_, _ = fmt.Fprintf(out, "queue %s-instrumental needs at least one --id or --path\n", verb)
		return 2
	}
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
	defer func() { _ = sqlDB.Close() }() //nolint:errcheck // reason: best-effort close on command exit

	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		slog.Error("failed to list libraries", "error", err)
		return 1
	}
	roots := make([]string, 0, len(libs))
	for _, l := range libs {
		roots = append(roots, l.Path)
	}
	marker := instrumentalmark.New(sqlDB, lyrics.NewLRCWriter(roots...))

	// A backup that is itself a lyric file, or sits where a scan reads lyric
	// files, would be replaced by the very run that wrote it.
	if args.Backup != "" && backupPathUnsafe(args.Backup, roots) {
		_, _ = fmt.Fprintln(out, "--backup must not be a lyric file (.lrc, .txt, .elrc) or inside a library root")
		return 2
	}

	var c instrumentalCounts
	var ids []int64
	seen := make(map[int64]bool, len(args.IDs))
	for _, id := range args.IDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	seenPath := make(map[string]bool, len(args.Paths))
	for _, p := range args.Paths {
		if seenPath[p] {
			continue
		}
		seenPath[p] = true
		// work_queue.source_path holds the audio file path the scan enqueued
		// (the same value scan_results.file_path carries); the scan joins an
		// absolute library root with directory entries, so it is always
		// absolute and clean. The selector is therefore matched exactly as
		// given: a non-absolute one is not found, without a query. A path can
		// have more than one row, so every match is processed.
		if !filepath.IsAbs(p) {
			c.notFound++
			continue
		}
		found, qerr := rowsForSourcePath(ctx, sqlDB, p)
		if qerr != nil {
			slog.Error("failed to look up path", "error", qerr)
			return 1
		}
		if len(found) == 0 {
			c.notFound++
		}
		for _, id := range found {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = lyrics.DefaultBackupPath(cfg.DB.Path, "instrumental-"+verb, time.Now())
	}
	bk := &lyrics.LazyBackupFile{Path: backupPath, What: "instrumental backup"}
	defer bk.Close()
	report := func(rec instrumentalmark.Record) error {
		f, ferr := bk.File()
		if ferr != nil {
			return ferr
		}
		return instrumentalmark.AppendRecord(f, rec)
	}
	opts := instrumentalmark.Options{DryRun: !args.Yes, Report: report}

	for _, id := range ids {
		var res instrumentalmark.Result
		var rerr error
		if unmark {
			res, rerr = marker.Unmark(ctx, id, opts)
		} else {
			res, rerr = marker.Mark(ctx, id, opts)
		}
		switch {
		case errors.Is(rerr, queue.ErrManualInstrumentalNotFound):
			c.notFound++
		case errors.Is(rerr, queue.ErrManualInstrumentalInFlight):
			c.inFlight++
		case rerr != nil:
			// The service's errors carry the work item id and no path.
			slog.Error("instrumental "+verb+" failed", "work_item", id, "error", rerr)
			c.failed++
		default:
			c.files += res.FilesBackedUp
			switch res.Outcome {
			case instrumentalmark.OutcomeDryRun:
				c.dry++
			case instrumentalmark.OutcomeAlreadyMarked, instrumentalmark.OutcomeNotMarked:
				c.already++
			default:
				c.done++
			}
		}
	}

	printInstrumentalSummary(out, unmark, args.Yes, c)
	if args.Yes {
		if bk.Opened() {
			_, _ = fmt.Fprintf(out, "backup: %s\n", backupPath)
		} else if c.failed > 0 {
			_, _ = fmt.Fprintln(out, "backup: not written (see the errors above)")
		} else {
			_, _ = fmt.Fprintln(out, "backup: none written (no lyric files were replaced)")
		}
	}
	// Exit 0 only when every selected row ended in a mark, an unmark, or an
	// already-marked / not-marked no-op.
	if c.failed > 0 || c.notFound > 0 || c.inFlight > 0 {
		return 1
	}
	return 0
}

// backupPathUnsafe reports whether a --backup path would be destroyed by the
// run it backs up: a lyric file name, or a location inside a library root.
// Both the as-given and the symlink-resolved forms are tested.
func backupPathUnsafe(backup string, roots []string) bool {
	switch strings.ToLower(filepath.Ext(backup)) {
	case ".lrc", ".txt", ".elrc":
		return true
	}
	abs, err := filepath.Abs(backup)
	if err != nil {
		abs = filepath.Clean(backup)
	}
	// Three forms: as given, parent-resolved (a path not created yet), and the
	// complete path resolved (an existing file that is itself a symlink).
	// Every form gets the extension and the inside-a-library test.
	forms := []string{abs,
		filepath.Join(pathutil.CanonicalPath(filepath.Dir(abs)), filepath.Base(abs)),
		pathutil.CanonicalPath(abs)}
	for _, form := range forms {
		switch strings.ToLower(filepath.Ext(form)) {
		case ".lrc", ".txt", ".elrc":
			return true
		}
		for _, root := range roots {
			absRoot, canonRoot := pathutil.CanonicalRoot(root)
			if pathutil.WithinRoot(absRoot, form) || pathutil.WithinRoot(canonRoot, form) {
				return true
			}
		}
	}
	// Remaining window: the open follows a final symlink created after this
	// guard (no no-follow append open exists in the backup helpers).
	return false
}

// rowsForSourcePath returns the ids of the work_queue rows enqueued for the
// audio file at path, oldest first.
func rowsForSourcePath(ctx context.Context, sqlDB *sql.DB, path string) (ids []int64, retErr error) {
	rows, err := sqlDB.QueryContext(ctx, `SELECT id FROM work_queue WHERE source_path = ? ORDER BY id`, path)
	if err != nil {
		return nil, fmt.Errorf("query work items by path: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close work item rows: %w", cerr)
		}
	}()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan work item id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate work items: %w", err)
	}
	return ids, nil
}

// printInstrumentalSummary prints counts only.
func printInstrumentalSummary(out io.Writer, unmark, applied bool, c instrumentalCounts) {
	verb, done, already, would, files := "mark", "marked", "already marked", "would mark", "lyric files backed up"
	if unmark {
		verb, done, already, would, files = "unmark", "unmarked", "not marked", "would unmark", "lyric files backed up"
	}
	if !applied {
		_, _ = fmt.Fprintf(out, "dry run: nothing changed (pass --yes to %s)\n", verb)
		files = "files that would be backed up"
	}
	_, _ = fmt.Fprintf(out, "%s: %d\n%s: %d\n%s: %d\nnot found: %d\nin flight: %d\nfailed: %d\n%s: %d\n",
		done, c.done, would, c.dry, already, c.already, c.notFound, c.inFlight, c.failed, files, c.files)
}
