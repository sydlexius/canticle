package commands

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/instrumentalbackfill"
	"github.com/sydlexius/canticle/internal/lyrics"
)

// reconcileInstrumentalBackup is one JSONL record per row the backfill settles,
// written and fsynced before the row is mutated so an applied change always has
// its restorable record.
type reconcileInstrumentalBackup struct {
	// Type discriminates the intent record (this shape) from the outcome record
	// (reconcileInstrumentalOutcome) in the same JSONL. Absent on records written
	// before #515; a reader must treat a missing Type as "intent".
	Type       string `json:"type,omitempty"`
	QueueID    int64  `json:"queue_id"`
	Artist     string `json:"artist"`
	Title      string `json:"title"`
	SourcePath string `json:"source_path"`
	// Instrumental records WHICH mutation this row underwent: true settled it and
	// wrote the markers below; false stamped instrumental_result=0 and left it
	// deferred. Both are recorded -- a negative stamp also removes the row from
	// every future backfill, so it needs a restorable record just as much.
	Instrumental bool      `json:"instrumental"`
	MarkerPaths  []string  `json:"marker_paths,omitempty"`
	MusicSum     float64   `json:"music_sum"`
	VocalPeak    float64   `json:"vocal_peak"`
	SpeechMean   float64   `json:"speech_mean"`
	VocalClass   string    `json:"vocal_class"`
	Detector     string    `json:"detector_version"`
	At           time.Time `json:"at"`
}

// reconcileInstrumentalOutcome is the second JSONL record for a row: the realized
// result appended AFTER the mutation resolves, so a restore replays only rows
// whose outcome is "applied" (#515). Mirrors the recalibrate sibling's outcome
// record so both share the backup-trail contract.
type reconcileInstrumentalOutcome struct {
	Type      string    `json:"type"` // always "outcome"
	QueueID   int64     `json:"queue_id"`
	Outcome   string    `json:"outcome"`             // applied | skipped | failed
	Ambiguous bool      `json:"ambiguous,omitempty"` // failed-but-maybe-applied settle
	At        time.Time `json:"at"`
}

func appendReconcileInstrumentalBackup(f *os.File, ch instrumentalbackfill.Change) error {
	rec := reconcileInstrumentalBackup{
		Type:         "intent",
		QueueID:      ch.QueueID,
		Artist:       ch.Artist,
		Title:        ch.Title,
		SourcePath:   ch.SourcePath,
		Instrumental: ch.Instrumental,
		MarkerPaths:  ch.MarkerPaths,
		MusicSum:     ch.Telemetry.MusicSum,
		VocalPeak:    ch.Telemetry.VocalPeak,
		SpeechMean:   ch.Telemetry.SpeechMean,
		VocalClass:   ch.Telemetry.VocalClass,
		Detector:     ch.Telemetry.DetectorVersion,
		At:           time.Now().UTC(),
	}
	// appendJSONLSynced is shared with the recalibrate sibling (defined in
	// reconcile_instrumental_recalibrate.go): both records get identical
	// marshal+append+fsync durability (the identityrepair backup-first rule).
	return appendJSONLSynced(f, rec)
}

// appendReconcileInstrumentalOutcome appends the realized-outcome record for a
// row, fsynced like the intent record so the trail is crash-durable (#515).
func appendReconcileInstrumentalOutcome(f *os.File, o instrumentalbackfill.Outcome) error {
	return appendJSONLSynced(f, reconcileInstrumentalOutcome{
		Type:      "outcome",
		QueueID:   o.QueueID,
		Outcome:   o.Status,
		Ambiguous: o.Ambiguous,
		At:        time.Now().UTC(),
	})
}

// runReconcileInstrumental is CLI wiring over internal/instrumentalbackfill: it
// resolves config/detector/queue, owns the JSONL backup file and the operator
// output, and lets the package own the classification logic. Dry-run unless --yes.
func runReconcileInstrumental(ctx context.Context, out io.Writer, args ScanReconcileInstrumentalCmd) int {
	env, code := openDetectorEnv(ctx, out, args.ConfigPath, args.Library, "backfill instrumental verdicts", args.Yes)
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
		Outcome: func(o instrumentalbackfill.Outcome) error {
			// A nil handle means Report failed to open the backup; the engine
			// already reports that row as failed, so there is nothing to append to.
			if backup == nil {
				return nil
			}
			return appendReconcileInstrumentalOutcome(backup, o)
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
	// Two axes, reported separately: what the detector heard, then what happened to
	// the rows. Folding them into one line is how a claimed row silently looked like
	// a changed verdict.
	_, _ = fmt.Fprintf(out, "reconcile-instrumental verdicts: checked=%d instrumental=%d not-instrumental=%d\n",
		res.Checked, res.Instrumental, res.NotInstrumental)
	_, _ = fmt.Fprintf(out, "reconcile-instrumental done: markers-written=%d rows-settled=%d rows-stamped=%d skipped(detect-off=%d,no-source=%d,worker-claimed=%d,peer-settled=%d) errors=%d\n",
		res.MarkersWritten, res.RowsSettled, res.RowsStamped, res.SkippedDetectOff, res.SkippedNoSource, res.SkippedClaimed, res.SkippedAlreadySettled, res.Errors)
	if args.Yes && (res.RowsSettled > 0 || res.RowsStamped > 0) {
		_, _ = fmt.Fprintf(out, "backup of classified rows written to %s\n", backupPath)
	}
	if res.Errors > 0 {
		return 1
	}
	return 0
}
