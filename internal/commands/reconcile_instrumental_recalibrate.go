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
	// Type discriminates the two records a row can produce in the JSONL: "intent"
	// (this shape, written before the mutation) and "outcome" (the realized result
	// appended after, see reconcileInstrumentalRecalibrateOutcome). Absent on
	// records written before #515; a reader must treat a missing Type as "intent".
	Type        string    `json:"type,omitempty"`
	QueueID     int64     `json:"queue_id"`
	Artist      string    `json:"artist"`
	Title       string    `json:"title"`
	SourcePath  string    `json:"source_path"`
	VocalPeak   float64   `json:"vocal_peak"`
	Action      string    `json:"action"`
	MarkerPaths []string  `json:"marker_paths,omitempty"`
	At          time.Time `json:"at"`
}

// reconcileInstrumentalRecalibrateOutcome is the second JSONL record for a row:
// the realized result appended AFTER the mutation resolves, so a restore replays
// only rows whose outcome is "applied" and never a "skipped" no-op (#515). Keyed
// by QueueID to the earlier intent record.
type reconcileInstrumentalRecalibrateOutcome struct {
	Type      string    `json:"type"` // always "outcome"
	QueueID   int64     `json:"queue_id"`
	Outcome   string    `json:"outcome"`             // applied | skipped | failed
	Ambiguous bool      `json:"ambiguous,omitempty"` // failed-but-maybe-applied settle
	At        time.Time `json:"at"`
}

func appendReconcileInstrumentalRecalibrateBackup(f *os.File, ch instrumentalrecalib.Change) error {
	rec := reconcileInstrumentalRecalibrateBackup{
		Type:        "intent",
		QueueID:     ch.QueueID,
		Artist:      ch.Artist,
		Title:       ch.Title,
		SourcePath:  ch.SourcePath,
		VocalPeak:   ch.VocalPeak,
		Action:      ch.Action,
		MarkerPaths: ch.MarkerPaths,
		At:          time.Now().UTC(),
	}
	return appendJSONLSynced(f, rec)
}

// appendReconcileInstrumentalRecalibrateOutcome appends the realized-outcome
// record for a row. Like the intent record it is fsynced, so the backup trail is
// durable across a crash between the mutation and the next row (#515).
func appendReconcileInstrumentalRecalibrateOutcome(f *os.File, o instrumentalrecalib.Outcome) error {
	return appendJSONLSynced(f, reconcileInstrumentalRecalibrateOutcome{
		Type:      "outcome",
		QueueID:   o.QueueID,
		Outcome:   o.Status,
		Ambiguous: o.Ambiguous,
		At:        time.Now().UTC(),
	})
}

// appendJSONLSynced marshals rec, appends it as one JSONL line, and fsyncs --
// the shared write path for both the intent and outcome records so they get
// identical durability (the identityrepair backup-first rule).
func appendJSONLSynced(f *os.File, rec any) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync backup record: %w", err)
	}
	return nil
}

// runReconcileInstrumentalRecalibrate is CLI wiring over
// internal/instrumentalrecalib: it resolves config/queue, owns the JSONL backup
// file and the operator output, and lets the package own the re-decision logic.
// Dry-run unless --yes.
//
// The re-DECISION still runs entirely on telemetry already stamped on each row --
// no inference, no audio read. The one thing it asks the sidecar is which MODEL
// it currently serves (#684), a single cheap GET that keys the version
// comparison; when that is unanswerable the engine treats the version as unknown
// and declines to reset anything.
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
	// --after-id is a forward-direction resume cursor; the Reverse path lists a
	// different row set (confirmed instrumentals), so the cursor does not apply
	// there. Reject the combination loudly rather than silently ignore it.
	if args.Reverse && args.AfterID > 0 {
		_, _ = fmt.Fprintln(out, "reconcile-instrumental-recalibrate: --after-id is not supported with --reverse (the cursor is forward-direction only)")
		return 1
	}

	res, err := run(ctx, instrumentalrecalib.Options{
		LibraryID:     env.libraryID,
		Limit:         args.Limit,
		AfterID:       args.AfterID,
		DryRun:        !args.Yes,
		MinConfidence: env.cfg.InstrumentalDetector.MinConfidence,
		VocalMax:      env.cfg.InstrumentalDetector.VocalMaxConfidence,
		SpeechMax:     env.cfg.InstrumentalDetector.SpeechMaxConfidence,
		// The MODEL version, matching what the worker persists as
		// work_queue.detector_version (#684). Reading the APP version here would
		// make every row look version-mismatched after migration 038 and take the
		// RESET branch, converting a recalibration into a full-library
		// re-inference. Unknown ("") is handled inside the engine as "change
		// nothing destructive", never as a mismatch.
		CurrentVersion: currentDetectorModelVersion(ctx, env.cfg),
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
		Outcome: func(o instrumentalrecalib.Outcome) error {
			// The intent record (Report) already opened the backup; a nil handle
			// here means Report itself failed to open it, and the engine reports
			// that row's outcome as failed. Nothing to append to, so skip.
			if backup == nil {
				return nil
			}
			return appendReconcileInstrumentalRecalibrateOutcome(backup, o)
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
	// Resume cursor (#516): only the forward direction pages by id, and only a
	// --limit-capped run can leave more behind (a full run examined everything).
	// A run that filled its --limit may have more rows past MaxExaminedID, so tell
	// the operator how to continue without re-selecting the rows just examined.
	if !args.Reverse && args.Limit > 0 && res.Total == args.Limit && res.MaxExaminedID > 0 {
		_, _ = fmt.Fprintf(out, "more rows may remain; resume with --after-id=%d\n", res.MaxExaminedID)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}
