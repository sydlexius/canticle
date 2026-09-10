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

// reconcilePathsBackupRecord is one JSONL line capturing the outcome for one
// source path -- pruned, relinked, retained, or repaired -- so the operation
// is auditable and hand-restorable (re-enqueue Inputs, re-scan, undo a relink
// by reversing OldPath/NewPath, or undo a repair by restoring OldOutputPaths).
// Written before the corresponding mutation commits (backup-first); a
// retained row is never a mutation, so its record is purely informational.
type reconcilePathsBackupRecord struct {
	Action        string          `json:"action"` // "pruned", "relinked", "retained", or "repaired"
	SourcePath    string          `json:"source_path"`
	ScanResultIDs []int64         `json:"scan_result_ids,omitempty"`
	WorkItemIDs   []int64         `json:"work_item_ids,omitempty"`
	Inputs        []models.Inputs `json:"inputs,omitempty"`
	// NewPath, MBID, ISRC, and Reason are populated for "relinked"/"retained"
	// records; empty for "pruned".
	NewPath string `json:"new_path,omitempty"`
	MBID    string `json:"mbid,omitempty"`
	ISRC    string `json:"isrc,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// WorkItemID, OldOutputPaths, and NewOutputPaths are populated for
	// "repaired" records only (issue #921): the id of the work_queue row whose
	// output_paths JSON was rewritten, plus its value before and after, so an
	// operator can hand-restore the prior value if the repair guessed wrong.
	// SourcePath is left empty for a "repaired" record -- the repair pass
	// selects candidates by outdir/output_paths, never source_path, so the row
	// is identified by WorkItemID instead.
	WorkItemID     int64               `json:"work_item_id,omitempty"`
	OldOutputPaths []models.OutputPath `json:"old_output_paths,omitempty"`
	NewOutputPaths []models.OutputPath `json:"new_output_paths,omitempty"`
}

// runReconcilePaths reconciles the durable queue and scan-result cache against
// the filesystem. A row whose source audio file cannot be found is no longer
// deleted unconditionally (#640): its stored MBID/ISRC is first checked
// against every other file present in the library via the shared
// internal/identity resolver -- realign's same exact tier, so a bulk move can
// never leave a sidecar and its database row disagreeing about where a file
// went. A unique match RE-LINKS the row to its new location, preserving every
// telemetry/timing/provenance column; identity that is absent or ambiguous is
// RETAINED and reported, never guessed at. Only identity that is present but
// matches nothing anywhere is genuinely PRUNED, so the table does not grow
// without bound. Runs at Exact granularity (every source_path is statted
// individually), so single-file renames within a surviving directory are
// caught, unlike the disk-cheap periodic sweep. Dry-run by default; --yes
// applies and writes a JSONL backup covering every outcome.
//
// After the sweep, a SEPARATE repair pass (issue #921) fixes a row a relink
// already broke before internal/prune.relinkOne started keeping output_paths
// in sync with a relinked outdir/filename: such a row's output_paths still
// names the pre-move directory, so the worker keeps writing to a vanished
// destination and re-fetching forever, and it cannot be caught by the sweep
// above because its source_path already resolves (a prior relink already
// moved it) -- it never looks "gone". See prune.RepairOutputPaths.
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
	openBackup := func() error {
		if backupFile != nil {
			return nil
		}
		f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
		if ferr != nil {
			return fmt.Errorf("open reconcile-paths backup %q: %w", backupPath, ferr)
		}
		backupFile = f
		return nil
	}
	// Each report hook fires once per outcome, before its mutation commits (or,
	// in dry-run, for the outcome that would occur). Under --yes it lazily opens
	// the shared backup file and appends a restorable record; in dry-run
	// nothing is mutated, so nothing needs backing up.
	reportPruned := func(row prune.PrunedRow) error {
		if !args.Yes {
			return nil
		}
		if err := openBackup(); err != nil {
			return err
		}
		return appendReconcilePathsBackup(backupFile, reconcilePathsBackupRecord{
			Action:        "pruned",
			SourcePath:    row.SourcePath,
			ScanResultIDs: row.ScanResultIDs,
			WorkItemIDs:   row.WorkItemIDs,
			Inputs:        row.Inputs,
		})
	}
	reportRelinked := func(row prune.RelinkedRow) error {
		if !args.Yes {
			return nil
		}
		if err := openBackup(); err != nil {
			return err
		}
		return appendReconcilePathsBackup(backupFile, reconcilePathsBackupRecord{
			Action:        "relinked",
			SourcePath:    row.OldPath,
			NewPath:       row.NewPath,
			ScanResultIDs: row.ScanResultIDs,
			WorkItemIDs:   row.WorkItemIDs,
			MBID:          row.MBID,
			ISRC:          row.ISRC,
		})
	}
	// Retaining never mutates the database, so this record is purely
	// informational and is written the same way in dry-run and real runs --
	// gated on args.Yes anyway, matching the other two hooks, so a dry-run
	// preview never writes a backup file at all.
	reportRetained := func(row prune.RetainedRow) error {
		if !args.Yes {
			return nil
		}
		if err := openBackup(); err != nil {
			return err
		}
		return appendReconcilePathsBackup(backupFile, reconcilePathsBackupRecord{
			Action:     "retained",
			SourcePath: row.SourcePath,
			MBID:       row.MBID,
			ISRC:       row.ISRC,
			Reason:     row.Reason,
		})
	}
	// The repair pass (#921) calls this inside each row's transaction, before
	// COMMIT, so a failed append rolls that row back (backup-first). A dry-run
	// preview still calls it but writes nothing.
	reportRepaired := func(row prune.RepairedRow) error {
		if !args.Yes {
			return nil
		}
		if err := openBackup(); err != nil {
			return err
		}
		return appendReconcilePathsBackup(backupFile, reconcilePathsBackupRecord{
			Action:         "repaired",
			WorkItemID:     row.WorkItemID,
			OldOutputPaths: row.OldOutputPaths,
			NewOutputPaths: row.NewOutputPaths,
		})
	}

	pruner := prune.New(sqlDB)
	pruner.SetIdentityKeys(cfg.Realign.IdentityKeys)
	pruner.SetNameMatchThresholds(cfg.Realign.MinConfidence, cfg.Realign.MinMargin)
	res, err := pruner.Sweep(ctx, prune.SweepOptions{
		LibraryID:      libID,
		Granularity:    prune.Exact,
		DryRun:         !args.Yes,
		Report:         reportPruned,
		ReportRelinked: reportRelinked,
		ReportRetained: reportRetained,
	})
	if err != nil {
		slog.Error("reconcile-paths failed", "error", err)
		return 1
	}

	// The output_paths repair pass (#921) runs AFTER the sweep, on its own
	// row population: rows whose output_paths names a directory that no
	// longer exists while the row's own outdir names one that does. This is
	// deliberately disjoint from the sweep above -- a repair candidate's
	// source_path already resolves (a prior relink already moved it), so it
	// never appears as "gone" to Sweep's gather and the two passes never
	// double-count or race the same row.
	repairRes, err := pruner.RepairOutputPaths(ctx, prune.RepairOptions{
		LibraryID: libID,
		DryRun:    !args.Yes,
		Report:    reportRepaired,
	})
	if err != nil {
		slog.Error("reconcile-paths output_paths repair failed", "error", err)
		return 1
	}

	verb := "would prune"
	relinkVerb := "would relink"
	repairVerb := "would repair"
	if args.Yes {
		verb = "pruned"
		relinkVerb = "relinked"
		repairVerb = "repaired"
	}
	_, _ = fmt.Fprintf(out, "reconcile-paths: %s %d source(s) with a vanished file (%d scan_results, %d work_items), %s %d source(s) to a moved file, retained %d source(s) with unresolved identity%s\n",
		verb, len(res.Pruned), res.ScanResults, res.WorkItems, relinkVerb, len(res.Relinked), len(res.Retained), suffixDryRun(args.Yes))
	_, _ = fmt.Fprintf(out, "reconcile-paths: %s %d work_queue row(s) with a stale output_paths destination (skipped: %d ambiguous, %d unfixable, %d stat error, %d malformed, %d raced)%s\n",
		repairVerb, len(repairRes.Repaired), repairRes.SkippedAmbiguous, repairRes.SkippedUnfixable,
		repairRes.SkippedStatError, repairRes.SkippedMalformed, repairRes.SkippedRaced, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of pruned/relinked/retained/repaired rows written to %s\n", backupPath)
	}
	return 0
}

// appendReconcilePathsBackup writes one JSONL record for an outcome and
// flushes it to disk, so the backup is durable before the mutation it
// protects (a retained record protects nothing, but is written the same way
// for a single consistent format).
func appendReconcilePathsBackup(f *os.File, rec reconcilePathsBackupRecord) error {
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
