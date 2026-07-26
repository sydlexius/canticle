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

// purgeProvenanceBackupRecord is one JSONL line capturing a deleted sidecar --
// its full byte content plus the database rows reset for it -- so the operation
// is auditable and genuinely hand-restorable: writing Content back to Path
// reconstructs the file exactly. Written and fsynced before the sidecar it
// protects is deleted (backup-first / write-ahead).
//
// The content is not optional garnish. The purge invalidates the lyrics_cache
// row carrying the same lyrics in the same transaction that resets the queue
// rows, so after a run this record is the ONLY surviving copy. Without it a
// hand-edited sidecar, or one whose provider no longer serves the track, is
// unrecoverable; so is any sidecar no scan_results row claims (nothing is
// requeued to rewrite it).
type purgeProvenanceBackupRecord struct {
	Path          string  `json:"path"`
	ScanResultIDs []int64 `json:"scan_result_ids,omitempty"`
	WorkItemIDs   []int64 `json:"work_item_ids,omitempty"`
	// Content is the sidecar's bytes as they were on disk, base64-encoded by
	// encoding/json. Captured before the delete; a capture failure aborts that
	// sidecar's deletion rather than losing it.
	Content []byte `json:"content"`
}

// purgeProvenanceMaxBackupBytes caps how large a sidecar the backup will
// capture. Lyrics files are kilobytes; anything past this is not a sidecar this
// command should be silently swallowing into a JSONL line. Exceeding it is an
// error, which (backup-first) leaves the file on disk untouched -- never a
// truncated record standing in for a deleted file.
const purgeProvenanceMaxBackupBytes = 4 << 20 // 4 MiB

// runPurgeProvenance bulk-deletes .lrc/.txt sidecars matching a provenance
// filter (--source or --no-source) and requeues their coupled work_queue /
// scan_results rows so the next scan re-fetches (issue #474). Dry-run by
// default; --yes applies and writes a JSONL backup of every deleted sidecar,
// fsynced before its delete.
func runPurgeProvenance(ctx context.Context, out io.Writer, args ScanPurgeProvenanceCmd) int {
	// Trim ONCE and use the trimmed value everywhere. Validating the trimmed
	// form while filtering on the raw one let `--source " musixmatch"` pass the
	// guard and then match nothing at all, silently reporting an empty purge
	// set for a filter the operator believed was valid.
	source := strings.TrimSpace(args.Source)
	hasSource := source != ""
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

	filter := purgeprovenance.Filter{Source: source, NoSource: args.NoSource}
	res, err := purgeprovenance.New(sqlDB).Run(ctx, purgeprovenance.Options{
		Roots:     roots,
		LibraryID: libID,
		Filter:    filter,
		DryRun:    !args.Yes,
		Report:    report,
	})
	if err != nil {
		// Do NOT return here. A fatal abort (context cancellation) can land
		// after files have ALREADY been deleted across earlier roots, and
		// exiting before the summary would leave the operator with no record
		// of what was destroyed. Log, print the partial summary below, then
		// exit non-zero.
		slog.Error("purge-provenance failed", "error", err)
		_, _ = fmt.Fprintf(out, "purge-provenance: run aborted: %v (partial results follow)\n", err)
	}

	verb := "would delete"
	// res.Matched is incremented before the in-flight check, so it includes
	// sidecars an apply run will SKIP. Subtracting the skipped-in-flight count
	// makes the preview agree with what apply mode actually deletes -- the
	// whole point of a dry run.
	deleted := res.Matched - res.SkippedProcessing
	if args.Yes {
		verb = "deleted"
		deleted = res.Deleted
	}
	_, _ = fmt.Fprintf(out, "purge-provenance: scanned %d sidecar(s); %s %d, requeued %d (%d scan_results reset, %d cache entries invalidated, %d skipped in-flight, %d skipped symlink, %d errors)%s\n",
		res.Scanned, verb, deleted, res.WorkItemsRequeued, res.ScanResultsReset, res.CacheInvalidated, res.SkippedProcessing, res.SkippedSymlink, res.Errors, suffixDryRun(args.Yes))
	if res.UnlinkedNoCacheKey > 0 {
		// Not an error: nothing was lost, but the re-fetch guarantee does not
		// extend to these, so say so rather than let the summary imply it does.
		_, _ = fmt.Fprintf(out, "note: %d deleted sidecar(s) had no linked scan_results row; nothing was requeued for them and no cache entry could be invalidated\n",
			res.UnlinkedNoCacheKey)
	}
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup of deleted sidecars written to %s\n", backupPath)
	}
	if err != nil || res.Errors > 0 {
		return 1
	}
	return 0
}

// appendPurgeProvenanceBackup reads the sidecar's bytes and writes and fsyncs
// one JSONL record per deleted sidecar, so the backup is durable -- and
// complete -- before the delete it protects. Any failure here (read or write)
// is returned, which aborts that sidecar's deletion: a file is never deleted
// without a restorable copy of its content.
func appendPurgeProvenanceBackup(f *os.File, rec purgeprovenance.Record) error {
	content, err := readPurgeProvenanceSidecar(rec.Path)
	if err != nil {
		return err
	}
	b, err := json.Marshal(purgeProvenanceBackupRecord{
		Path:          rec.Path,
		ScanResultIDs: rec.ScanResultIDs,
		WorkItemIDs:   rec.WorkItemIDs,
		Content:       content,
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

// readPurgeProvenanceSidecar reads a sidecar's bytes for the backup record.
// Lstat-gated: the purge never follows a symlink, so neither does the backup,
// and an oversized file is refused outright rather than captured partially.
func readPurgeProvenanceSidecar(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat sidecar %q for backup: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to back up symlinked sidecar %q", path)
	}
	if fi.Size() > purgeProvenanceMaxBackupBytes {
		return nil, fmt.Errorf("sidecar %q is %d bytes, over the %d-byte backup limit; not deleting it", path, fi.Size(), purgeProvenanceMaxBackupBytes)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a sidecar enumerated from a configured library root, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("read sidecar %q for backup: %w", path, err)
	}
	return b, nil
}
