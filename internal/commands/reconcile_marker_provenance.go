package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/instrumentalbackfill"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
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

	var stamped, skipped, errored int
	for _, r := range rows {
		for _, p := range instrumentalbackfill.MarkerPaths(r.Inputs) {
			// Match WriteMarkerProvenance's eligibility exactly so dry-run previews
			// only what apply will stamp: it Lstat-skips symlinks, whereas
			// ReadInstrumentalProvenance follows them.
			fi, lerr := os.Lstat(p)
			if lerr != nil {
				if errors.Is(lerr, os.ErrNotExist) {
					// Marker absent -- nothing to backfill at this path.
					skipped++
					continue
				}
				slog.Warn("reconcile-marker-provenance: lstat failed", "path", p, "error", lerr)
				errored++
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				// WriteMarkerProvenance is a no-op on symlinks; skip to keep dry-run honest.
				skipped++
				continue
			}
			prov, isMarker, rerr := lyrics.ReadInstrumentalProvenance(p)
			if rerr != nil {
				if errors.Is(rerr, os.ErrNotExist) {
					// Vanished between the Lstat and the read (e.g. a concurrent
					// prune) -- same benign "absent" case as the Lstat branch.
					skipped++
					continue
				}
				slog.Warn("reconcile-marker-provenance: read failed", "path", p, "error", rerr)
				errored++
				continue
			}
			if !isMarker || prov.Source != "" {
				// Not a bare marker (already headed, or not a marker) -- skip.
				skipped++
				continue
			}
			_, _ = fmt.Fprintf(out, "  %s -> [source:%s]%s\n", p, lyrics.SourceDetector, dvNote(r.DetectorVersion))
			if !args.Yes {
				stamped++
				continue
			}
			// Write-ahead: secure the backup record before mutating the marker, so a
			// stamped file is never left unrecorded (a rerun idempotently skips it and
			// could not recreate the record). The JSONL is therefore a superset of the
			// files actually mutated: a record whose stamp then no-ops or errors is
			// harmless, since restore just strips the additive header (a no-op on an
			// unstamped file) and a rerun re-stamps and re-records.
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
			changed, werr := lyrics.WriteMarkerProvenance(p, lyrics.InstrumentalProvenance{
				Source:          lyrics.SourceDetector,
				DetectorVersion: r.DetectorVersion,
			})
			if werr != nil {
				slog.Warn("reconcile-marker-provenance: stamp failed", "path", p, "error", werr)
				errored++
				continue
			}
			if !changed {
				skipped++
				continue
			}
			stamped++
		}
	}

	verb := "would stamp"
	if args.Yes {
		verb = "stamped"
	}
	_, _ = fmt.Fprintf(out, "reconcile-marker-provenance: scanned %d detector marker row(s)%s; %s %d, skipped %d, errored %d%s\n",
		len(rows), env.libLabel, verb, stamped, skipped, errored, suffixDryRun(args.Yes))
	if backupFile != nil {
		_, _ = fmt.Fprintf(out, "backup written to %s\n", backupPath)
	}
	if errored > 0 {
		return 1
	}
	return 0
}
