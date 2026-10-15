package tierrepair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/revalidate"
)

// SyncTierBackupRecord is one JSONL line: a classified row's id and the tier
// it was stamped with. Every candidate's prior sync_tier is NULL by
// construction, so restoring is always "set it back to NULL".
type SyncTierBackupRecord struct {
	ID   int64  `json:"id"`
	Tier string `json:"tier"`
}

// SyncTierQueue is the queue surface the sync-tier backfill needs.
// *queue.DBQueue satisfies it; a test may fake the write.
type SyncTierQueue interface {
	SetSyncTierIfPending(ctx context.Context, id int64, tier string, backup func() error) (bool, error)
}

// SyncTierLister lists the rows still to classify (database only).
type SyncTierLister interface {
	ListSyncTierPending(ctx context.Context) ([]queue.SyncTierCandidate, error)
}

// ListSyncTier returns the completed synced rows with no sync_tier yet. Its
// length is the pending count; it opens no library file.
func ListSyncTier(ctx context.Context, q SyncTierLister) ([]queue.SyncTierCandidate, error) {
	return q.ListSyncTierPending(ctx) //nolint:wrapcheck // reason: the caller logs the list failure with its own context
}

// SyncTierResult is a run's counts. Counts is keyed by the outcome of the
// classification (word, line, unsynced, no_audio, no_sidecar, errored) plus
// "raced"; WriteFailed counts guarded writes that errored.
type SyncTierResult struct {
	Scanned     int
	Counts      map[string]int
	WriteFailed int
	Stamped     int
}

// RunSyncTier classifies each candidate's sidecar and, when o.Apply, stamps
// the tier through q. Backup-first: each record is written inside
// SetSyncTierIfPending's own transaction before that stamp commits, so a
// raced row never gets a backup call. A write failure counts separately from
// a classification failure and does not stop the remaining rows. When ctx is
// canceled it stops at the next row boundary and returns the partial result
// with an error wrapping ctx.Err(); every row already stamped has its backup
// record, and no row is half-applied.
func RunSyncTier(ctx context.Context, q SyncTierQueue, candidates []queue.SyncTierCandidate, o Options) (SyncTierResult, error) {
	res := SyncTierResult{Scanned: len(candidates), Counts: map[string]int{}}
	if err := o.check(); err != nil {
		return res, err
	}
	for i, c := range candidates {
		if cerr := ctx.Err(); cerr != nil {
			return res, fmt.Errorf("sync-tier repair stopped: %w", cerr)
		}
		tier, outcome := ClassifySyncTier(c)
		res.Counts[outcome]++
		if tier != "" && o.Apply {
			applied, err := q.SetSyncTierIfPending(ctx, c.ID, tier, func() error {
				return appendSyncTierBackup(o.Backup, c.ID, tier)
			})
			switch {
			case err != nil:
				slog.Warn("reconcile-sync-tier: stamp failed; leaving row unclassified", "id", c.ID, "error", err)
				res.Counts[outcome]--
				res.WriteFailed++
			case !applied:
				// Raced since listing (#1075 finding 3): never overwritten, applied, or backed up.
				res.Counts[outcome]--
				res.Counts["raced"]++
			default:
				res.Stamped++
			}
		}
		o.report(i+1, len(candidates))
	}
	return res, nil
}

// ClassifySyncTier resolves and classifies one candidate's sidecar.
// Returns ("", outcome) for anything it could not classify (no_audio,
// no_sidecar, errored -- an unreadable/symlinked sidecar, or ClassifyLRCFile's
// own error for a merely unreadable companion), so the row is left NULL and
// counted, never guessed. The sidecar is DERIVED from the audio path (stem +
// ".lrc", matching revalidate.judgeCandidate), with
// revalidate.ResolveSidecarCaseVariant as its case-mismatch fallback. NO
// outdir+filename fallback for a blank source_path (#1075 finding 4).
func ClassifySyncTier(c queue.SyncTierCandidate) (tier, outcome string) {
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
// that stamp commits (#1087 review).
func appendSyncTierBackup(s Sink, id int64, tier string) error {
	b, err := json.Marshal(SyncTierBackupRecord{ID: id, Tier: tier})
	if err != nil {
		return fmt.Errorf("marshal reconcile-sync-tier backup record: %w", err)
	}
	return writeRecord(s, "reconcile-sync-tier", b)
}
