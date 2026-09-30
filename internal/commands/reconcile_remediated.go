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
	"github.com/sydlexius/canticle/internal/reports"
)

// remediatedBackupRecord is one JSONL line: the row's prior state and the
// action applied, enough to restore it by hand. No artist, title or path.
type remediatedBackupRecord struct {
	ID            int64  `json:"id"`
	Action        string `json:"action"`
	Tier          string `json:"tier,omitempty"`
	PriorOutcome  string `json:"prior_outcome_type"`
	PriorSyncTier string `json:"prior_sync_tier"`
	PriorTiming   string `json:"prior_timing_outcome"`
	PriorStatus   string `json:"prior_status"`
}

// Skip/outcome keys that are not queue actions.
const (
	remAlreadyRecorded = "already_recorded"
	remKeptVerdict     = "kept_remediation_verdict"
	remUnsyncedLRC     = "unsynced_lrc"
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
	candidates, err := q.ListRemediatedCandidates(ctx, reports.TierUnknownPredicate)
	if err != nil {
		slog.Error("reconcile-remediated: list candidates failed", "error", err)
		return 1
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-remediated-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}

	var backupFile *os.File
	if args.Yes {
		f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
		if ferr != nil {
			slog.Error("reconcile-remediated: open backup file failed", "error", ferr)
			return 1
		}
		backupFile = f
		lyrics.FsyncDir(filepath.Dir(backupPath))
		defer func() { _ = backupFile.Close() }()
	}

	counts := map[string]int{}
	writeFailed, applied := 0, 0
	for _, c := range candidates {
		if ctx.Err() != nil {
			return 1
		}
		action, tier, outcome := planRemediated(c)
		counts[outcome]++
		if action == "" || !args.Yes {
			continue
		}
		ok, err := q.ApplyRemediated(ctx, c, action, tier, func() error {
			return appendRemediatedBackup(backupFile, c, action, tier)
		})
		switch {
		case err != nil:
			slog.Warn("reconcile-remediated: apply failed; leaving row unchanged", "id", c.ID, "error", err)
			counts[outcome]--
			writeFailed++
		case !ok:
			counts[outcome]--
			counts["raced"]++
		default:
			applied++
		}
	}

	verb := "would"
	if args.Yes {
		verb = "did"
	}
	// "scanned" is the candidate superset (in-flight rows included), not the
	// dashboard's tier-unknown tile.
	_, _ = fmt.Fprintf(out, "reconcile-remediated: scanned %d candidate row(s) (a superset of the dashboard tier-unknown count); %s reset %d for re-fetch, %s re-describe %d as unsynced, %s record tier for %d%s\n",
		len(candidates), verb, counts[queue.RemediatedReset], verb, counts[queue.RemediatedUnsynced], verb, counts[queue.RemediatedTier], suffixDryRun(args.Yes))
	_, _ = fmt.Fprintf(out, "skipped: already_recorded=%d kept_remediation_verdict=%d unsynced_lrc=%d retired=%d processing=%d word_recheck=%d upgrade_armed=%d no_audio=%d audio_gone=%d unreadable=%d raced=%d write_failed=%d\n",
		counts[remAlreadyRecorded], counts[remKeptVerdict], counts[remUnsyncedLRC], counts["retired"], counts["processing"], counts["word_recheck"],
		counts["upgrade_armed"], counts["no_audio"], counts["audio_gone"], counts["errored"], counts["raced"], writeFailed)
	if applied > 0 {
		_, _ = fmt.Fprintf(out, "backup of %d changed row(s) written to %s\n", applied, backupPath)
	}
	if writeFailed > 0 {
		return 1
	}
	return 0
}

