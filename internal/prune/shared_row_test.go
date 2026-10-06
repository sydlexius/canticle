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

// An unscoped sweep whose gone source has no scan_results row of its own (the
// candidate comes from work_queue.source_path alone, so its library is unknown)
// still keeps a queue row linked to a present file in another library (#1293).
func TestSweep_SharedRowWithUnknownOwnLibraryIsKept(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	otherRoot := filepath.Join(filepath.Dir(root), "other")
	lib2, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "A", "01. a.mp3")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-gone", "")
	id := mustWorkQueueID(t, ctx, sqlDB)
	sr2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, filepath.Join(otherRoot, "A", "01. a.mp3"), "", "")
	execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr2)
	// No scan_results row names the gone path any more; the identity that
	// classifies it for deletion comes from the queue row itself.
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET mbid = 'mbid-gone' WHERE id = ?`, id)
	execWQ(t, ctx, sqlDB, `DELETE FROM scan_results WHERE file_path = ?`, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	res := sweepExact(t, ctx, sqlDB)
	if len(res.Pruned) != 0 || len(res.Retained) != 1 {
		t.Errorf("pruned=%d retained=%d, want 0 and 1", len(res.Pruned), len(res.Retained))
	}
	if _, wq, j := rowCounts(t, ctx, sqlDB); wq != 1 || j != 1 {
		t.Errorf("work_queue=%d junction=%d, want 1 and 1", wq, j)
	}
}

// The preflight read can lose a race with a scan that links another library's
// present file; the delete transaction re-checks and keeps the row whole.
func TestDeletePrunedTx_RechecksCrossLibraryLink(t *testing.T) {
	for _, link := range []bool{true, false} {
		ctx, sqlDB, libID, root := openSeeded(t)
		otherRoot := filepath.Join(filepath.Dir(root), "other")
		lib2, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
		if err != nil {
			t.Fatal(err)
		}
		gone := filepath.Join(root, "A", "01. a.mp3")
		srID := seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "", "")
		id := mustWorkQueueID(t, ctx, sqlDB)
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		row := PrunedRow{SourcePath: gone, ScanResultIDs: []int64{srID}, WorkItemIDs: []int64{id},
			Inputs: make([]models.Inputs, 1), States: make([]WorkState, 1), guard: &linkGuard{lib: &libID}}
		if link { // the scan lands after the preflight, before the transaction
			sr2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, filepath.Join(otherRoot, "A", "01. a.mp3"), "", "")
			execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr2)
		}
		_, wd, skipped, _, err := New(sqlDB).deletePrunedTx(ctx, []PrunedRow{row})
		if err != nil {
			t.Fatal(err)
		}
		_, wq, _ := rowCounts(t, ctx, sqlDB)
		if link && (wd != 0 || skipped != 1 || wq != 1) {
			t.Errorf("linked: workDeleted=%d skipped=%d work_queue=%d, want 0, 1, 1", wd, skipped, wq)
		}
		if !link && (wd != 1 || skipped != 0 || wq != 0) {
			t.Errorf("unlinked: workDeleted=%d skipped=%d work_queue=%d, want 1, 0, 0", wd, skipped, wq)
		}
	}
}
