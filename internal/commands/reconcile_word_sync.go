package commands

import (
	"context"
	"database/sql"
	"encoding/json"
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
)

// wordSyncFlipBatch bounds one flip transaction, which holds the writer lock,
// so a library-sized run in one transaction would stall a live serve.
const wordSyncFlipBatch = 500

// wordSyncBackupRecord is one JSONL line: a flipped row's prior value of every
// column the flip writes (nil = NULL), so writing it back restores the row.
type wordSyncBackupRecord struct {
	ID                   int64   `json:"id"`
	Status               string  `json:"status"`
	Priority             int     `json:"priority"`
	NextAttemptAt        string  `json:"next_attempt_at"`
	Attempts             int     `json:"attempts"`
	LastError            string  `json:"last_error"`
	RefusedWaits         int     `json:"refused_waits"`
	WordTimingState      *string `json:"word_timing_state"`
	WordTimingGeneration *int64  `json:"word_timing_generation"`
}

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
		_, _ = fmt.Fprintln(out, "reconcile-word-sync: output.word_sync_mode is off, so a re-check could not write any word timings; set it to sidecar, inline or both first")
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
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

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
	candidates, err := q.CountWordRecheckCandidates(ctx, opts)
	if err != nil {
		slog.Error("reconcile-word-sync: count candidates failed", "error", err)
		return 1
	}
	already, err := q.CountWordRecheckQueued(ctx, opts.LibraryIDs)
	if err != nil {
		slog.Error("reconcile-word-sync: count queued rows failed", "error", err)
		return 1
	}
	selected := candidates
	if args.Limit > 0 && args.Limit < selected {
		selected = args.Limit
	}

	queued, written := 0, 0
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-word-sync-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-word-sync backup file", "path", backupPath, "error", cerr)
			}
		}
	}()
	if args.Yes && selected > 0 {
		// report runs INSIDE the flip transaction, before that row's UPDATE. It
		// must not touch the database: the handle has one connection, which the
		// flip's own transaction holds, so a query here would deadlock.
		report := func(p queue.WordRecheckPrior) error {
			if backupFile == nil {
				f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
				if ferr != nil {
					return fmt.Errorf("open reconcile-word-sync backup %q: %w", backupPath, ferr)
				}
				backupFile = f
			}
			if err := writeWordSyncBackup(backupFile, p); err != nil {
				return err
			}
			written++
			return nil
		}
		// One fsync per batch, before its COMMIT (write-ahead): every record of
		// the batch is durable before any of its rows flips.
		opts.BeforeCommit = func() error { return syncWordSyncBackup(backupFile) }
		ids, lerr := q.ListWordRecheckCandidates(ctx, opts)
		if lerr != nil {
			slog.Error("reconcile-word-sync: list candidates failed", "error", lerr)
			return 1
		}
		for start := 0; start < len(ids); start += wordSyncFlipBatch {
			end := min(start+wordSyncFlipBatch, len(ids))
			prior, ferr := q.MarkWordRecheckQueued(ctx, ids[start:end], opts, report)
			if ferr != nil {
				_, _ = fmt.Fprintf(out, "reconcile-word-sync: stopped after queuing %d row(s); the failed batch was rolled back\n", queued)
				if backupFile != nil {
					_, _ = fmt.Fprintf(out, "backup %s: its last %d record(s) belong to the rolled-back batch; do not restore them\n", backupPath, written-queued)
				}
				slog.Error("reconcile-word-sync: flip failed", "error", ferr)
				return 1
			}
			queued += len(prior)
		}
	}

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
	if backupFile != nil {
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
	writeWordSyncBackup = appendWordSyncBackup
	syncWordSyncBackup  = func(f *os.File) error { return f.Sync() }
)

// appendWordSyncBackup writes one record inside the flip transaction; the
// batch's last record also fsyncs (write-ahead), and any failure rolls it back.
func appendWordSyncBackup(f *os.File, p queue.WordRecheckPrior) error {
	b, err := json.Marshal(wordSyncBackupRecord{
		ID: p.ID, Status: p.Status, Priority: p.Priority, NextAttemptAt: p.NextAttemptAt,
		Attempts: p.Attempts, LastError: p.LastError, RefusedWaits: p.RefusedWaits,
		WordTimingState: p.WordTimingState, WordTimingGeneration: p.WordTimingGeneration,
	})
	if err != nil {
		return fmt.Errorf("marshal reconcile-word-sync backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-word-sync backup record: %w", err)
	}
	return nil
}
