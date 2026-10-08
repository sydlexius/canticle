package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyricblock"
)

// ScanMarkWrongCmd marks one track's current lyrics as wrong (#1398): the files
// are backed up and removed, the words are blocked, and the track is queued
// again. Dry-run unless --yes. The track is chosen by work item id only.
type ScanMarkWrongCmd struct {
	ID         int64  `arg:"--id" help:"work_queue row id of the track (required)"`
	Yes        bool   `arg:"--yes" help:"actually apply (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of the lyric files removed (default: <db-dir>/mark-wrong-backup-<ts>.jsonl)" default:""`
	Tail       bool   `arg:"--tail" help:"also print the path of each lyric file backed up (applied runs only)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanListBlocksCmd lists the blocked lyric results. Counts only unless --tail.
type ScanListBlocksCmd struct {
	Tail       bool   `arg:"--tail" help:"also print one line per block (id, work item, lane, upstream, time, identity keys)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanUnblockCmd removes a block, or every block on a work item's track, and
// reopens a track that settled as blocked, or (--orphans) clears every block
// whose track has no work_queue row. Dry-run unless --yes. Exactly one of --id,
// --work-item and --orphans.
type ScanUnblockCmd struct {
	ID         int64  `arg:"--id" help:"block id (see scan list-blocks --tail)"`
	WorkItem   int64  `arg:"--work-item" help:"work_queue row id; removes every block on that track"`
	Orphans    bool   `arg:"--orphans" help:"remove every block whose track has no work_queue row"`
	Yes        bool   `arg:"--yes" help:"actually apply (without it, prints what would change)"`
	Tail       bool   `arg:"--tail" help:"with --orphans, also print one line per block (id, work item, identity keys)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// openBlockDB loads config and opens the database; write is true when the run
// will change rows.
func openBlockDB(ctx context.Context, cfgPath string, write bool) (config.Config, *sql.DB, bool) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return cfg, nil, false
	}
	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, write)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return cfg, nil, false
	}
	return cfg, sqlDB, true
}

func runMarkWrong(ctx context.Context, out io.Writer, args ScanMarkWrongCmd) int {
	if args.ID <= 0 {
		_, _ = fmt.Fprintln(out, "scan mark-wrong needs --id <work item id>")
		return 2
	}
	cfg, sqlDB, ok := openBlockDB(ctx, args.ConfigPath, args.Yes)
	if !ok {
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
	// A backup inside a library root or named like a lyric file would be
	// replaced by the very run that wrote it (same guard as queue mark-instrumental).
	if args.Backup != "" && backupPathUnsafe(args.Backup, roots) {
		_, _ = fmt.Fprintln(out, "--backup must not be a lyric file (.lrc, .txt, .elrc) or inside a library root")
		return 2
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path),
			fmt.Sprintf("mark-wrong-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	bk := &lazyBackupFile{path: backupPath, what: "mark-wrong backup"}
	defer bk.close()
	var tailPaths []string
	report := func(b lyricblock.Backup) error {
		f, ferr := bk.file()
		if ferr != nil {
			return ferr
		}
		if err := lyricblock.AppendBackup(f, b); err != nil {
			return err
		}
		tailPaths = append(tailPaths, b.Path)
		return nil
	}

	svc := lyricblock.New(sqlDB, slog.Default(), nil)
	res, merr := svc.Mark(ctx, lyricblock.MarkRequest{WorkItemID: args.ID, Roots: roots, DryRun: !args.Yes, Report: report})
	_, _ = fmt.Fprintf(out, "work item: %d\n", args.ID)
	if args.Yes && bk.f != nil {
		_, _ = fmt.Fprintf(out, "backup: %s\n", backupPath)
	}
	if word := markWrongRefusal(merr, res); word != "" {
		_, _ = fmt.Fprintf(out, "refused: %s\n", word)
		return 1
	}
	if merr != nil {
		// The service's errors carry the work item id and no path.
		slog.Error("mark-wrong failed", "work_item", args.ID, "error", merr)
		if res != (lyricblock.MarkResult{}) {
			_, _ = fmt.Fprintf(out, "failed after changes: lyric files backed up %d, removed %d, new blocks %d, reopened %t; run it again to finish\n",
				res.Files, res.Removed, res.NewBlocks, res.Reopened)
		} else {
			_, _ = fmt.Fprintln(out, "failed: see the errors above")
		}
		return 1
	}
	if !args.Yes {
		_, _ = fmt.Fprintf(out, "dry run: nothing changed (pass --yes to mark wrong)\nlyric files that would be backed up: %d\n", res.Files)
		return 0
	}
	reopened := "no"
	if res.Reopened {
		reopened = "yes"
	}
	_, _ = fmt.Fprintf(out, "lyric files backed up: %d\nlyric files removed: %d\nnew blocks: %d\nreopened: %s\ncache entries invalidated: %d\n",
		res.Files, res.Removed, res.NewBlocks, reopened, res.CacheInvalidated)
	if args.Tail {
		for _, p := range tailPaths {
			_, _ = fmt.Fprintf(out, "file: %s\n", p)
		}
	}
	return 0
}

// markWrongRefusal maps the service's refusal errors to a path-free outcome
// word, or "" when err is nil or not a refusal. A refusal changes nothing, so a
// non-zero res means the error came after Mark began changing things: a failure,
// whatever sentinel it wraps.
func markWrongRefusal(err error, res lyricblock.MarkResult) string {
	if res != (lyricblock.MarkResult{}) {
		return ""
	}
	switch {
	case errors.Is(err, lyricblock.ErrNotFound):
		return "not found"
	case errors.Is(err, lyricblock.ErrBusy):
		return "in flight"
	case errors.Is(err, lyricblock.ErrNoSidecar):
		return "no lyric file on disk"
	case errors.Is(err, lyricblock.ErrManualInstrumental):
		return "marked instrumental by hand"
	case errors.Is(err, lyricblock.ErrNotMarkable):
		return "failed or unavailable"
	}
	return ""
}

func runListBlocks(ctx context.Context, out io.Writer, args ScanListBlocksCmd) int {
	_, sqlDB, ok := openBlockDB(ctx, args.ConfigPath, false)
	if !ok {
		return 1
	}
	defer func() { _ = sqlDB.Close() }() //nolint:errcheck // reason: best-effort close on command exit
	blocks, err := lyricblock.NewStore(sqlDB, slog.Default()).List(ctx, lyricblock.ListFilter{})
	if err != nil {
		slog.Error("failed to list blocks", "error", err)
		return 1
	}
	byLane := map[string]int{}
	byUpstream := map[string]int{}
	for _, b := range blocks {
		byLane[dimLabel(b.Lane)]++
		byUpstream[dimLabel(b.Upstream)]++
	}
	_, _ = fmt.Fprintf(out, "blocks: %d\n", len(blocks))
	printDim(out, "lane", byLane)
	printDim(out, "upstream", byUpstream)
	if args.Tail {
		for _, b := range blocks {
			_, _ = fmt.Fprintf(out, "block %d: work item %d, lane %s, upstream %s, created %s, artist %q, title %q\n",
				b.ID, b.WorkQueueID, dimLabel(b.Lane), dimLabel(b.Upstream), b.CreatedAt, b.ArtistKey, b.TitleKey)
		}
	}
	return 0
}

func dimLabel(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func printDim(out io.Writer, name string, m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(out, "by %s %s: %d\n", name, k, m[k])
	}
}

func runUnblock(ctx context.Context, out io.Writer, args ScanUnblockCmd) int {
	set := 0
	for _, b := range []bool{args.ID > 0, args.WorkItem > 0, args.Orphans} {
		if b {
			set++
		}
	}
	if set != 1 {
		_, _ = fmt.Fprintln(out, "scan unblock needs exactly one of --id <block id>, --work-item <work item id> and --orphans")
		return 2
	}
	_, sqlDB, ok := openBlockDB(ctx, args.ConfigPath, args.Yes)
	if !ok {
		return 1
	}
	defer func() { _ = sqlDB.Close() }() //nolint:errcheck // reason: best-effort close on command exit
	if args.Orphans {
		return runUnblockOrphans(ctx, out, sqlDB, args)
	}

	res, err := lyricblock.New(sqlDB, slog.Default(), nil).Unblock(ctx, lyricblock.UnblockRequest{BlockID: args.ID, WorkItemID: args.WorkItem, DryRun: !args.Yes})
	if err != nil {
		return reportUnblockErr(out, err)
	}
	if !args.Yes {
		_, _ = fmt.Fprintln(out, "dry run: nothing changed (pass --yes to unblock)")
	}
	removed, reopened := "blocks removed", "rows reopened"
	if !args.Yes {
		removed, reopened = "blocks that would be removed", "rows that would be reopened"
	}
	_, _ = fmt.Fprintf(out, "%s: %d\n%s: %d\n", removed, res.Removed, reopened, res.Reopened)
	return 0
}

func reportUnblockErr(out io.Writer, err error) int {
	if errors.Is(err, lyricblock.ErrNotFound) {
		_, _ = fmt.Fprintln(out, "refused: not found")
		return 1
	}
	slog.Error("unblock failed", "error", err)
	_, _ = fmt.Fprintln(out, "failed: see the errors above")
	return 1
}

// runUnblockOrphans clears the blocks whose track identity has no work_queue
// row. The listing before the delete feeds the dry-run count and --tail; the
// delete re-checks orphan status itself, so a row created in between keeps its
// blocks and the applied count is what was actually removed.
func runUnblockOrphans(ctx context.Context, out io.Writer, sqlDB *sql.DB, args ScanUnblockCmd) int {
	store := lyricblock.NewStore(sqlDB, slog.Default())
	orphans, err := store.List(ctx, lyricblock.ListFilter{Orphans: true})
	if err != nil {
		slog.Error("failed to list orphaned blocks", "error", err)
		_, _ = fmt.Fprintln(out, "failed: see the errors above")
		return 1
	}
	removed := len(orphans)
	label := "orphaned blocks that would be removed"
	if args.Yes {
		if removed, err = store.DeleteOrphans(ctx, sqlDB); err != nil {
			slog.Error("failed to remove orphaned blocks", "error", err)
			_, _ = fmt.Fprintln(out, "failed: see the errors above")
			return 1
		}
		label = "orphaned blocks removed"
	} else {
		_, _ = fmt.Fprintln(out, "dry run: nothing changed (pass --yes to unblock)")
	}
	_, _ = fmt.Fprintf(out, "%s: %d\n", label, removed)
	if args.Tail {
		for _, b := range orphans {
			_, _ = fmt.Fprintf(out, "block %d: work item %d, artist %q, title %q\n", b.ID, b.WorkQueueID, b.ArtistKey, b.TitleKey)
		}
	}
	return 0
}