// planRemediated decides one candidate's action from the files on disk,
// reusing classifySyncTierCandidate for the .lrc (incl. the case-variant
// resolver). Returns ("", "", outcome) when nothing is to be done: skipped
// rows, unjudgeable rows (never guessed) or a recorded tier. A row keeping a
// remediation timing_outcome beside a present .lrc is counted as
// kept_remediation_verdict: the tier is still recorded if it differs, but the
// row stays tier-unknown by design. An .lrc that classifies as unsynced is left
// untouched and counted (unsynced_lrc): the tier action only takes word/line
// and re-typing a row's outcome off a classifier call is not this command's job.
func planRemediated(c queue.RemediatedCandidate) (action, tier, outcome string) {
	switch {
	case c.Retired:
		return "", "", "retired"
	case c.Status == "processing":
		return "", "", "processing"
	case c.WordQueued:
		return "", "", "word_recheck"
	case c.UpgradeArmed:
		return "", "", "upgrade_armed"
	}
	t, o := classifySyncTierCandidate(queue.SyncTierCandidate{ID: c.ID, AudioPath: c.AudioPath})
	switch {
	case t == queue.SyncTierWord || t == queue.SyncTierLine:
		switch {
		case isRemediationVerdict(c.PriorTiming) && t == c.PriorSyncTier:
			return "", "", remKeptVerdict
		case isRemediationVerdict(c.PriorTiming):
			return queue.RemediatedTier, t, remKeptVerdict
		case t == c.PriorSyncTier:
			return "", "", remAlreadyRecorded
		}
		return queue.RemediatedTier, t, queue.RemediatedTier
	case t != "":
		return "", "", remUnsyncedLRC
	case o != "no_sidecar":
		return "", "", o // no_audio or errored
	}
	audio := strings.TrimSpace(c.AudioPath)
	// A row whose audio moved (but kept an ISRC/MBID, so prune never retired
	// it) must not be reset: that would refetch and orphan a sidecar.
	if _, err := os.Lstat(audio); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", "audio_gone"
		}
		return "", "", "errored"
	}
	base := strings.TrimSuffix(audio, filepath.Ext(audio))
	// classifySyncTierCandidate reports a symlinked .lrc as no_sidecar too; a
	// present entry of any kind must never be reset over, and only a regular
	// .txt counts as the unsynced sidecar.
	for _, ext := range []string{".lrc", ".txt"} {
		fi, err := sidecarInfo(base + ext)
		switch {
		case err != nil:
			return "", "", "errored"
		case fi == nil:
			continue
		case ext == ".lrc" || !fi.Mode().IsRegular():
			return "", "", "errored"
		}
		return queue.RemediatedUnsynced, "", queue.RemediatedUnsynced
	}
	return queue.RemediatedReset, "", queue.RemediatedReset
}

// isRemediationVerdict matches the timing_outcome values reports'
// TierUnknownPredicate treats as tier-unknown.
func isRemediationVerdict(timing string) bool {
	return timing == "categorical" || timing == "mis_synced" || timing == "degenerate"
}

// sidecarInfo Lstats path, else any extension-case variant in its directory
// (revalidate.ResolveSidecarCaseVariant skips symlinks and directories, which
// this command must still see); nil, nil if absent. A regular variant wins.
func sidecarInfo(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err == nil {
		return fi, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	var found os.FileInfo
	for _, e := range entries {
		if !strings.EqualFold(e.Name(), filepath.Base(path)) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			return nil, ierr
		}
		if info.Mode().IsRegular() {
			return info, nil
		}
		if found == nil {
			found = info
		}
	}
	return found, nil
}

// appendRemediatedBackup writes and fsyncs one record, write-ahead from inside
// the row's transaction.
func appendRemediatedBackup(f *os.File, c queue.RemediatedCandidate, action, tier string) error {
	b, err := json.Marshal(remediatedBackupRecord{ID: c.ID, Action: action, Tier: tier, PriorOutcome: c.PriorOutcome,
		PriorSyncTier: c.PriorSyncTier, PriorTiming: c.PriorTiming, PriorStatus: c.Status})
	if err != nil {
		return fmt.Errorf("marshal reconcile-remediated backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-remediated backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-remediated backup record: %w", err)
	}
	return nil
}
