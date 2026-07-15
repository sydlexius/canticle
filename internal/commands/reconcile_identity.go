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

	"github.com/doxazo-net/canticle/internal/config"
	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/identityrepair"
	"github.com/doxazo-net/canticle/internal/library"
	"github.com/doxazo-net/canticle/internal/scanner"
)

// reconcileIdentityBackupRecord is one JSONL line capturing a corrected row's
// before/after identity so the operation is auditable and hand-reversible.
type reconcileIdentityBackupRecord struct {
	ScanResultID   int64  `json:"scan_result_id"`
	LibraryID      int64  `json:"library_id"`
	FilePath       string `json:"file_path"`
	OldArtist      string `json:"old_artist"`
	NewArtist      string `json:"new_artist"`
	OldAlbumArtist string `json:"old_album_artist,omitempty"`
	NewAlbumArtist string `json:"new_album_artist,omitempty"`
	OldArtistKey   string `json:"old_artist_key"`
	NewArtistKey   string `json:"new_artist_key"`
}

// runReconcileIdentity re-reads each scan_results row's file tags and corrects
// the stored artist / album-artist (and the coupled work_queue row) where a
// multi-value ID3v2.4 artist frame was ingested run-together before the fix
// (issue #466). The lost value boundaries cannot be recovered from the database,
// so every row's file is re-read from disk. Dry-run by default; --yes applies
// and writes a JSONL backup of every corrected row.
func runReconcileIdentity(ctx context.Context, out io.Writer, args ScanReconcileIdentityCmd) int {
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
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-identity-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-identity backup file", "path", backupPath, "error", cerr)
			}
		}
	}()
	// report is invoked once per corrected row. In dry-run it prints the planned
	// change; under --yes it also appends a restorable backup record.
	report := func(ch identityrepair.Change) error {
		_, _ = fmt.Fprintf(out, "  %s\n    %q -> %q\n", ch.FilePath, ch.OldArtist, ch.NewArtist)
		if !args.Yes {
			return nil
		}
		if backupFile == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				return fmt.Errorf("open reconcile-identity backup %q: %w", backupPath, ferr)
			}
			backupFile = f
		}
		return appendReconcileIdentityBackup(backupFile, ch)
	}

	res, err := identityrepair.New(sqlDB, scanner.ReadArtistIdentity).Run(ctx, identityrepair.Options{
		LibraryID: libID,
		DryRun:    !args.Yes,
		Report:    report,
	})
	if err != nil {
		slog.Error("reconcile-identity failed", "error", err)
		return 1
	}

	verb := "would correct"
	if args.Yes {
		verb = "corrected"
	}
	_, _ = fmt.Fprintf(out, "reconcile-identity: scanned %d row(s); %s %d (%d queue re-keyed, %d queue merged, %d skipped in-flight, %d unreadable)%s\n",
		res.Scanned, verb, res.Changed, res.QueueUpdated, res.QueueMerged, res.ProcessingSkips, res.ReadFailures, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of corrected rows written to %s\n", backupPath)
	}
	return 0
}

// identityBackfillMarker names the one-shot serve-mode backfill in
// maintenance_markers (migration 027). Its presence means the pass has already
// run against this database and is skipped on subsequent startups.
const identityBackfillMarker = "identity_backfill_466"

// runIdentityBackfill runs the identity-repair pass once per database in
// serve mode: if the marker is absent, it re-reads every scan_results file's
// tags, corrects run-together multi-value artists ingested before the fix
// (#466), and stamps the marker so later startups skip the expensive re-read.
// It is best-effort and non-fatal -- any failure is logged and the marker is
// left unset so the next startup (or the CLI) retries; a canceled context
// (shutdown) simply stops it, also leaving the marker unset. The pass is
// idempotent, so a partial run followed by a retry is safe.
func runIdentityBackfill(ctx context.Context, sqlDB *sql.DB) {
	done, err := identityBackfillDone(ctx, sqlDB)
	if err != nil {
		slog.Warn("identity backfill: marker check failed; skipping this startup", "error", err)
		return
	}
	if done {
		return
	}

	slog.Info("identity backfill: correcting run-together multi-value artist rows (#466); this re-reads file tags and runs once")
	res, err := identityrepair.New(sqlDB, scanner.ReadArtistIdentity).Run(ctx, identityrepair.Options{
		Progress: func(scanned int) {
			slog.Debug("identity backfill: progress", "scanned", scanned)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("identity backfill: interrupted by shutdown; will resume on next startup",
				"scanned", res.Scanned, "corrected", res.Changed)
			return
		}
		slog.Error("identity backfill: failed; will retry on next startup", "error", err,
			"scanned", res.Scanned, "corrected", res.Changed)
		return
	}

	slog.Info("identity backfill: complete",
		"scanned", res.Scanned, "corrected", res.Changed,
		"queue_rekeyed", res.QueueUpdated, "queue_merged", res.QueueMerged,
		"skipped_in_flight", res.ProcessingSkips, "unreadable", res.ReadFailures)

	if err := markIdentityBackfillDone(ctx, sqlDB); err != nil {
		slog.Warn("identity backfill: completed but failed to record marker; it may re-run next startup", "error", err)
	}
}

// identityBackfillDone reports whether the one-shot backfill marker is present.
func identityBackfillDone(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	var one int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT 1 FROM maintenance_markers WHERE name = ?`, identityBackfillMarker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query maintenance marker %q: %w", identityBackfillMarker, err)
	}
	return true, nil
}

// markIdentityBackfillDone records the marker so the pass is skipped hereafter.
func markIdentityBackfillDone(ctx context.Context, sqlDB *sql.DB) error {
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO maintenance_markers (name) VALUES (?)`, identityBackfillMarker); err != nil {
		return fmt.Errorf("record maintenance marker %q: %w", identityBackfillMarker, err)
	}
	return nil
}

// appendReconcileIdentityBackup writes one JSONL record for a corrected row and
// flushes it, so the backup is durable before the correction it protects.
func appendReconcileIdentityBackup(f *os.File, ch identityrepair.Change) error {
	rec := reconcileIdentityBackupRecord{
		ScanResultID:   ch.ScanResultID,
		LibraryID:      ch.LibraryID,
		FilePath:       ch.FilePath,
		OldArtist:      ch.OldArtist,
		NewArtist:      ch.NewArtist,
		OldAlbumArtist: ch.OldAlbumArtist,
		NewAlbumArtist: ch.NewAlbumArtist,
		OldArtistKey:   ch.OldArtistKey,
		NewArtistKey:   ch.NewArtistKey,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal reconcile-identity backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-identity backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-identity backup record: %w", err)
	}
	return nil
}
