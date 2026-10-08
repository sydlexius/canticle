package prune

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func decodePaths(t *testing.T, raw string) []models.OutputPath {
	t.Helper()
	var p []models.OutputPath
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return p
}

// healCase seeds a processing row with the given entries and returns a closure
// that runs HealOutputPaths against them.
func healCase(t *testing.T, entries func(root string) []models.OutputPath) (run func() ([]models.OutputPath, bool), src string, rawNow func() string, setStatus func(string)) {
	t.Helper()
	ctx, sqlDB, libID, root := openSeeded(t)
	src = filepath.Join(root, "amb", "New", "01.flac")
	paths := entries(root)
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", paths)
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
	p := New(sqlDB)
	run = func() ([]models.OutputPath, bool) {
		got, healed, err := p.HealOutputPaths(ctx, id, src, filepath.Dir(src), "01.flac", paths)
		if err != nil {
			t.Fatal(err)
		}
		return got, healed
	}
	rawNow = func() string { return rawOutputPaths(t, ctx, sqlDB, id) }
	setStatus = func(s string) { execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = ? WHERE id = ?`, s, id) }
	return run, src, rawNow, setStatus
}

func staleOne(root string) []models.OutputPath {
	return []models.OutputPath{{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"}}
}

// The #921 shape heals and persists.
func TestHealOutputPaths_SingleEntryPersists(t *testing.T) {
	run, src, raw, _ := healCase(t, staleOne)
	got, healed := run()
	if !healed || len(got) != 1 || got[0].Outdir != filepath.Dir(src) {
		t.Fatalf("heal = %+v healed=%v, want the row's own directory", got, healed)
	}
	if p := decodePaths(t, raw()); len(p) != 1 || p[0].Outdir != filepath.Dir(src) {
		t.Errorf("persisted output_paths = %s, want the corrected entry", raw())
	}
}

// The correction is proven only while the source audio exists.
func TestHealOutputPaths_NeedsTheSourceFile(t *testing.T) {
	run, src, raw, _ := healCase(t, staleOne)
	before := raw()
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if _, healed := run(); healed {
		t.Fatal("healed with the source gone")
	}
	if raw() != before {
		t.Errorf("output_paths changed: %s", raw())
	}
}

// E1: the compare-and-set is limited to the row the worker holds. A row that is
// no longer processing is not healed.
func TestHealOutputPaths_OnlyWhileProcessing(t *testing.T) {
	run, _, raw, setStatus := healCase(t, staleOne)
	before := raw()
	setStatus("done")
	if _, healed := run(); healed {
		t.Fatal("healed a row that is not processing")
	}
	if raw() != before {
		t.Errorf("output_paths changed: %s", raw())
	}
}

// A multi-entry row is never rewritten or shrunk, whatever the entries are.
func TestHealOutputPaths_MultiEntryUntouched(t *testing.T) {
	run, _, raw, _ := healCase(t, func(root string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"},
			{Outdir: filepath.Join(root, "amb", "New"), Filename: "01.flac"},
		}
	})
	before := raw()
	if _, healed := run(); healed {
		t.Fatal("a multi-entry row was healed")
	}
	if raw() != before {
		t.Errorf("output_paths changed: %s", raw())
	}
}

// F2: an entry in ANOTHER library is a second copy there, not this row's stale
// path. The heal rewrites only an entry under the same root as the row's source.
func TestHealOutputPaths_EntryInAnotherLibraryIsNotHealed(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	b := addLibraryB(t, ctx, sqlDB, root)
	src := filepath.Join(root, "amb", "New", "01.flac")
	paths := []models.OutputPath{{Outdir: filepath.Join(b, "amb", "Old"), Filename: "01.flac"}}
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", paths)
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
	before := rawOutputPaths(t, ctx, sqlDB, id)
	got, healed, err := New(sqlDB).HealOutputPaths(ctx, id, src, filepath.Dir(src), "01.flac", paths)
	if err != nil || healed || len(got) != 1 || got[0] != paths[0] {
		t.Fatalf("heal = %+v healed=%v err=%v, want the cross-library entry left alone", got, healed, err)
	}
	if after := rawOutputPaths(t, ctx, sqlDB, id); after != before {
		t.Errorf("output_paths changed: %s", after)
	}
}
