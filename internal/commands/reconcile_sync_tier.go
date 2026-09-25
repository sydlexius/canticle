package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/revalidate"
)

// syncTierBackupRecord is one JSONL line: a classified row's id and the tier
// it was stamped with. Every candidate's prior sync_tier is NULL by
// construction, so restoring is always "set it back to NULL".
type syncTierBackupRecord struct {
	ID   int64  `json:"id"`
	Tier string `json:"tier"`
}

// syncTierStamper lets the write-failure path (#1087 review, Copilot
// 4100243158 / CodeRabbit 4100264743) be tested with a fake, without a real
// database. *queue.DBQueue satisfies it.
type syncTierStamper interface {
	SetSyncTierIfPending(ctx context.Context, id int64, tier string, backup func() error) (bool, error)
}

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
	candidates, err := q.ListSyncTierPending(ctx)
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
func runReconcileSyncTierCore(ctx context.Context, out io.Writer, q syncTierStamper, candidates []queue.SyncTierCandidate, yes bool, backupPath string) int {
	var backupFile *os.File
	if yes {
		f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
		if ferr != nil {
			slog.Error("reconcile-sync-tier: open backup file failed", "path", backupPath, "error", ferr)
			return 1
		}
		backupFile = f
		lyrics.FsyncDir(filepath.Dir(backupPath))
		defer func() {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-sync-tier backup file", "path", backupPath, "error", cerr)
			}
		}()
	}

	counts := map[string]int{}
	writeFailed, stamped := 0, 0
	for _, c := range candidates {
		if cerr := ctx.Err(); cerr != nil {
			return 1
		}
		tier, outcome := classifySyncTierCandidate(c)
		counts[outcome]++
		if tier == "" || !yes {
			continue
		}
		applied, err := q.SetSyncTierIfPending(ctx, c.ID, tier, func() error {
			return appendSyncTierBackup(backupFile, c.ID, tier)
		})
		if err != nil {
			slog.Warn("reconcile-sync-tier: stamp failed; leaving row unclassified", "id", c.ID, "error", err)
			counts[outcome]--
			writeFailed++
			continue
		}
		if !applied {
			// Raced since listing (#1075 finding 3): never overwritten, applied, or backed up.
			counts[outcome]--
			counts["raced"]++
			continue
		}
		stamped++
	}

	verb := "would classify"
	if yes {
		verb = "classified"
	}
	_, _ = fmt.Fprintf(out, "reconcile-sync-tier: scanned %d row(s); %s %d as word, %d as line, %d as unsynced (no_audio=%d no_sidecar=%d unreadable=%d raced=%d write_failed=%d)%s\n",
		len(candidates), verb, counts[queue.SyncTierWord], counts[queue.SyncTierLine], counts[queue.SyncTierUnsynced],
		counts["no_audio"], counts["no_sidecar"], counts["errored"], counts["raced"], writeFailed, suffixDryRun(yes))
	// Only when >=1 record landed, so an empty eagerly-opened file (every
	// candidate raced) never reads as "backup written".
	if stamped > 0 {
		_, _ = fmt.Fprintf(out, "backup of classified rows written to %s\n", backupPath)
	}
	if writeFailed > 0 {
		return 1
	}
	return 0
}

// classifySyncTierCandidate resolves and classifies one candidate's sidecar.
// Returns ("", outcome) for anything it could not classify (no_audio,
// no_sidecar, errored -- an unreadable/symlinked sidecar, or ClassifyLRCFile's
// own error for a merely unreadable companion), so the row is left NULL and
// counted, never guessed. The sidecar is DERIVED from the audio path (stem +
// ".lrc", matching revalidate.judgeCandidate), with
// revalidate.ResolveSidecarCaseVariant as its case-mismatch fallback. NO
// outdir+filename fallback for a blank source_path (#1075 finding 4).
func classifySyncTierCandidate(c queue.SyncTierCandidate) (tier, outcome string) {
	audio := strings.TrimSpace(c.AudioPath)
	if audio == "" {
		return "", "no_audio"
	}
	path := strings.TrimSuffix(audio, filepath.Ext(audio)) + ".lrc"
	fi, lerr := os.Lstat(path)
	if lerr != nil && errors.Is(lerr, fs.ErrNotExist) {
		if variant, vfi, ok := revalidate.ResolveSidecarCaseVariant(path); ok {
			path, fi, lerr = variant, vfi, nil
		}
	}
	switch {
	case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
		return "", "errored"
	case lerr != nil:
		return "", "no_sidecar"
	case fi.Mode()&os.ModeSymlink != 0:
		return "", "no_sidecar"
	}
	classified, cerr := lyrics.ClassifyLRCFile(path)
	if cerr != nil {
		return "", "errored"
	}
	tier = string(classified)
	return tier, tier
}

// appendSyncTierBackup writes and fsyncs one JSONL record per classified row,
// called write-ahead from inside SetSyncTierIfPending's transaction, before
// that stamp commits (#1087 review). A candidate's prior sync_tier is always
// NULL, so restoring is always "set it back to NULL".
func appendSyncTierBackup(f *os.File, id int64, tier string) error {
	b, err := json.Marshal(syncTierBackupRecord{ID: id, Tier: tier})
	if err != nil {
		return fmt.Errorf("marshal reconcile-sync-tier backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-sync-tier backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-sync-tier backup record: %w", err)
	}
	return nil
}
