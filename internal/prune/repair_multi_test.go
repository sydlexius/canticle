package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// The B1 shape: the SAME filename in a second library whose folder is missing,
// with a scan_results file linked inside it. A real second copy is retained,
// whether library B is online or offline, and nothing is reported.
func TestRepairMulti_SecondCopyIsRetained(t *testing.T) {
	for _, offline := range []bool{false, true} {
		ctx, sqlDB, libID, root := openSeeded(t)
		b, bID := addLibraryB(t, ctx, sqlDB, root)
		second := filepath.Join(b, "amb", "New")
		id, before := multiRow(t, ctx, sqlDB, libID, root, second)
		linkScanResult(t, ctx, sqlDB, bID, id, filepath.Join(second, "01.flac"))
		if offline {
			if err := os.RemoveAll(b); err != nil {
				t.Fatal(err)
			}
		}
		reported := 0
		rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{Report: func(RepairedRow) error { reported++; return nil }})
		if err != nil || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 1 || reported != 0 {
			t.Fatalf("offline=%v rep=%+v err=%v reported=%d, want the second copy retained", offline, rep, err, reported)
		}
		if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
			t.Errorf("offline=%v output_paths changed: %s", offline, got)
		}
	}
}

// An entry with no linked file, under an online root, beside an entry that
// exists, is stale: dropped with the old and new lists in the backup record. A
// dry run reports the same and writes nothing.
func TestRepairMulti_UnlinkedStaleEntryIsDropped(t *testing.T) {
	for _, dry := range []bool{true, false} {
		ctx, sqlDB, libID, root := openSeeded(t)
		id, before := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
		var rec []RepairedRow
		rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{DryRun: dry, Report: func(r RepairedRow) error { rec = append(rec, r); return nil }})
		if err != nil || len(rep.Repaired) != 1 || rep.DroppedEntries != 1 || rep.RetainedEntries != 0 {
			t.Fatalf("dry=%v rep=%+v err=%v, want one entry dropped", dry, rep, err)
		}
		if len(rec) != 1 || len(rec[0].OldOutputPaths) != 2 || len(rec[0].NewOutputPaths) != 1 {
			t.Fatalf("dry=%v backup record = %+v, want old (2) and new (1) lists", dry, rec)
		}
		got := rawOutputPaths(t, ctx, sqlDB, id)
		if dry && got != before {
			t.Errorf("dry run wrote output_paths: %s", got)
		}
		if !dry {
			if p := decodePaths(t, got); len(p) != 1 || rec[0].NewOutputPaths[0] != p[0] {
				t.Errorf("output_paths = %s, want only the surviving entry", got)
			}
		}
	}
}

// A stale entry under an OFFLINE library root, or under no library, is never
// touched: an outage must not shrink a row.
func TestRepairMulti_OfflineOrUnconfiguredRootEntryUntouched(t *testing.T) {
	for _, name := range []string{"offline", "unconfigured"} {
		ctx, sqlDB, libID, root := openSeeded(t)
		second := filepath.Join(t.TempDir(), "elsewhere", "New") // under no library
		if name == "offline" {
			b, _ := addLibraryB(t, ctx, sqlDB, root)
			second = filepath.Join(b, "gone")
			if err := os.RemoveAll(b); err != nil {
				t.Fatal(err)
			}
		}
		id, before := multiRow(t, ctx, sqlDB, libID, root, second)
		rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
		if err != nil || len(rep.Repaired) != 0 || rep.RetainedEntries != 1 {
			t.Fatalf("%s: rep=%+v err=%v, want the entry retained", name, rep, err)
		}
		if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
			t.Errorf("%s: output_paths changed: %s", name, got)
		}
	}
}

// F4: the present == 0 guard is load-bearing. With NO entry's directory
// existing, a droppable entry (unlinked, online root) beside a retained one (a
// real second copy) must not be dropped: dropping would leave a list whose only
// entry is a retained-by-evidence path, a row the worker can then never write.
func TestRepairMulti_NoSurvivingEntryDropsNothing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	src := filepath.Join(root, "amb", "New", "01.flac")
	gone := filepath.Join(root, "amb", "Gone")
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", []models.OutputPath{
		{Outdir: filepath.Join(root, "amb", "Stale"), Filename: "01.flac"}, // droppable on its own
		{Outdir: gone, Filename: "01.flac"},                                // a real copy: a linked file lives there
	})
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, id)
	linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(gone, "01.flac"))
	if err := os.RemoveAll(gone); err != nil { // linkScanResult only records the path
		t.Fatal(err)
	}
	before := rawOutputPaths(t, ctx, sqlDB, id)
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 2 || rep.SkippedAmbiguous != 1 {
		t.Fatalf("rep=%+v err=%v, want nothing dropped, both missing entries retained, and the row counted ambiguous", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// Backup-first: a failing Report rolls the multi-entry drop back and aborts the
// pass, so no dropped entry is ever unrecorded.
func TestRepairMulti_FailedReportKeepsTheRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, before := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
	_, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{Report: func(RepairedRow) error { return os.ErrClosed }})
	if err == nil {
		t.Fatal("want the report error surfaced")
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths rewritten despite a failed report: %s", got)
	}
}

// An in-flight row belongs to the worker: its stale entry is neither dropped nor
// counted, whatever the evidence.
func TestRepairMulti_ProcessingRowSkipped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, before := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 0 {
		t.Fatalf("rep=%+v err=%v, want the processing row untouched", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// An entry whose directory EXISTS is never dropped, however stale it looks.
func TestRepairMulti_PresentEntryNeverDropped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	second := filepath.Join(root, "amb", "Present")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	id, before := multiRow(t, ctx, sqlDB, libID, root, second)
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 0 {
		t.Fatalf("rep=%+v err=%v, want nothing touched", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// Under a symlinked root the drop decides in the resolved spelling, the same as
// the worker's settle. An entry in the resolved spelling with nothing linked
// inside it is dropped; the same entry with a linked real file inside it, recorded
// in the OTHER (configured) spelling, is a real second copy and is retained.
func TestRepairMulti_SymlinkedRootAcrossSpellings(t *testing.T) {
	cases := []struct {
		name       string
		linkedFile bool
		wantDrop   int
	}{
		{"nothing-inside-is-dropped", false, 1},
		{"linked-file-other-spelling-is-retained", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, link, real := symlinkedLibrary(t)
			src := filepath.Join(real, "amb", "New", "01.flac")
			stale := filepath.Join(real, "amb", "Old")
			id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", []models.OutputPath{
				{Outdir: filepath.Dir(src), Filename: "01.flac"},
				{Outdir: stale, Filename: "01.flac"},
			})
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, id)
			if tc.linkedFile {
				linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(link, "amb", "Old", "01.flac"))
			}
			before := rawOutputPaths(t, ctx, sqlDB, id)
			rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
			if err != nil || rep.DroppedEntries != tc.wantDrop || rep.RetainedEntries != 1-tc.wantDrop {
				t.Fatalf("rep=%+v err=%v, want %d dropped", rep, err, tc.wantDrop)
			}
			got := rawOutputPaths(t, ctx, sqlDB, id)
			if tc.wantDrop == 0 && got != before {
				t.Errorf("output_paths changed: %s", got)
			}
			if tc.wantDrop == 1 {
				if p := decodePaths(t, got); len(p) != 1 || p[0].Outdir != filepath.Dir(src) {
					t.Errorf("output_paths = %s, want only the surviving entry", got)
				}
			}
		})
	}
}
