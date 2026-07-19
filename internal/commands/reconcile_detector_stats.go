package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
)

// appendReconcileDetectorStatsBackup writes one attributed row as a JSONL
// record. The record is the lane_attempts row that was inserted; the backfill
// only ever inserts (ON CONFLICT DO NOTHING), so there is no pre-image to
// preserve.
//
// RESTORE HAZARD: deleting these (queue_id, lane) pairs undoes the backfill only
// while the rows are still the ones it wrote. queue.RecordLaneAttempts upserts
// with DO UPDATE, so once the worker runs again it may overwrite a backfilled
// row IN PLACE with an authoritative live outcome. A delete after that point
// destroys live-recorded history, which this backfill otherwise treats as
// inviolable. Restore promptly, or reconcile against attempted_at first.
func appendReconcileDetectorStatsBackup(f *os.File, ch detectorbackfill.Change) error {
	b, err := json.Marshal(ch)
	if err != nil {
		return fmt.Errorf("marshal detector-stats backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write detector-stats backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync detector-stats backup: %w", err)
	}
	return nil
}

// runReconcileDetectorStats attributes historical audio detections to the
// detector lane in lane_attempts, correcting an under-count left by instrumentals
// that settled before the detector became a lane (issue #537). It is driven
// entirely from work_queue.instrumental_result, never by re-running detection.
// Dry-run by default; --yes applies and writes a JSONL backup of every row
// written.
func runReconcileDetectorStats(ctx context.Context, out io.Writer, args ScanReconcileDetectorStatsCmd) int {
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
	defer sqlDB.Close() //nolint:errcheck // reason: best-effort close on shutdown

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-detector-stats-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-detector-stats backup file", "path", backupPath, "error", cerr)
			}
		}
	}()
	// report is invoked once per attributed row. In dry-run it previews the
	// attribution; under --yes it appends a restorable backup record from inside
	// the backfill's transaction, before commit.
	report := func(ch detectorbackfill.Change) error {
		verdict := "miss"
		if ch.Hit {
			verdict = "hit"
		}
		_, _ = fmt.Fprintf(out, "  queue row %d -> %s %s (attempted_at %s)\n", ch.QueueID, ch.Lane, verdict, ch.AttemptedAt)
		if !args.Yes {
			return nil
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				return fmt.Errorf("open reconcile-detector-stats backup %q: %w", backupPath, ferr)
			}
			backupFile = f
		}
		return appendReconcileDetectorStatsBackup(backupFile, ch)
	}

	res, err := detectorbackfill.New(sqlDB).Run(ctx, detectorbackfill.Options{
		DryRun: !args.Yes,
		Report: report,
	})
	if err != nil {
		slog.Error("reconcile-detector-stats failed", "error", err)
		return 1
	}

	verb := "would attribute"
	if args.Yes {
		verb = "attributed"
	}
	_, _ = fmt.Fprintf(out, "reconcile-detector-stats: scanned %d row(s); %s %d (%d hits, %d misses, %d already recorded)%s\n",
		res.Scanned, verb, res.Hits+res.Misses, res.Hits, res.Misses, res.AlreadyRecorded, suffixDryRun(args.Yes))
	// Both remainders are stated explicitly. The NULL tally is countable; the
	// ClearDone tail is not visible from work_queue at all, so reporting only the
	// former would imply a completeness the backfill cannot claim.
	_, _ = fmt.Fprintf(out, "uncovered: %d row(s) have no recorded detection verdict and are left untouched (not estimated)\n", res.UncoveredNull)
	_, _ = fmt.Fprintln(out, "uncovered: detections on work_queue rows already removed by ClearDone cannot be recovered or counted")
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of attributed rows written to %s\n", backupPath)
	}
	return 0
}
