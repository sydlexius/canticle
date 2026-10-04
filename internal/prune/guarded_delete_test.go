package prune

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// A row the sweep read as gone is moved, before the delete, to the file that
// replaced its source (#1262). The delete must skip what moved, record only the
// ids it removed, and count the source in PruneSkipped.
func TestSweep_RowMovedAfterGatherIsNotDeleted(t *testing.T) {
	// repoint is the scan's own repair, fired between the gather and the delete.
	repoint := func(indexed bool) func(*testing.T, context.Context, *sql.DB, int64, string) {
		return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, gone string) {
			flac := strings.TrimSuffix(gone, ".mp3") + ".flac"
			in := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: filepath.Base(gone)}, SourcePath: flac}
			if indexed {
				seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
				in.LibraryID = libID
			} else if err := os.WriteFile(flac, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			if moved, err := queue.NewDBQueue(sqlDB).RepointGoneSource(ctx, in); err != nil || !moved {
				t.Fatalf("RepointGoneSource = (%v, %v), want the row moved", moved, err)
			}
		}
	}
	for name, tc := range map[string]struct {
		move             func(*testing.T, context.Context, *sql.DB, int64, string)
		wantWork         string // extension of the surviving queue row, "" when deleted
		wantScan         int    // scan_results rows left for the track
		recWork, recScan int    // ids in the backup record; 0/0 = no record at all
	}{
		// The repoint also removed the vanished file's scan_results row itself.
		"scan repoints the row to its indexed replacement": {move: repoint(true), wantWork: ".flac", wantScan: 1},
		// A path-only move (no scan_results row for the replacement yet) leaves the
		// vanished file's scan_results row: it is deleted, the queue row is not.
		"path-only repoint: only the scan_results row is deleted": {move: repoint(false), wantWork: ".flac", recScan: 1},
		"only the scan_results row was re-pathed": {move: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, _ int64, gone string) {
			if _, err := sqlDB.ExecContext(ctx, `UPDATE scan_results SET file_path = ? WHERE file_path = ?`, gone+".moved", gone); err != nil {
				t.Fatal(err)
			}
		}, wantScan: 1, recWork: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Artist", "Album", "01. a.mp3")
			kept := filepath.Join(root, "Zed", "01. kept.mp3") // sorts last: its stat follows the gather
			seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-nowhere", "")
			seedRow(t, ctx, sqlDB, libID, kept, "done", "done")
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			if dry, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true}); err != nil || len(dry.Pruned) != 1 || dry.PruneSkipped != 0 {
				t.Fatalf("dry run pruned=%d skipped=%d err=%v, want the plan 1/0", len(dry.Pruned), dry.PruneSkipped, err)
			}
			p, fired := New(sqlDB), false
			p.stat = func(path string) (fs.FileInfo, error) {
				if path == kept && !fired {
					fired = true
					tc.move(t, ctx, sqlDB, libID, gone)
				}
				return os.Stat(path)
			}
			var recs []PrunedRow
			res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact, Report: func(r PrunedRow) error {
				recs = append(recs, r)
				return nil
			}})
			if err != nil || !fired {
				t.Fatalf("Sweep err=%v, move fired=%v", err, fired)
			}
			if len(res.Pruned) != 1 || res.PruneSkipped != 1 || res.WorkItems != tc.recWork || res.ScanResults != tc.recScan {
				t.Errorf("planned=%d skipped=%d deleted work=%d scan=%d, want 1/1/%d/%d",
					len(res.Pruned), res.PruneSkipped, res.WorkItems, res.ScanResults, tc.recWork, tc.recScan)
			}
			var work sql.NullString
			if err := sqlDB.QueryRowContext(ctx, `SELECT max(source_path) FROM work_queue WHERE source_path != ?`, kept).Scan(&work); err != nil {
				t.Fatal(err)
			}
			if filepath.Ext(work.String) != tc.wantWork {
				t.Errorf("surviving queue row names %q, want extension %q (a row at a present file is never deleted)", filepath.Base(work.String), tc.wantWork)
			}
			if sr, _, _ := rowCounts(t, ctx, sqlDB); sr-1 != tc.wantScan {
				t.Errorf("scan_results left for the track = %d, want %d", sr-1, tc.wantScan)
			}
			if tc.recWork+tc.recScan == 0 && len(recs) != 0 {
				t.Fatalf("backup records = %+v, want none: nothing was deleted", recs)
			} else if tc.recWork+tc.recScan > 0 && (len(recs) != 1 || len(recs[0].WorkItemIDs) != tc.recWork ||
				len(recs[0].Inputs) != tc.recWork || len(recs[0].ScanResultIDs) != tc.recScan) {
				t.Errorf("backup records = %+v, want one listing %d work id(s) and %d scan id(s): only what was deleted", recs, tc.recWork, tc.recScan)
			}
		})
	}
}

// A backup failure for one deleted source must not leave the others unrecorded,
// and the one error carries a count and the first cause, never a source path.
func TestSweep_ReportFailureStillReportsRemainingRows(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	for i, name := range []string{"01. a.mp3", "02. b.mp3", "03. c.mp3"} {
		track := filepath.Join(root, "Artist", "Album", name)
		seedRowWithIdentity(t, ctx, sqlDB, libID, track, "done", "done", fmt.Sprintf("mbid-nowhere-%d", i), "")
		if err := os.Remove(track); err != nil {
			t.Fatal(err)
		}
	}
	seedRow(t, ctx, sqlDB, libID, filepath.Join(root, "Zed", "01. kept.mp3"), "done", "done")
	cause, calls := errors.New("disk full"), 0
	_, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, Report: func(PrunedRow) error {
		if calls++; calls != 2 {
			return cause
		}
		return nil
	}})
	if calls != 3 {
		t.Errorf("report called %d time(s), want 3: a failure must not stop the rest", calls)
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "2 of 3 deleted source(s) not recorded") {
		t.Errorf("err = %v, want one error counting 2 of 3 and wrapping the first cause", err)
	}
	if err != nil && strings.Contains(err.Error(), root) {
		t.Errorf("err = %v names a library path", err)
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Errorf("scan_results=%d work_queue=%d, want 1/1: the deletes had committed", sr, wq)
	}
}
