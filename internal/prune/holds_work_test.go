package prune

import (
	"os"
	"path/filepath"
	"testing"
)

// #1430 question 1: a gone row whose identity matches several present files is
// retained and, being unsettled work, stays dequeue-eligible after an applied
// run. The result now says so, in a dry run and an applied one alike.
func TestSweep_AmbiguousIdentityRowKeepsWorkAndIsCounted(t *testing.T) {
	for _, dry := range []bool{true, false} {
		ctx, sqlDB, libID, root := openSeeded(t)
		gone := filepath.Join(root, "ArtistH", "Old", "01. track.flac")
		seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "failed", "failed", "mbid-shared", "")
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "ArtistH", "D1", "01. track.flac"), "mbid-shared", "")
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "ArtistH", "D2", "01. track.flac"), "mbid-shared", "")

		res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if res.RetainedHoldingWork != 1 || len(res.Retained) != 1 || !res.Retained[0].HoldsWork || res.Retained[0].Retired {
			t.Errorf("dry=%v: holding=%d retained=%+v, want one retained row holding work", dry, res.RetainedHoldingWork, res.Retained)
		}
		if st, _ := statusOf(t, ctx, sqlDB, gone); st != "failed" {
			t.Errorf("dry=%v: status = %q, want failed (still dequeue-eligible)", dry, st)
		}
	}
}

// An identity-less row that is retired no longer holds work, so it is not counted.
func TestSweep_RetiredRowIsNotCountedAsHoldingWork(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistG", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatal(err)
	}
	if res.RetainedHoldingWork != 0 {
		t.Errorf("RetainedHoldingWork = %d, want 0 after a committed retirement", res.RetainedHoldingWork)
	}
}

// #1430 group-1 evidence: a gone scan_results row with no identity and no queue
// row is retained ("left untouched") by the attended Exact run and not deleted.
func TestSweep_ScanResultOnlyGoneRowSurvivesAttendedRun(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistS", "01. gone.flac")
	seedPresentScanResult(t, ctx, sqlDB, libID, gone, "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Retained) != 1 || len(res.Pruned) != 0 || res.RetainedHoldingWork != 0 {
		t.Fatalf("retained=%d pruned=%d holding=%d, want 1/0/0", len(res.Retained), len(res.Pruned), res.RetainedHoldingWork)
	}
	if sr, _, _ := rowCounts(t, ctx, sqlDB); sr != 1 {
		t.Errorf("scan_results = %d, want 1 (kept forever)", sr)
	}
}
