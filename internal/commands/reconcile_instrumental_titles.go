package commands

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/titlerepair"
)

// ScanReconcileInstrumentalTitlesCmd repairs rows a provider settled with
// lyrics before the instrumental title rule existed (#1470): the lyric file is
// backed up and removed, its words are blocked, and the row is reopened so the
// worker's title rule settles it with a marker. Dry-run unless --yes. Stdout is
// counts only; row ids go to the --tail file and paths to the backup file.
type ScanReconcileInstrumentalTitlesCmd struct {
	Yes        bool   `arg:"--yes" help:"actually repair (without it, prints what would change); needs --backup and instrumental_title.mode on"`
	Backup     string `arg:"--backup" help:"JSONL file each lyric file is appended to before it is removed (required with --yes; appended to if it exists)" default:""`
	Tail       string `arg:"--tail" help:"append one line per strong-titled row (work item id, TAB, disposition) to this LOCAL file, never stdout (default with --yes: <backup>.rows.tsv)" default:""`
	Library    string `arg:"--library" help:"limit the rows examined to one library (name or numeric id)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// usableRoots is libraryRoots (no blank path) less every filesystem root,
// inside which every path would count as the library's.
func usableRoots(libs []models.Library) []string {
	var roots []string
	for _, p := range libraryRoots(libs) {
		if clean := filepath.Clean(p); filepath.Dir(clean) != clean {
			roots = append(roots, p)
		}
	}
	return roots
}

// samePath reports whether a and b name one file: by absolute path, or, when
// both exist, by identity (a symlink, a hard link, a case variant).
func samePath(a, b string) bool {
	abs := func(p string) string {
		if full, err := filepath.Abs(p); err == nil {
			return full
		}
		return filepath.Clean(p)
	}
	if abs(a) == abs(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// workerFileAlbum returns the album the worker would classify a row with. The
// worker replaces the row's album with the audio file's when enrichment is on
// (refreshRecordingIdentity) and leaves it alone when it is off, so off is nil
// here. The read is the worker's resolveMetadata: the audio_metadata row when
// it matches the file's current mtime, size and reader version under the
// canonical path, and scanner.ReadAudioMetadata on a miss, a failed stat or a
// failed lookup. A failed read is an error, never "": an unreadable file must
// not look like a file with no album tag, which would make the row a candidate.
func workerFileAlbum(ctx context.Context, cfg config.Config, sqlDB *sql.DB) func(string) (string, error) {
	if !cfg.Enrichment.Enabled {
		return nil
	}
	cache := audiometa.New(sqlDB, scanner.DurationReaderVersion)
	return func(sourcePath string) (string, error) {
		if info, serr := os.Stat(sourcePath); serr == nil {
			facts, found, cerr := cache.Facts(ctx, pathutil.CanonicalPath(sourcePath), info.ModTime().UnixNano(), info.Size())
			if cerr == nil && found {
				return facts.Album, nil
			}
		}
		meta, err := scanner.ReadAudioMetadata(sourcePath)
		if err != nil {
			return "", fmt.Errorf("read audio metadata: %w", err)
		}
		return meta.AlbumName, nil
	}
}

// tailFile is the --tail file as the run uses it.
type tailFile interface {
	io.Writer
	Sync() error
	Close() error
}

func runReconcileInstrumentalTitles(ctx context.Context, out io.Writer, args ScanReconcileInstrumentalTitlesCmd) int {
	return reconcileInstrumentalTitles(ctx, out, args, func(path string) (tailFile, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304, the tail path is operator-supplied (--tail), not untrusted input
	})
}

// reconcileInstrumentalTitles is the command; openTail opens the --tail file.
func reconcileInstrumentalTitles(ctx context.Context, out io.Writer, args ScanReconcileInstrumentalTitlesCmd, openTail func(string) (tailFile, error)) int {
	const name = "scan reconcile-instrumental-titles"
	if args.Yes && strings.TrimSpace(args.Backup) == "" {
		_, _ = fmt.Fprintln(out, name+" --yes needs --backup <file>: every lyric file is saved there before it is removed")
		return 2
	}
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	// The engine does not look at the mode. With the rule off a reopened row is
	// only fetched again, so nothing is opened for writing.
	mode := cfg.InstrumentalTitleMode()
	if args.Yes && mode == config.InstrumentalTitleOff {
		_, _ = fmt.Fprintln(out, "refused: instrumental_title.mode is off, so reopened rows would only be fetched again. Turn the mode on, restart serve, then rerun.")
		return 1
	}
	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, args.Yes)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer func() { _ = sqlDB.Close() }() //nolint:errcheck // reason: best-effort close on command exit

	// A tail line or a backup record that cannot be written stops the run.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	opts := titlerepair.Options{Apply: args.Yes, FileAlbum: workerFileAlbum(ctx, cfg, sqlDB)}
	libLabel := ""
	if strings.TrimSpace(args.Library) != "" {
		id, label, code, rerr := resolveEnvLibrary(ctx, out, sqlDB, args.Library)
		if rerr != nil {
			return code
		}
		opts.LibraryID, libLabel = id, label
	}
	// Every library's root, as mark-wrong: --library narrows the rows only.
	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		slog.Error("failed to list libraries", "error", err)
		return 1
	}
	if opts.Roots = usableRoots(libs); len(opts.Roots) == 0 {
		_, _ = fmt.Fprintln(out, "refused: no usable library root is configured, so no lyric file can be shown to sit inside a library")
		return 1
	}
	tailPath := args.Tail
	if tailPath == "" && args.Yes {
		// A restore needs the row dispositions, so an applied run always keeps them.
		tailPath = args.Backup + ".rows.tsv"
	}
	for _, p := range []string{args.Backup, tailPath} {
		if p == "" {
			continue
		}
		if backupPathUnsafe(p, opts.Roots) {
			_, _ = fmt.Fprintln(out, "--backup and --tail must not be a lyric file (.lrc, .txt, .elrc) or inside a library root")
			return 2
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if samePath(p, cfg.DB.Path+suffix) {
				_, _ = fmt.Fprintln(out, "--backup and --tail must not be the database file or its -wal or -shm file")
				return 2
			}
		}
	}
	const sameMsg = "--backup and --tail must be two different files"
	if tailPath != "" && samePath(tailPath, args.Backup) {
		_, _ = fmt.Fprintln(out, sameMsg)
		return 2
	}

	bk := &lyrics.LazyBackupFile{Path: args.Backup, What: "reconcile-instrumental-titles backup"}
	defer bk.Close()
	var backupErr error
	opts.Report = func(b lyricblock.Backup) error {
		f, ferr := bk.File()
		if ferr == nil {
			ferr = lyricblock.AppendBackup(f, b)
		}
		if ferr != nil && backupErr == nil {
			// A failed append can leave a torn line: no later row may append onto it.
			backupErr = ferr
			stop()
		}
		return ferr
	}
	var tail tailFile
	var tailErr error
	// lost names the row whose tail line failed: OnRow runs after the row was
	// acted on, so that row may be repaired with no line to say so.
	lost := ""
	if tailPath != "" {
		if tail, err = openTail(tailPath); err != nil {
			slog.Error("failed to open the --tail file", "error", err)
			return 1
		}
		defer func() { _ = tail.Close() }()
		// Again now that the tail exists: a link or a case variant of the backup.
		if samePath(tailPath, args.Backup) {
			_, _ = fmt.Fprintln(out, sameMsg)
			return 2
		}
		opts.OnRow = func(r titlerepair.Row) {
			// The id and a fixed word only: Row.Files are private paths.
			_, werr := fmt.Fprintf(tail, "%d\t%s\n", r.ID, r.Disposition)
			if werr == nil && args.Yes {
				werr = tail.Sync()
			}
			if werr != nil && tailErr == nil {
				tailErr, lost = werr, fmt.Sprintf("row %d (%s) has no line in the --tail file", r.ID, r.Disposition)
				stop()
			}
		}
	}

	_, _ = fmt.Fprintf(out, "%s%s\n", name, libLabel)
	if !args.Yes {
		_, _ = fmt.Fprintln(out, "dry run: nothing changed (pass --yes with --backup <file> to repair)")
	}
	_, _ = fmt.Fprintf(out, "instrumental_title.mode: %s\n", mode)
	if mode == config.InstrumentalTitleOff {
		_, _ = fmt.Fprintln(out, "the mode is off: --yes is refused until it is on")
	}
	res, runErr := titlerepair.New(sqlDB, slog.Default()).Run(ctx, opts)
	if tail != nil && tailErr == nil {
		tailErr = tail.Sync()
	}
	printTitleRepair(out, res, args.Yes)
	if args.Yes && bk.Opened() {
		_, _ = fmt.Fprintf(out, "backup: %s\n", args.Backup)
	}
	if tailPath != "" {
		_, _ = fmt.Fprintf(out, "row dispositions: %s\n", tailPath)
	}
	code := titleRepairExit(out, res)
	if backupErr != nil {
		// LazyBackupFile and AppendBackup strip the path from their errors.
		slog.Error("failed to write the --backup file", "error", backupErr)
		_, _ = fmt.Fprintln(out, "failed: the --backup file could not be written; the run was stopped")
	}
	if tailErr != nil {
		slog.Error("failed to write the --tail file", "error", tailErr)
		_, _ = fmt.Fprintln(out, "failed: the --tail file could not be written; the run was stopped")
		code = 1
	}
	if lost != "" {
		_, _ = fmt.Fprintln(out, lost)
	}
	if runErr != nil {
		// Run's error names a row id at most ("stopped after id N").
		slog.Error("reconcile-instrumental-titles did not finish", "error", runErr)
		_, _ = fmt.Fprintf(out, "not finished, the counts above are partial: %v\n", runErr)
		code = 1
	} else if args.Yes {
		_, _ = fmt.Fprintln(out, "this command read instrumental_title.mode and enrichment.enabled from its own config: keep serve running with the same values, or the reopened rows are fetched again instead of settled with a marker")
	}
	return code
}

// titleRepairExit prints the notes for the rows a run left unfinished and
// returns its exit code: 1 when a row errored or needs a manual mark.
func titleRepairExit(out io.Writer, res titlerepair.Result) int {
	code := 0
	if res.NeedsManualMark > 0 {
		_, _ = fmt.Fprintln(out, "rows needing a manual mark are left done with their files in place and their words blocked: run `scan mark-wrong --id <id> --yes` for each (ids are in the --tail file)")
		code = 1
	}
	if res.Errors > 0 {
		_, _ = fmt.Fprintln(out, "errors: a failed row may be part-way (files removed, row not reopened); a rerun reports where each row stands")
		code = 1
	}
	return code
}

// printTitleRepair prints the counts of one run, aggregate only. Strong is the
// sum of the four "strong, skipped" lines and candidates; candidates is the sum
// of every line below it down to the repaired line.
func printTitleRepair(out io.Writer, res titlerepair.Result, applied bool) {
	line := func(label string, n int) { _, _ = fmt.Fprintf(out, "%s: %d\n", label, n) }
	line("rows examined", res.Examined)
	line("no signal or excluded (a cappella, karaoke)", res.NoSignal)
	line("weak", res.Weak)
	line("strong", res.Strong)
	cells := make([]string, 0, len(res.StrongBy))
	for k, n := range res.StrongBy {
		if n > 0 {
			cells = append(cells, fmt.Sprintf("strong %s %s: %d", k.Family, k.Position, n))
		}
	}
	sort.Strings(cells)
	for _, c := range cells {
		_, _ = fmt.Fprintln(out, c)
	}
	line("strong, detector says instrumental", res.DetectorInstrumental)
	line("strong, detector says not instrumental", res.DetectorNotInstrumental)
	line("strong, never scored", res.DetectorUnscored)
	line("strong, skipped, audio file unreadable", res.Unreadable)
	line("strong, skipped, in flight", res.InFlight)
	line("strong, skipped, hand-marked", res.HandMarked)
	line("strong, skipped, artist before the dash or no artist known", res.ArtistPrefixed)
	line("candidates", res.Candidates)
	line("skipped, symlinked lyric file", res.Symlink)
	line("skipped, outside library roots", res.OutsideRoots)
	line("skipped, a file the repair would leave on the stem", res.StemOccupied)
	line("skipped, no lyric file on disk", res.NoFile)
	line("skipped, source tag names another lane", res.SourceDisagrees)
	line("skipped, not provable (file not dated at the settle)", res.NotProvable)
	line("needs a manual mark", res.NeedsManualMark)
	line("errors", res.Errors)
	if !applied {
		line("rows that would be repaired", res.WouldRepair)
		line("lyric files that would be backed up", res.BackedUp)
		return
	}
	line("rows repaired", res.Repaired)
	line("lyric files backed up", res.BackedUp)
	line("lyric files removed", res.Removed)
	line("new blocks", res.NewBlocks)
	line("cache entries invalidated", res.CacheInvalidated)
	line("rows reopened", res.Reopened)
}
