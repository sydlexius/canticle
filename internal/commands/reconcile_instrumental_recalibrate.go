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

	"github.com/sydlexius/canticle/internal/instrumentalrecalib"
	"github.com/sydlexius/canticle/internal/lyrics"
)

// reconcileInstrumentalRecalibrateBackup is one JSONL record per row this run
// acted on, written and fsynced before the row is mutated so an applied
// change always has its restorable record.
type reconcileInstrumentalRecalibrateBackup struct {
	QueueID     int64     `json:"queue_id"`
	Artist      string    `json:"artist"`
	Title       string    `json:"title"`
	SourcePath  string    `json:"source_path"`
	VocalPeak   float64   `json:"vocal_peak"`
	Action      string    `json:"action"`
	MarkerPaths []string  `json:"marker_paths,omitempty"`
	At          time.Time `json:"at"`
}

func appendReconcileInstrumentalRecalibrateBackup(f *os.File, ch instrumentalrecalib.Change) error {
	rec := reconcileInstrumentalRecalibrateBackup{
		QueueID:     ch.QueueID,
		Artist:      ch.Artist,
		Title:       ch.Title,
		SourcePath:  ch.SourcePath,
		VocalPeak:   ch.VocalPeak,
		Action:      ch.Action,
		MarkerPaths: ch.MarkerPaths,
		At:          time.Now().UTC(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write backup record: %w", err)
	}
	// fsync before the row is mutated (the identityrepair backup-first rule).
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync backup record: %w", err)
	}
	return nil
}

// runReconcileInstrumentalRecalibrate is CLI wiring over
// internal/instrumentalrecalib: it resolves config/queue (no detector -- the
// engine re-decides from telemetry already stamped on each row), owns the
// JSONL backup file and the operator output, and lets the package own the
// re-decision logic. Dry-run unless --yes.
func runReconcileInstrumentalRecalibrate(ctx context.Context, out io.Writer, args ScanReconcileInstrumentalRecalibrateCmd) int {
	env, code := openQueueEnv(ctx, out, args.ConfigPath, args.Library)
	if env == nil {
		return code
	}
	defer env.Close()

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(env.cfg.DB.Path), fmt.Sprintf("reconcile-instrumental-recalibrate-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backup *os.File
	defer func() {
		if backup != nil {
			_ = backup.Close() //nolint:errcheck // reason: best-effort close on command exit
		}
	}()

	if !args.Yes {
		_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate%s: dry run; pass --yes to apply\n", env.libLabel)
	}

	// The engine's Result counters are APPLY-ONLY by design (a dry run reports
	// through Preview and mutates nothing, pinned by
	// TestRun_DryRunDoesNotMutate). Counting previews here keeps the dry-run
	// summary honest: printing the apply-shaped counters after a preview-only
	// pass renders "reversed=0" under a screenful of "would reverse" lines,
	// which reads as "nothing to do" when the opposite is true.
	previewed := 0

	rc := instrumentalrecalib.New(env.queue, lyrics.NewLRCWriter())
	run := rc.Run
	if args.Reverse {
		run = rc.Reverse
	}
	res, err := run(ctx, instrumentalrecalib.Options{
		LibraryID:      env.libraryID,
		Limit:          args.Limit,
		DryRun:         !args.Yes,
		MinConfidence:  env.cfg.InstrumentalDetector.MinConfidence,
		VocalMax:       env.cfg.InstrumentalDetector.VocalMaxConfidence,
		SpeechMax:      env.cfg.InstrumentalDetector.SpeechMaxConfidence,
		CurrentVersion: version,
		Preview: func(ch instrumentalrecalib.Change) {
			previewed++
			switch ch.Action {
			case "settle":
				_, _ = fmt.Fprintf(out, "would settle: id=%d  %s  -> write instrumental marker + settle\n", ch.QueueID, ch.SourcePath)
			case "reverse":
				_, _ = fmt.Fprintf(out, "would reverse: id=%d  %s  vocal_peak=%.6f  -> remove detector marker + requeue for a provider fetch\n", ch.QueueID, ch.SourcePath, ch.VocalPeak)
			default:
				_, _ = fmt.Fprintf(out, "would reset: id=%d  %s  -> stale telemetry version, reset for re-scan\n", ch.QueueID, ch.SourcePath)
			}
		},
		Report: func(ch instrumentalrecalib.Change) error {
			if backup == nil {
				f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
				if ferr != nil {
					return fmt.Errorf("open backup file %s: %w", backupPath, ferr)
				}
				backup = f
			}
			return appendReconcileInstrumentalRecalibrateBackup(backup, ch)
		},
	})
	if err != nil {
		slog.Error("reconcile-instrumental-recalibrate failed", "error", err)
		return 1
	}

	candidates := "vocal-gate-rejected"
	if args.Reverse {
		candidates = "confirmed-instrumental"
	}
	_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate%s: %d %s row(s) considered\n",
		env.libLabel, res.Total, candidates)
	if !args.Yes {
		verb := "settle/reset"
		if args.Reverse {
			verb = "reverse"
		}
		_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate dry run: would %s %d row(s); skipped(provider-owned=%d) errors=%d\n",
			verb, previewed, res.SkippedProviderOwned, res.Errors)
		if res.Errors > 0 {
			return 1
		}
		return 0
	}
	if args.Reverse {
		_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate done: reversed=%d markers-removed=%d skipped(provider-owned=%d worker-claimed=%d) errors=%d\n",
			res.Reversed, res.MarkersRemoved, res.SkippedProviderOwned, res.SkippedClaimed, res.Errors)
	} else {
		_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate done: settled=%d markers-written=%d reset-stale=%d skipped(worker-claimed=%d) errors=%d\n",
			res.Settled, res.MarkersWritten, res.ResetStale, res.SkippedClaimed, res.Errors)
	}
	if args.Yes && (res.Settled > 0 || res.ResetStale > 0 || res.Reversed > 0) {
		_, _ = fmt.Fprintf(out, "backup of changed rows written to %s\n", backupPath)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}
