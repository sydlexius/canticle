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

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/wordsyncrepair"
)

// runReconcileWordSync queues settled line-synced rows for a word-timing
// re-check (#982). It never fetches: a running serve worker drains the rows at
// PriorityMiss, behind fresh work. Dry-run by default and aggregate-only, so no
// path, artist or title is ever printed.
func runReconcileWordSync(ctx context.Context, out io.Writer, args ScanReconcileWordSyncCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	// Refused before the database is opened: under off the writer lands no word
	// timings (and removes an owned .elrc), so a recheck would buy nothing and
	// stamp served on files without markers.
	if cfg.Output.WordSyncMode == config.WordSyncModeOff {
		_, _ = fmt.Fprintln(out, "reconcile-word-sync: output.word_sync_mode is off, so a re-check could not write any word timings; set it to both or replace first")
		return 1
	}
	gen, err := configuredWordGeneration(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(out, "reconcile-word-sync: %v\n", err)
		return 1
	}
	opts := queue.WordRecheckOptions{Generation: gen, Limit: args.Limit}
	if args.Limit < 0 {
		_, _ = fmt.Fprintln(out, "--limit must not be negative")
		return 1
	}
	if args.CompletedBefore != "" {
		if opts.CompletedBefore, err = parseCutoff("--completed-before", args.CompletedBefore); err != nil {
			_, _ = fmt.Fprintln(out, err)
			return 1
		}
	}
	if args.RecheckAbsentBefore != "" {
		if opts.RecheckAbsentBefore, err = parseCutoff("--recheck-absent-before", args.RecheckAbsentBefore); err != nil {
			_, _ = fmt.Fprintln(out, err)
			return 1
		}
	}

	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, args.Yes)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // reason: best-effort close on shutdown

	libs := library.New(sqlDB)
	for _, ref := range args.Libraries {
		lib, rerr := resolveLibrary(ctx, libs, ref)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", ref)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		opts.LibraryIDs = append(opts.LibraryIDs, lib.ID)
	}

	q := queue.NewDBQueue(sqlDB)
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-word-sync-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	// The seams are read at call time so a test can swap them per run.
	backup := &wordsyncrepair.FileBackup{
		Path:     backupPath,
		Write:    func(f *os.File, p queue.WordRecheckPrior) error { return writeWordSyncBackup(f, p) },
		SyncFile: func(f *os.File) error { return syncWordSyncBackup(f) },
	}
	defer func() {
		if cerr := backup.Close(); cerr != nil {
			slog.Warn("failed to close reconcile-word-sync backup file", "path", backupPath, "error", cerr)
		}
	}()
	runOpts := wordsyncrepair.Options{Apply: args.Yes, Query: opts}
	if args.Yes {
		runOpts.Backup = backup
	}
	res, err := wordsyncrepair.Run(ctx, q, runOpts)
	if err != nil {
		var se *wordsyncrepair.StageError
		isStage := errors.As(err, &se)
		ctxStop := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		switch {
		// Only a flip failure, or a stop between batches (no stage error), has a
		// rolled-back batch to report. A cancel that surfaces from a count or
		// list stage falls through to the "<stage> failed" case, as on main.
		case isStage && se.Stage == "flip", !isStage && ctxStop:
			_, _ = fmt.Fprintf(out, "reconcile-word-sync: stopped after queuing %d row(s); the failed batch was rolled back\n", res.Queued)
			if backup.Opened() {
				_, _ = fmt.Fprintf(out, "backup %s: its last %d record(s) belong to the rolled-back batch; do not restore them\n", backupPath, res.Written-res.Queued)
			}
			if se != nil {
				err = se.Err
			}
			slog.Error("reconcile-word-sync: flip failed", "error", err)
		case se != nil:
			slog.Error("reconcile-word-sync: "+se.Stage+" failed", "error", se.Err)
		default:
			slog.Error("reconcile-word-sync: run failed", "error", err)
		}
		return 1
	}
	candidates, already, selected, queued := res.Candidates, res.AlreadyQueued, res.Selected, res.Queued

	// A floor: the worker is serial at serve's poll interval (config tiers only;
	// a serve --work-interval flag is invisible here), behind fresh work.
	interval := max(serveWorkerInterval(cfg, ServeCmd{}), 0)
	drain := time.Duration(selected) * interval
	_, _ = fmt.Fprintf(out, "reconcile-word-sync: candidates=%d selected=%d already-queued=%d estimated-minimum-drain=%s (at worker interval=%s)",
		candidates, selected, already, drain, interval)
	if args.Yes {
		_, _ = fmt.Fprintf(out, " queued=%d\n", queued)
	} else {
		_, _ = fmt.Fprintf(out, "%s\n", suffixDryRun(false))
	}
	_, _ = fmt.Fprintln(out, "reconcile-word-sync: this command fetches nothing; a running `canticle serve` worker drains queued rows behind fresh work")
	if backup.Opened() {
		_, _ = fmt.Fprintf(out, "backup of queued rows' prior state written to %s\n", backupPath)
	}
	return 0
}

// configuredWordGeneration names serve's lanes through the SAME
// selectedProvider/fallbackProviders calls runServe makes (only names are read,
// no client is used), so the CLI judges "absent" verdicts against the generation
// the worker stamps. A mismatch would read every current verdict as stale. An
// empty env/TOML token is assumed present: serve reads a stored token or mints
// one (#554), neither of which the CLI can see.
func configuredWordGeneration(cfg config.Config) (int64, error) {
	token := cfg.API.Token
	if strings.TrimSpace(token) == "" {
		token = "assumed-present"
	}
	nameOnly := func(string) musixmatch.Fetcher { return noopFetcher{} }
	primary, err := selectedProvider(cfg, token, nameOnly)
	if err != nil {
		return 0, err
	}
	names := []string{primary.Name()}
	for _, p := range fallbackProviders(cfg, token, primary.Name(), nameOnly) {
		names = append(names, p.Name())
	}
	return providers.WordGeneration(names), nil
}

// writeWordSyncBackup and syncWordSyncBackup are seams so a test can fail a
// record or the batch's fsync.
var (
	writeWordSyncBackup = func(f *os.File, p queue.WordRecheckPrior) error { return wordsyncrepair.AppendRecord(f, p) }
	syncWordSyncBackup  = func(f *os.File) error { return f.Sync() }
)
