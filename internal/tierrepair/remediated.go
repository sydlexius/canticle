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

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// RemediatedBackupRecord is one JSONL line: the row's prior description and the
// action applied. No artist, title or path. For unsynced and tier it is enough
// to restore the row by hand. For reset it is an AUDIT record only: reset clears
// the word-timing, upgrade and timing-stamp columns it does not save, and
// restoring the pre-reset state would re-create what reset exists to remove, a
// row claiming a synced file that is not on disk.
type RemediatedBackupRecord struct {
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
	OutcomeAlreadyRecorded = "already_recorded"
	OutcomeKeptVerdict     = "kept_remediation_verdict"
	OutcomeUnsyncedLRC     = "unsynced_lrc"
)

// RemediatedQueue is the queue surface the remediated backfill needs.
// *queue.DBQueue satisfies it.
type RemediatedQueue interface {
	ListRemediatedCandidates(ctx context.Context, tierUnknown string) ([]queue.RemediatedCandidate, error)
	ApplyRemediated(ctx context.Context, c queue.RemediatedCandidate, action, tier string, backup func() error) (bool, error)
}

// ListRemediated returns the candidate superset (in-flight rows included, not
// the dashboard's tier-unknown tile). Its length is the pending count; it
// opens no library file.
func ListRemediated(ctx context.Context, q RemediatedQueue) ([]queue.RemediatedCandidate, error) {
	return q.ListRemediatedCandidates(ctx, reports.TierUnknownPredicate) //nolint:wrapcheck // reason: the caller logs the list failure with its own context
}

// RemediatedResult is a run's counts. Counts is keyed by the outcome of
// DecideRemediated plus "raced"; WriteFailed counts guarded writes that
// errored; Applied counts rows changed.
type RemediatedResult struct {
	Scanned     int
	Counts      map[string]int
	WriteFailed int
	Applied     int
}

// RunRemediated decides each candidate's action from the files on disk and,
// when o.Apply, applies it through q, writing each backup record inside the
// row's own transaction first. No SQLITE_BUSY retry: a retry would write the
// backup record twice, so a busy row counts as write_failed and a rerun is
// safe (the apply guards make it idempotent). When ctx is canceled it stops at
// the next row boundary and returns the partial result with an error wrapping
// ctx.Err(); every changed row has its backup record, and no row is
// half-applied.
func RunRemediated(ctx context.Context, q RemediatedQueue, candidates []queue.RemediatedCandidate, o Options) (RemediatedResult, error) {
	res := RemediatedResult{Scanned: len(candidates), Counts: map[string]int{}}
	if err := o.check(); err != nil {
		return res, err
	}
	for i, c := range candidates {
		if cerr := ctx.Err(); cerr != nil {
			return res, fmt.Errorf("remediated repair stopped: %w", cerr)
		}
		action, tier, outcome := DecideRemediated(c)
		res.Counts[outcome]++
		if action != "" && o.Apply {
			ok, err := q.ApplyRemediated(ctx, c, action, tier, func() error {
				return appendRemediatedBackup(o.Backup, c, action, tier)
			})
			switch {
			case err != nil:
				slog.Warn("reconcile-remediated: apply failed; leaving row unchanged", "id", c.ID, "error", err)
				res.Counts[outcome]--
				res.WriteFailed++
			case !ok:
				res.Counts[outcome]--
				res.Counts["raced"]++
			default:
				res.Applied++
			}
		}
		o.report(i+1, len(candidates))
	}
	return res, nil
}

// DecideRemediated decides one candidate's action from the files on disk,
// reusing ClassifySyncTier for the .lrc (incl. the case-variant
// resolver). Returns ("", "", outcome) when nothing is to be done: skipped
// rows, unjudgeable rows (never guessed) or a recorded tier. A row keeping a
// remediation timing_outcome beside a present .lrc is counted as
// kept_remediation_verdict: the tier is still recorded if it differs, but the
// row stays tier-unknown by design. An .lrc that classifies as unsynced is left
// untouched and counted (unsynced_lrc): the tier action only takes word/line
// and re-typing a row's outcome off a classifier call is not this command's job.
func DecideRemediated(c queue.RemediatedCandidate) (action, tier, outcome string) {
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
	t, o := ClassifySyncTier(queue.SyncTierCandidate{ID: c.ID, AudioPath: c.AudioPath})
	switch {
	case t == queue.SyncTierWord || t == queue.SyncTierLine:
		switch {
		case isRemediationVerdict(c.PriorTiming) && t == c.PriorSyncTier:
			return "", "", OutcomeKeptVerdict
		case isRemediationVerdict(c.PriorTiming):
			return queue.RemediatedTier, t, OutcomeKeptVerdict
		case t == c.PriorSyncTier:
			return "", "", OutcomeAlreadyRecorded
		}
		return queue.RemediatedTier, t, queue.RemediatedTier
	case t != "":
		return "", "", OutcomeUnsyncedLRC
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
	// ClassifySyncTier reports a symlinked .lrc as no_sidecar too; a
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
func appendRemediatedBackup(s Sink, c queue.RemediatedCandidate, action, tier string) error {
	b, err := json.Marshal(RemediatedBackupRecord{ID: c.ID, Action: action, Tier: tier, PriorOutcome: c.PriorOutcome,
		PriorSyncTier: c.PriorSyncTier, PriorTiming: c.PriorTiming, PriorStatus: c.Status})
	if err != nil {
		return fmt.Errorf("marshal reconcile-remediated backup record: %w", err)
	}
	return writeRecord(s, "reconcile-remediated", b)
}
