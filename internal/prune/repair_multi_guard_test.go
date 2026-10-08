package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// F1: a sub-share that went away under an ONLINE root must not read as a stale
// path. The root answers online, so only the walk from the entry up toward the
// root can see it.
func TestRepairMulti_VanishedSubtreeUnderOnlineRootIsNotStale(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string) string // returns the second entry's directory
	}{
		{"dangling-symlinked-intermediate", func(t *testing.T, root string) string {
			if err := os.MkdirAll(filepath.Join(root, "amb"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "amb", "Mnt")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "amb", "Mnt", "Album")
		}},
		{"empty-existing-intermediate", func(t *testing.T, root string) string {
			if err := os.MkdirAll(filepath.Join(root, "amb", "Mnt"), 0o755); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "amb", "Mnt", "Album")
		}},
		{"live-symlinked-intermediate-above-a-real-parent", func(t *testing.T, root string) string {
			target := filepath.Join(filepath.Dir(root), "elsewhere")
			if err := os.MkdirAll(filepath.Join(target, "Sub", "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "link", "Sub", "Album")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			src := filepath.Join(root, "amb", "New", "01.flac")
			second := tc.setup(t, root)
			id, before := multiRow(t, ctx, sqlDB, libID, root, second)
			rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
			if err != nil || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 1 {
				t.Fatalf("rep=%+v err=%v, want the entry retained", rep, err)
			}
			if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
				t.Errorf("output_paths changed: %s", got)
			}
			// The worker-facing predicate refuses the same entry.
			stale, err := New(sqlDB).EntryProvablyStale(ctx, id, src, models.OutputPath{Outdir: second, Filename: "01.flac"}, func(string) bool { return true })
			if err != nil || stale {
				t.Errorf("EntryProvablyStale = %v err=%v, want false", stale, err)
			}
		})
	}
}

// The guard must not disable the feature: a populated real parent with the entry
// directory simply gone, nothing linked, is still dropped.
func TestRepairMulti_PopulatedParentMissingEntryStillDropped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, _ := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old", "Disc1"))
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || rep.DroppedEntries != 1 || rep.RetainedEntries != 0 {
		t.Fatalf("rep=%+v err=%v, want the stale entry dropped", rep, err)
	}
	if p := decodePaths(t, rawOutputPaths(t, ctx, sqlDB, id)); len(p) != 1 {
		t.Errorf("output_paths has %d entries, want 1", len(p))
	}
}

// F2: a row that goes in flight between plan and write is raced, and its plan
// counts must not reach the result.
func TestRepairMulti_RacedRowContributesNoCounts(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, before := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{beforeWrite: func(int64) {
		execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
	}})
	if err != nil || rep.SkippedRaced != 1 || len(rep.Repaired) != 0 || rep.DroppedEntries != 0 || rep.RetainedEntries != 0 {
		t.Fatalf("rep=%+v err=%v, want raced=1 and no counted entries", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// F3a: an existing NON-directory entry is not a surviving destination.
func TestRepairMulti_NonDirectoryEntryIsNotASurvivor(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	src := filepath.Join(root, "amb", "New", "01.flac")
	file := filepath.Join(root, "amb", "AFile")
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", []models.OutputPath{
		{Outdir: file, Filename: "01.flac"},
		{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"},
	})
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, id)
	before := rawOutputPaths(t, ctx, sqlDB, id)
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || rep.DroppedEntries != 0 || rep.SkippedAmbiguous != 1 || len(rep.Repaired) != 0 {
		t.Fatalf("rep=%+v err=%v, want nothing dropped and the row ambiguous", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// F3b: a stat error other than not-exist on an entry refuses the whole row.
func TestRepairMulti_EntryStatErrorRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	ctx, sqlDB, libID, root := openSeeded(t)
	locked := filepath.Join(root, "amb", "Locked")
	if err := os.MkdirAll(filepath.Join(locked, "Inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	id, before := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
	// Add a third entry whose parent is unreadable: its stat fails with EACCES.
	paths := decodePaths(t, before)
	paths = append(paths, models.OutputPath{Outdir: filepath.Join(locked, "Inner"), Filename: "01.flac"})
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET output_paths = ? WHERE id = ?`, mustJSON(t, paths), id)
	before = rawOutputPaths(t, ctx, sqlDB, id)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	rep, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || rep.DroppedEntries != 0 || rep.SkippedStatError != 1 || len(rep.Repaired) != 0 {
		t.Fatalf("rep=%+v err=%v, want the row refused on the stat error", rep, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("output_paths changed: %s", got)
	}
}

// subtreeIntact on its own: a file as the nearest existing ancestor is not
// provable (the repair pass refuses such a row earlier, on the entry's ENOTDIR
// stat, so this is the predicate's own backstop for the worker-facing caller).
func TestSubtreeIntact_FileAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Mnt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if subtreeIntact(root, filepath.Join(root, "Mnt", "Album")) {
		t.Error("subtreeIntact = true under a file ancestor, want false")
	}
	if err := os.MkdirAll(filepath.Join(root, "Real", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !subtreeIntact(root, filepath.Join(root, "Real", "Gone")) {
		t.Error("subtreeIntact = false for a populated real parent, want true")
	}
}
