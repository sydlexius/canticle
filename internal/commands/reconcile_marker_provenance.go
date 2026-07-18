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
	"github.com/doxazo-net/canticle/internal/queue"
)

type markerProvenanceBackupRecord struct {
	FilePath        string `json:"file_path"`
	Source          string `json:"source"`
	DetectorVersion string `json:"detector_version,omitempty"`
}

func appendMarkerProvenanceBackup(f *os.File, rec markerProvenanceBackupRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal marker-provenance backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write marker-provenance backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync marker-provenance backup record: %w", err)
	}
	return nil
}

func dvNote(dv string) string {
	if dv == "" {
		return ""
	}
	return fmt.Sprintf(" [dv:%s]", dv)
}

// runReconcileMarkerProvenance backfills [source:canticle-detector]/[dv:] headers
// onto bare detector-written instrumental markers (#502), so the scanner treats
// them as provisional/re-checkable rather than terminal. Dry-run by default;
// --yes applies and appends a JSONL backup of each stamped file.
func runReconcileMarkerProvenance(ctx context.Context, out io.Writer, args ScanReconcileMarkerProvenanceCmd) int {
	env, code := openQueueEnv(ctx, out, args.ConfigPath, args.Library)
	if env == nil {
		return code
	}
	defer env.Close()

	rows, err := env.queue.ListDetectorInstrumentalMarkers(ctx, queue.ListInstrumentalMarkersOptions{
		LibraryID: env.libraryID,
		Limit:     args.Limit,
	})
	if err != nil {
		slog.Error("reconcile-marker-provenance: list failed", "error", err)
		return 1
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(env.cfg.DB.Path),
			fmt.Sprintf("reconcile-marker-provenance-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	defer func() {
		if backupFile != nil {
			if cerr := backupFile.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-marker-provenance backup file", "path", backupPath, "error", cerr)
			}
		}
	}()

	var stamped, skipped int
	for _, r := range rows {
		for _, p := range instrumentalbackfill.MarkerPaths(r.Inputs) {
			prov, isMarker, rerr := lyrics.ReadInstrumentalProvenance(p)
			if rerr != nil {
				// Marker absent or unreadable -- nothing to backfill at this path.
				continue
			}
			if !isMarker || prov.Source != "" {
				// Not a bare marker (already headed, or not a marker) -- skip.
				continue
			}
			_, _ = fmt.Fprintf(out, "  %s -> [source:%s]%s\n", p, lyrics.SourceDetector, dvNote(r.DetectorVersion))
			if !args.Yes {
				stamped++
				continue
			}
			changed, werr := lyrics.WriteMarkerProvenance(p, lyrics.InstrumentalProvenance{
				Source:          lyrics.SourceDetector,
				DetectorVersion: r.DetectorVersion,
			})
			if werr != nil {
				slog.Warn("reconcile-marker-provenance: stamp failed", "path", p, "error", werr)
				continue
			}
			if !changed {
				skipped++
				continue
			}
			stamped++
			if backupFile == nil {
				f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied or derived from the configured db dir
				if ferr != nil {
					slog.Error("reconcile-marker-provenance: open backup failed", "path", backupPath, "error", ferr)
					return 1
				}
				backupFile = f
			}
			if berr := appendMarkerProvenanceBackup(backupFile, markerProvenanceBackupRecord{
				FilePath:        p,
				Source:          lyrics.SourceDetector,
				DetectorVersion: r.DetectorVersion,
			}); berr != nil {
				slog.Error("reconcile-marker-provenance: write backup failed", "error", berr)
				return 1
			}
		}
	}

	verb := "would stamp"
	if args.Yes {
		verb = "stamped"
	}
	_, _ = fmt.Fprintf(out, "reconcile-marker-provenance: scanned %d detector marker row(s)%s; %s %d, skipped %d%s\n",
		len(rows), env.libLabel, verb, stamped, skipped, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup written to %s\n", backupPath)
	}
	return 0
}
