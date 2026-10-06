package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// One queue row linked to files in two libraries: when the named file is gone
// and the other library's file is present, the row survives every sweep shape
// and the other library's scan_results row and link are untouched (#1293).
func TestSweep_SharedRowWithPresentFileInOtherLibraryIsKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scoped bool
		gran   Granularity
	}{
		{"exact unscoped", false, Exact},
		{"exact scoped", true, Exact},
		{"directory unscoped", false, Directory},
	} {
		for _, dry := range []bool{true, false} {
			ctx, sqlDB, libID, root := openSeeded(t)
			otherRoot := filepath.Join(filepath.Dir(root), "other")
			lib2, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
			if err != nil {
				t.Fatal(err)
			}
			gone := filepath.Join(root, "A", "01. a.mp3")
			seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-gone", "")
			id := mustWorkQueueID(t, ctx, sqlDB)
			present := filepath.Join(otherRoot, "A", "01. a.mp3")
			sr2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, present, "", "")
			execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr2)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			opts := SweepOptions{Granularity: tc.gran, DryRun: dry}
			if tc.scoped {
				opts.LibraryID = &libID
			}
			res, err := New(sqlDB).Sweep(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Pruned) != 0 || len(res.Retained) != 1 {
				t.Errorf("%s dry=%v: pruned=%d retained=%d, want 0 and 1", tc.name, dry, len(res.Pruned), len(res.Retained))
			}
			if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 1 {
				t.Errorf("%s dry=%v: work_queue = %d, want 1 (shared row survives)", tc.name, dry, wq)
			}
			var n int
			if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM work_queue_scan_results j JOIN scan_results sr ON sr.id = j.scan_result_id WHERE sr.id = ?`, sr2).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s dry=%v: other library's link count = %d (err %v), want 1", tc.name, dry, n, err)
			}
		}
	}
}

// When the other library's file is gone too, nothing is shared with a present
// file and the row is deleted as before.
func TestSweep_SharedRowWithGoneFileInOtherLibraryIsDeleted(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	otherRoot := filepath.Join(filepath.Dir(root), "other")
	lib2, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "A", "01. a.mp3")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-gone", "")
	id := mustWorkQueueID(t, ctx, sqlDB)
	other := filepath.Join(otherRoot, "A", "01. a.mp3")
	sr2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, other, "", "")
	execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr2)
	for _, p := range []string{gone, other} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	res := sweepExact(t, ctx, sqlDB)
	if len(res.Pruned) != 1 {
		t.Fatalf("pruned=%d retained=%d, want 1 pruned", len(res.Pruned), len(res.Retained))
	}
	if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 0 {
		t.Errorf("work_queue = %d, want 0", wq)
	}
}
