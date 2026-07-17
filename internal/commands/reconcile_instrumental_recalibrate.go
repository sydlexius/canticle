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

	"github.com/doxazo-net/canticle/internal/instrumentalrecalib"
	"github.com/doxazo-net/canticle/internal/lyrics"
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

	rc := instrumentalrecalib.New(env.queue, lyrics.NewLRCWriter())
	res, err := rc.Run(ctx, instrumentalrecalib.Options{
		LibraryID:      env.libraryID,
		Limit:          args.Limit,
		DryRun:         !args.Yes,
		MinConfidence:  env.cfg.InstrumentalDetector.MinConfidence,
		VocalMax:       env.cfg.InstrumentalDetector.VocalMaxConfidence,
		SpeechMax:      env.cfg.InstrumentalDetector.SpeechMaxConfidence,
		CurrentVersion: version,
		Preview: func(ch instrumentalrecalib.Change) {
			switch ch.Action {
			case "settle":
				_, _ = fmt.Fprintf(out, "would settle: id=%d  %s  -> write instrumental marker + settle\n", ch.QueueID, ch.SourcePath)
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

	_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate%s: %d vocal-gate-rejected row(s) considered\n",
		env.libLabel, res.Total)
	_, _ = fmt.Fprintf(out, "reconcile-instrumental-recalibrate done: settled=%d markers-written=%d reset-stale=%d skipped(worker-claimed=%d) errors=%d\n",
		res.Settled, res.MarkersWritten, res.ResetStale, res.SkippedClaimed, res.Errors)
	if args.Yes && (res.Settled > 0 || res.ResetStale > 0) {
		_, _ = fmt.Fprintf(out, "backup of changed rows written to %s\n", backupPath)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}
