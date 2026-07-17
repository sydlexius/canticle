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

	"github.com/doxazo-net/canticle/internal/instrumentalbackfill"
	"github.com/doxazo-net/canticle/internal/lyrics"
)

// reconcileInstrumentalBackup is one JSONL record per row the backfill settles,
// written and fsynced before the row is mutated so an applied change always has
// its restorable record.
type reconcileInstrumentalBackup struct {
	QueueID     int64     `json:"queue_id"`
	Artist      string    `json:"artist"`
	Title       string    `json:"title"`
	SourcePath  string    `json:"source_path"`
	MarkerPaths []string  `json:"marker_paths"`
	MusicSum    float64   `json:"music_sum"`
	VocalPeak   float64   `json:"vocal_peak"`
	SpeechMean  float64   `json:"speech_mean"`
	VocalClass  string    `json:"vocal_class"`
	Detector    string    `json:"detector_version"`
	At          time.Time `json:"at"`
}

func appendReconcileInstrumentalBackup(f *os.File, ch instrumentalbackfill.Change) error {
	rec := reconcileInstrumentalBackup{
		QueueID:     ch.QueueID,
		Artist:      ch.Artist,
		Title:       ch.Title,
		SourcePath:  ch.SourcePath,
		MarkerPaths: ch.MarkerPaths,
		MusicSum:    ch.Telemetry.MusicSum,
		VocalPeak:   ch.Telemetry.VocalPeak,
		SpeechMean:  ch.Telemetry.SpeechMean,
		VocalClass:  ch.Telemetry.VocalClass,
		Detector:    ch.Telemetry.DetectorVersion,
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

// runReconcileInstrumental is CLI wiring over internal/instrumentalbackfill: it
// resolves config/detector/queue, owns the JSONL backup file and the operator
// output, and lets the package own the classification logic. Dry-run unless --yes.
func runReconcileInstrumental(ctx context.Context, out io.Writer, args ScanReconcileInstrumentalCmd) int {
	env, code := openDetectorEnv(ctx, out, args.ConfigPath, args.Library, "backfill instrumental verdicts")
	if env == nil {
		return code
	}
	defer env.Close()

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(env.cfg.DB.Path), fmt.Sprintf("reconcile-instrumental-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backup *os.File
	defer func() {
		if backup != nil {
			_ = backup.Close() //nolint:errcheck // reason: best-effort close on command exit
		}
	}()

	if !args.Yes {
		_, _ = fmt.Fprintf(out, "reconcile-instrumental%s: dry run; pass --yes to apply\n", env.libLabel)
	}

	bf := instrumentalbackfill.New(env.queue, env.detector, lyrics.NewLRCWriter())
	res, err := bf.Run(ctx, instrumentalbackfill.Options{
		LibraryID:           env.libraryID,
		Limit:               args.Limit,
		DryRun:              !args.Yes,
		GlobalDetectDefault: env.cfg.InstrumentalDetector.Enabled,
		Preview: func(ch instrumentalbackfill.Change) {
			_, _ = fmt.Fprintf(out, "would mark: id=%d  %s  -> write instrumental marker + settle\n", ch.QueueID, ch.SourcePath)
		},
		Report: func(ch instrumentalbackfill.Change) error {
			if backup == nil {
				f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
				if ferr != nil {
					return fmt.Errorf("open backup file %s: %w", backupPath, ferr)
				}
				backup = f
			}
			return appendReconcileInstrumentalBackup(backup, ch)
		},
	})
	if err != nil {
		slog.Error("reconcile-instrumental failed", "error", err)
		return 1
	}

	_, _ = fmt.Fprintf(out, "reconcile-instrumental%s: %d never-classified deferred row(s) total; %d candidate(s) to classify\n",
		env.libLabel, res.Total, res.Candidates)
	// Never let a cap read as full coverage.
	if args.Limit > 0 && res.Total > res.Candidates {
		_, _ = fmt.Fprintf(out, "note: --limit=%d caps this run; %d row(s) left unexamined\n", args.Limit, res.Total-res.Candidates)
	}
	_, _ = fmt.Fprintf(out, "reconcile-instrumental done: checked=%d instrumental=%d not-instrumental=%d markers-written=%d rows-settled=%d skipped(detect-off=%d,no-source=%d) errors=%d\n",
		res.Checked, res.Instrumental, res.NotInstrumental, res.MarkersWritten, res.RowsSettled, res.SkippedDetectOff, res.SkippedNoSource, res.Errors)
	if args.Yes && res.RowsSettled > 0 {
		_, _ = fmt.Fprintf(out, "backup of classified rows written to %s\n", backupPath)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}
