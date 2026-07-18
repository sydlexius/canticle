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
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/prune"
)

// reconcilePathsBackupRecord is one JSONL line capturing the rows pruned for a
// vanished source path so the operation is auditable and hand-restorable
// (re-enqueue Inputs, re-scan). Written before the delete commits.
type reconcilePathsBackupRecord struct {
	SourcePath    string          `json:"source_path"`
	ScanResultIDs []int64         `json:"scan_result_ids,omitempty"`
	WorkItemIDs   []int64         `json:"work_item_ids,omitempty"`
	Inputs        []models.Inputs `json:"inputs,omitempty"`
}

// runReconcilePaths reconciles the durable queue and scan-result cache against
// the filesystem: rows whose source audio file no longer exists are deleted so a
// renamed/merged/deleted track cannot leave a permanently-failing or wedged row
// behind (#453). It runs at Exact granularity (every source_path is statted
// individually), so single-file renames within a surviving directory are caught,
// unlike the disk-cheap periodic sweep. Dry-run by default; --yes applies and
// writes a JSONL backup.
func runReconcilePaths(ctx context.Context, out io.Writer, args ScanReconcilePathsCmd) int {
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

	var libID *int64
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, library.New(sqlDB), args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		libID = &lib.ID
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-paths-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-paths backup file", "path", backupPath, "error", cerr)
			}
		}
	}()
	// report is invoked once per pruned source before its delete commits. In
	// dry-run it is a no-op (nothing is deleted, so nothing needs backing up);
	// under --yes it lazily opens the backup and appends a restorable record.
	report := func(row prune.PrunedRow) error {
		if !args.Yes {
			return nil
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				return fmt.Errorf("open reconcile-paths backup %q: %w", backupPath, ferr)
			}
			backupFile = f
		}
		return appendReconcilePathsBackup(backupFile, row)
	}

	res, err := prune.New(sqlDB).Sweep(ctx, prune.SweepOptions{
		LibraryID:   libID,
		Granularity: prune.Exact,
		DryRun:      !args.Yes,
		Report:      report,
	})
	if err != nil {
		slog.Error("reconcile-paths failed", "error", err)
		return 1
	}

	verb := "would prune"
	if args.Yes {
		verb = "pruned"
	}
	_, _ = fmt.Fprintf(out, "reconcile-paths: %s %d source(s) with a vanished file (%d scan_results, %d work_items)%s\n",
		verb, len(res.Pruned), res.ScanResults, res.WorkItems, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of pruned rows written to %s\n", backupPath)
	}
	return 0
}

// appendReconcilePathsBackup writes one JSONL record for a pruned source and
// flushes it to disk, so the backup is durable before the delete it protects.
func appendReconcilePathsBackup(f *os.File, row prune.PrunedRow) error {
	rec := reconcilePathsBackupRecord{
		SourcePath:    row.SourcePath,
		ScanResultIDs: row.ScanResultIDs,
		WorkItemIDs:   row.WorkItemIDs,
		Inputs:        row.Inputs,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal reconcile-paths backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-paths backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-paths backup record: %w", err)
	}
	return nil
}
