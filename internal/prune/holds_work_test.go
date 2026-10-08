package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// assertHoldingInvariant checks that the counter equals the flagged Retained rows.
func assertHoldingInvariant(t *testing.T, label string, res Result) {
	t.Helper()
	n := 0
	for _, r := range res.Retained {
		if r.HoldsWork {
			n++
		}
	}
	if n != res.RetainedHoldingWork {
		t.Errorf("%s: RetainedHoldingWork = %d but %d Retained rows carry HoldsWork", label, res.RetainedHoldingWork, n)
	}
}

// Two gone rows, one sibling: the loser's relink is declined (owned by the
// winner). A dry run reaches planRelinks, an applied run applyRelinks' decline.
// The identity-less loser WouldRetire: the dry run still reports it holding work
// (nothing is retired), the applied run retires it so it no longer holds.
func TestSweep_DeclinedRelinkHoldsWorkUnlessRetired(t *testing.T) {
	for _, dry := range []bool{true, false} {
		ctx, sqlDB, libID, root := openSeeded(t)
		dir := filepath.Join(root, "A", "B")
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "keep.mp3"), "", "")
		for _, ext := range []string{".aac", ".mp3"} {
			gone := filepath.Join(dir, "01"+ext)
			seedRow(t, ctx, sqlDB, libID, gone, "done", "failed")
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET artist_key = artist_key || ?, title_key = title_key || ? WHERE source_path = ?`, ext, ext, gone)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
		}
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(dir, "01.flac"), "", "")
		res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Retained) != 1 || res.RelinkOwned != 1 || !res.Retained[0].WouldRetire || res.Retained[0].Retired == dry {
			t.Fatalf("dry=%v: retained=%+v owned=%d, want one declined loser planned for retirement", dry, res.Retained, res.RelinkOwned)
		}
		wantHolds := dry // retired by the applied run only
		want := 0
		if wantHolds {
			want = 1
		}
		if res.Retained[0].HoldsWork != wantHolds || res.RetainedHoldingWork != want {
			t.Errorf("dry=%v: HoldsWork=%v count=%d, want %v and %d", dry, res.Retained[0].HoldsWork, res.RetainedHoldingWork, wantHolds, want)
		}
		assertHoldingInvariant(t, "declined", res)
	}
}

// The cross-library guard never retires, and the worker runs the row's own gone
// source_path, so an unsettled row holds work; a settled row does not.
func TestSweep_CrossLibraryGuardRowHoldsWorkWhenUnsettled(t *testing.T) {
	for _, status := range []string{"failed", "done"} {
		for _, dry := range []bool{true, false} {
			ctx, sqlDB, libID, root := openSeeded(t)
			otherRoot := filepath.Join(filepath.Dir(root), "other")
			lib2, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
			if err != nil {
				t.Fatal(err)
			}
			gone := filepath.Join(root, "A", "01. a.mp3")
			seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", status, "mbid-gone", "")
			id := mustWorkQueueID(t, ctx, sqlDB)
			sr2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, filepath.Join(otherRoot, "A", "01. a.mp3"), "", "")
			execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr2)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dry})
			if err != nil {
				t.Fatal(err)
			}
			holds := status == "failed"
			want := 0
			if holds {
				want = 1
			}
			if len(res.Retained) != 1 || res.Retained[0].HoldsWork != holds || res.RetainedHoldingWork != want {
				t.Errorf("status=%s dry=%v: retained=%+v count=%d, want HoldsWork=%v count=%d", status, dry, res.Retained, res.RetainedHoldingWork, holds, want)
			}
			assertHoldingInvariant(t, status, res)
		}
	}
}

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
		assertHoldingInvariant(t, "ambiguous", res)
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
