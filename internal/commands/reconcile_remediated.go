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

// runReconcileRemediated is the one-time backfill for #1143: re-describes
// synced rows the dashboard counts as "tier unknown" from what is actually on
// disk (no sidecar: reset for re-fetch; .txt only: unsynced; .lrc present:
// record its tier). Dry-run by default; --yes applies, backup first.
// Aggregate-only stdout. No SQLITE_BUSY retry: a retry would write the backup
// record twice, so a busy row counts as write_failed and a rerun is safe (the
// apply guards make it idempotent).
func runReconcileRemediated(ctx context.Context, out io.Writer, args ScanReconcileRemediatedCmd) int {
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
	candidates, err := tierrepair.ListRemediated(ctx, q)
	if err != nil {
		slog.Error("reconcile-remediated: list candidates failed", "error", err)
		return 1
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-remediated-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}

	opts := tierrepair.Options{Apply: args.Yes}
	if args.Yes {
		f, ferr := tierrepair.OpenBackup(backupPath)
		if ferr != nil {
			slog.Error("reconcile-remediated: open backup file failed", "error", ferr)
			return 1
		}
		defer func() { _ = f.Close() }()
		opts.Backup = f
	}

	res, err := tierrepair.RunRemediated(ctx, q, candidates, opts)
	// A canceled run stops at a row boundary with every changed row backed
	// up, so it still prints the partial counts and the backup path (the
	// operator needs them to restore) and then exits 1, silent on stderr.
	canceled := err != nil && errors.Is(err, context.Canceled)
	if err != nil && !canceled {
		slog.Error("reconcile-remediated: run stopped", "error", err)
		return 1
	}
	counts, writeFailed, applied := res.Counts, res.WriteFailed, res.Applied

	verb := "would"
	if args.Yes {
		verb = "did"
	}
	// "scanned" is the candidate superset (in-flight rows included), not the
	// dashboard's tier-unknown tile.
	_, _ = fmt.Fprintf(out, "reconcile-remediated: scanned %d candidate row(s) (a superset of the dashboard tier-unknown count); %s reset %d for re-fetch, %s re-describe %d as unsynced, %s record tier for %d%s\n",
		res.Scanned, verb, counts[queue.RemediatedReset], verb, counts[queue.RemediatedUnsynced], verb, counts[queue.RemediatedTier], suffixDryRun(args.Yes))
	_, _ = fmt.Fprintf(out, "skipped: already_recorded=%d kept_remediation_verdict=%d unsynced_lrc=%d retired=%d processing=%d word_recheck=%d upgrade_armed=%d no_audio=%d audio_gone=%d unreadable=%d raced=%d write_failed=%d\n",
		counts[tierrepair.OutcomeAlreadyRecorded], counts[tierrepair.OutcomeKeptVerdict], counts[tierrepair.OutcomeUnsyncedLRC], counts["retired"], counts["processing"], counts["word_recheck"],
		counts["upgrade_armed"], counts["no_audio"], counts["audio_gone"], counts["errored"], counts["raced"], writeFailed)
	if applied > 0 {
		_, _ = fmt.Fprintf(out, "backup of %d changed row(s) written to %s\n", applied, backupPath)
	}
	if canceled || writeFailed > 0 {
		return 1
	}
	return 0
}
