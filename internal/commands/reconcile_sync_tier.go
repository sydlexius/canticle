package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tierrepair"
)

// runReconcileSyncTier is the one-time backfill for #1075: classifies every
// completed synced row's sidecar (word/line/unsynced, ClassifyLRCFile) into
// work_queue.sync_tier. Dry-run by default; --yes applies and backs up.
// Aggregate-only stdout, matching the other `scan reconcile-*` commands.
func runReconcileSyncTier(ctx context.Context, out io.Writer, args ScanReconcileSyncTierCmd) int {
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
	defer sqlDB.Close() //nolint:errcheck // reason: best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)
	candidates, err := tierrepair.ListSyncTier(ctx, q)
	if err != nil {
		slog.Error("reconcile-sync-tier: list candidates failed", "error", err)
		return 1
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-sync-tier-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	return runReconcileSyncTierCore(ctx, out, q, candidates, args.Yes, backupPath)
}

// runReconcileSyncTierCore stamps each candidate through q (#1087 review).
// Backup-first: the file opens before the first guarded write (a bad
// --backup path stops the run with zero DB writes), and each record is
// written inside SetSyncTierIfPending's own transaction before that stamp
// commits, so a raced row never gets a backup call at all. A write failure
// counts separately from a classification failure (write_failed, never
// unreadable) and fails the exit code without stopping the remaining rows.
func runReconcileSyncTierCore(ctx context.Context, out io.Writer, q tierrepair.SyncTierQueue, candidates []queue.SyncTierCandidate, yes bool, backupPath string) int {
	opts := tierrepair.Options{Apply: yes}
	if yes {
		f, ferr := tierrepair.OpenBackup(backupPath)
		if ferr != nil {
			slog.Error("reconcile-sync-tier: open backup file failed", "path", backupPath, "error", ferr)
			return 1
		}
		defer func() {
			if cerr := f.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-sync-tier backup file", "path", backupPath, "error", cerr)
			}
		}()
		opts.Backup = f
	}

	res, err := tierrepair.RunSyncTier(ctx, q, candidates, opts)
	// A canceled run stops at a row boundary with every changed row backed
	// up, so it still prints the partial counts and the backup path (the
	// operator needs them to restore) and then exits 1, silent on stderr.
	canceled := err != nil && errors.Is(err, context.Canceled)
	if err != nil && !canceled {
		slog.Error("reconcile-sync-tier: run stopped", "error", err)
		return 1
	}
	counts, writeFailed, stamped := res.Counts, res.WriteFailed, res.Stamped

	verb := "would classify"
	if yes {
		verb = "classified"
	}
	_, _ = fmt.Fprintf(out, "reconcile-sync-tier: scanned %d row(s); %s %d as word, %d as line, %d as unsynced (no_audio=%d no_sidecar=%d unreadable=%d raced=%d write_failed=%d)%s\n",
		res.Scanned, verb, counts[queue.SyncTierWord], counts[queue.SyncTierLine], counts[queue.SyncTierUnsynced],
		counts["no_audio"], counts["no_sidecar"], counts["errored"], counts["raced"], writeFailed, suffixDryRun(yes))
	// Only when >=1 record landed, so an empty eagerly-opened file (every
	// candidate raced) never reads as "backup written".
	if stamped > 0 {
		_, _ = fmt.Fprintf(out, "backup of classified rows written to %s\n", backupPath)
	}
	if canceled || writeFailed > 0 {
		return 1
	}
	return 0
}
