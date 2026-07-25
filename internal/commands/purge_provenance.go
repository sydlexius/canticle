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
	"github.com/sydlexius/canticle/internal/purgeprovenance"
)

// purgeProvenanceBackupRecord is one JSONL line capturing a deleted sidecar and
// the database rows reset for it, so the operation is auditable and
// hand-restorable (re-scan will re-fetch; the row keys let an operator find
// what was requeued). Written and fsynced before the sidecar it protects is
// deleted (backup-first / write-ahead).
type purgeProvenanceBackupRecord struct {
	Path          string  `json:"path"`
	ScanResultIDs []int64 `json:"scan_result_ids,omitempty"`
	WorkItemIDs   []int64 `json:"work_item_ids,omitempty"`
}

// runPurgeProvenance bulk-deletes .lrc/.txt sidecars matching a provenance
// filter (--source or --no-source) and requeues their coupled work_queue /
// scan_results rows so the next scan re-fetches (issue #474). Dry-run by
// default; --yes applies and writes a JSONL backup of every deleted sidecar,
// fsynced before its delete.
func runPurgeProvenance(ctx context.Context, out io.Writer, args ScanPurgeProvenanceCmd) int {
	hasSource := strings.TrimSpace(args.Source) != ""
	if hasSource == args.NoSource {
		_, _ = fmt.Fprintln(out, "purge-provenance: exactly one of --source or --no-source is required")
		return 2
	}

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

	libRepo := library.New(sqlDB)
	var roots []string
	var libID *int64
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, libRepo, args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		id := lib.ID
		libID = &id
		roots = []string{lib.Path}
	} else {
		libs, lerr := libRepo.List(ctx)
		if lerr != nil {
			slog.Error("failed to list libraries", "error", lerr)
			return 1
		}
		for _, l := range libs {
			roots = append(roots, l.Path)
		}
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "purge-provenance: no library roots configured")
		return 0
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("purge-provenance-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close purge-provenance backup file", "path", backupPath, "error", cerr)
			}
		}
	}()

	report := func(rec purgeprovenance.Record) error {
		if args.Yes {
			_, _ = fmt.Fprintf(out, "  deleting: %s\n", rec.Path)
		} else {
			_, _ = fmt.Fprintf(out, "  would delete: %s\n", rec.Path)
			return nil
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				return fmt.Errorf("open purge-provenance backup %q: %w", backupPath, ferr)
			}
			backupFile = f
		}
		return appendPurgeProvenanceBackup(backupFile, rec)
	}

	filter := purgeprovenance.Filter{Source: args.Source, NoSource: args.NoSource}
	res, err := purgeprovenance.New(sqlDB).Run(ctx, purgeprovenance.Options{
		Roots:     roots,
		LibraryID: libID,
		Filter:    filter,
		DryRun:    !args.Yes,
		Report:    report,
	})
	if err != nil {
		slog.Error("purge-provenance failed", "error", err)
		return 1
	}

	verb := "would delete"
	deleted := res.Matched
	if args.Yes {
		verb = "deleted"
		deleted = res.Deleted
	}
	_, _ = fmt.Fprintf(out, "purge-provenance: scanned %d sidecar(s); %s %d, requeued %d (%d scan_results reset, %d skipped in-flight, %d skipped symlink, %d errors)%s\n",
		res.Scanned, verb, deleted, res.WorkItemsRequeued, res.ScanResultsReset, res.SkippedProcessing, res.SkippedSymlink, res.Errors, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of deleted sidecars written to %s\n", backupPath)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}

// appendPurgeProvenanceBackup writes and fsyncs one JSONL record per deleted
// sidecar so the backup is durable before the delete it protects.
func appendPurgeProvenanceBackup(f *os.File, rec purgeprovenance.Record) error {
	b, err := json.Marshal(purgeProvenanceBackupRecord{
		Path:          rec.Path,
		ScanResultIDs: rec.ScanResultIDs,
		WorkItemIDs:   rec.WorkItemIDs,
	})
	if err != nil {
		return fmt.Errorf("marshal purge-provenance backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write purge-provenance backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync purge-provenance backup record: %w", err)
	}
	return nil
}
