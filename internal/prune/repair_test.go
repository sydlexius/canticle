package prune

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// seedStale seeds the live-production #921 shape: a row whose outdir/filename
// already name root/name/New (a prior relink moved them; the file exists) while
// its single output_paths entry still names the vanished root/name/Old. No
// scan_results row exists for Old, so Sweep can never re-select it.
func seedStale(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, root, name, status string) (int64, string) {
	t.Helper()
	newPath := filepath.Join(root, name, "New", "01.flac")
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, newPath, name,
		[]models.OutputPath{{Outdir: filepath.Join(root, name, "Old"), Filename: "01.flac"}})
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = ? WHERE id = ?`, status, id)
	return id, filepath.Dir(newPath)
}

func execWQ(t *testing.T, ctx context.Context, sqlDB *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := sqlDB.ExecContext(ctx, stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func rawOutputPaths(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64) string {
	t.Helper()
	var raw string
	if err := sqlDB.QueryRowContext(ctx, `SELECT output_paths FROM work_queue WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("query output_paths: %v", err)
	}
	return raw
}

// The "noncanonical" case pins the compare-and-set to the raw scanned column:
// JSON Go would not re-encode byte-for-byte must still be repaired, not raced.
func TestRepairOutputPaths_FixesPreBrokenRelinkRow(t *testing.T) {
	for _, noncanonical := range []bool{false, true} {
		ctx, sqlDB, libID, root := openSeeded(t)
		id, newDir := seedStale(t, ctx, sqlDB, libID, root, "fix", "failed")
		if noncanonical {
			old, _ := json.Marshal(filepath.Join(root, "fix", "Old"))
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET output_paths = ? WHERE id = ?`, `[ {"filename": "01.flac", "outdir": `+string(old)+`} ]`, id)
		}
		reported := 0
		res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{Report: func(RepairedRow) error { reported++; return nil }})
		if err != nil || len(res.Repaired) != 1 || res.Repaired[0].WorkItemID != id || reported != 1 {
			t.Fatalf("noncanonical=%v res=%+v err=%v reported=%d, want row %d repaired and reported once", noncanonical, res, err, reported, id)
		}
		want := models.OutputPath{Outdir: newDir, Filename: "01.flac"}
		if got := workQueueOutputPaths(t, ctx, sqlDB, id); len(got) != 1 || got[0] != want {
			t.Errorf("output_paths = %+v, want [%+v]", got, want)
		}
	}
}

func TestRepairOutputPaths_DryRunWritesNothing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, _ := seedStale(t, ctx, sqlDB, libID, root, "dry", "failed")
	before := rawOutputPaths(t, ctx, sqlDB, id)
	res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{DryRun: true})
	if err != nil || len(res.Repaired) != 1 {
		t.Fatalf("res=%+v err=%v, want 1 would-repair row", res, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("dry run mutated output_paths: %s -> %s", before, got)
	}
}

// TestRepairOutputPaths_LeavesNonMatchingRowsAlone: every row outside the
// exact #921 shape is left byte-for-byte unchanged, and counted where a count
// exists.
func TestRepairOutputPaths_LeavesNonMatchingRowsAlone(t *testing.T) {
	type setupFn func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, root, newDir string)
	setPaths := func(paths string) setupFn {
		return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, _, _ string) {
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET output_paths = ? WHERE id = ?`, paths, id)
		}
	}
	mustJSON := func(p []models.OutputPath) string { b, _ := json.Marshal(p); return string(b) }
	unfixable := func(r RepairResult) int { return r.SkippedUnfixable }
	oldEntry := func(filename string) func(string, string) setupFn {
		return func(root, _ string) setupFn {
			return setPaths(mustJSON([]models.OutputPath{{Outdir: filepath.Join(root, "x", "Old"), Filename: filename}}))
		}
	}
	mkFile := func(path func(root, newDir string) string) func(string, string) setupFn {
		return func(root, newDir string) setupFn {
			return func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, _, _ string) {
				p := path(root, newDir)
				if err := os.RemoveAll(p); err != nil {
					t.Fatalf("remove %s: %v", p, err)
				}
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatalf("make %s a file: %v", p, err)
				}
			}
		}
	}
	cases := []struct {
		name, status string
		setup        func(root, newDir string) setupFn
		count        func(RepairResult) int
	}{
		{name: "done", status: "done"},
		{name: "processing", status: "processing"},
		{name: "multi-entry", status: "failed", count: func(r RepairResult) int { return r.SkippedAmbiguous },
			// [{New,a},{Old,a}] with outdir New: rewriting Old would duplicate New.
			setup: func(root, newDir string) setupFn {
				return setPaths(mustJSON([]models.OutputPath{{Outdir: newDir, Filename: "01.flac"},
					{Outdir: filepath.Join(root, "x", "Old"), Filename: "01.flac"}}))
			}},
		{name: "malformed", status: "failed", count: func(r RepairResult) int { return r.SkippedMalformed },
			setup: func(string, string) setupFn { return setPaths("not json") }},
		{name: "unfixable", status: "failed", count: func(r RepairResult) int { return r.SkippedUnfixable },
			setup: func(_, newDir string) setupFn {
				return func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, _, _ string) {
					if err := os.RemoveAll(newDir); err != nil {
						t.Fatal(err)
					}
				}
			}},
		{name: "other-filename", status: "failed", count: unfixable, setup: oldEntry("02.flac")},
		{name: "relative-entry", status: "failed", count: unfixable,
			setup: func(string, string) setupFn { return setPaths(`[{"outdir":"x/Old","filename":"01.flac"}]`) }},
		// No outdir key decodes to "", not absolute: unfixable, not repaired.
		{name: "no-outdir-key", status: "failed", count: unfixable,
			setup: func(string, string) setupFn { return setPaths(`[{"filename":"01.flac"}]`) }},
		{name: "outdir-not-source-dir", status: "failed", count: unfixable,
			setup: func(_, newDir string) setupFn {
				return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, _, _ string) {
					if err := os.Mkdir(filepath.Join(newDir, "sub"), 0o755); err != nil {
						t.Fatal(err)
					}
					execWQ(t, ctx, sqlDB, `UPDATE work_queue SET outdir = ? WHERE id = ?`, filepath.Join(newDir, "sub"), id)
				}
			}},
		{name: "entry-is-file", status: "failed", setup: mkFile(func(root, _ string) string { return filepath.Join(root, "x", "Old") })},
		{name: "outdir-is-file", status: "failed", count: unfixable, setup: mkFile(func(_, newDir string) string { return newDir })},
		{name: "eacces", status: "failed", count: func(r RepairResult) int { return r.SkippedStatError },
			// The entry's parent is unreadable: EACCES is not proof the dir is gone.
			setup: func(root, _ string) setupFn {
				return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, _, _ string) {
					if os.Geteuid() == 0 {
						t.Skip("root ignores directory permissions")
					}
					locked := filepath.Join(root, "locked")
					if err := os.Mkdir(locked, 0o000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
					setPaths(mustJSON([]models.OutputPath{{Outdir: filepath.Join(locked, "Old"), Filename: "01.flac"}}))(t, ctx, sqlDB, id, "", "")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			id, newDir := seedStale(t, ctx, sqlDB, libID, root, "x", tc.status)
			if tc.setup != nil {
				tc.setup(root, newDir)(t, ctx, sqlDB, id, root, newDir)
			}
			before := rawOutputPaths(t, ctx, sqlDB, id)
			res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
			if err != nil {
				t.Fatalf("RepairOutputPaths: %v", err)
			}
			if len(res.Repaired) != 0 || res.SkippedRaced != 0 || (tc.count != nil && tc.count(res) != 1) {
				t.Errorf("res = %+v, want nothing repaired and this case counted once", res)
			}
			if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
				t.Errorf("output_paths mutated: %s -> %s", before, got)
			}
		})
	}
}

func TestRepairOutputPaths_LibraryIDScopes(t *testing.T) {
	ctx, sqlDB, libA, root := openSeeded(t)
	rootB := filepath.Join(filepath.Dir(root), "musicB")
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	libB, err := library.New(sqlDB).Add(ctx, rootB, "libB", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	idA, _ := seedStale(t, ctx, sqlDB, libA, root, "a", "failed")
	idB, _ := seedStale(t, ctx, sqlDB, libB.ID, rootB, "b", "failed")
	beforeB := rawOutputPaths(t, ctx, sqlDB, idB)
	res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{LibraryID: &libA})
	if err != nil || len(res.Repaired) != 1 || res.Repaired[0].WorkItemID != idA {
		t.Fatalf("res=%+v err=%v, want only library A's row %d", res, err, idA)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, idB); got != beforeB {
		t.Errorf("out-of-scope library B row mutated: %s -> %s", beforeB, got)
	}
}

// TestRepairOutputPaths_RowChangedUnderneathIsNotOverwritten: a row whose
// status or output_paths changes between gather and write is skipped as raced.
func TestRepairOutputPaths_RowChangedUnderneathIsNotOverwritten(t *testing.T) {
	for name, stmt := range map[string]string{
		"status":       `UPDATE work_queue SET status = 'processing' WHERE id = ?`,
		"output_paths": `UPDATE work_queue SET output_paths = '[]' WHERE id = ?`,
		"outdir":       `UPDATE work_queue SET outdir = outdir || '/moved' WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			id, newDir := seedStale(t, ctx, sqlDB, libID, root, "race", "failed")
			res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{beforeWrite: func(int64) { execWQ(t, ctx, sqlDB, stmt, id) }})
			if err != nil || len(res.Repaired) != 0 || res.SkippedRaced != 1 {
				t.Fatalf("res=%+v err=%v, want the row skipped as raced", res, err)
			}
			if got := workQueueOutputPaths(t, ctx, sqlDB, id); len(got) == 1 && got[0].Outdir == newDir {
				t.Errorf("a row changed underneath was overwritten with the repair: %+v", got)
			}
		})
	}
}

// TestRepairOutputPaths_ReportFailureRollsBack pins backup-first: a row is
// never committed without its record.
func TestRepairOutputPaths_ReportFailureRollsBack(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	id, _ := seedStale(t, ctx, sqlDB, libID, root, "rb", "failed")
	before := rawOutputPaths(t, ctx, sqlDB, id)
	boom := errors.New("disk full")
	res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{Report: func(RepairedRow) error { return boom }})
	if !errors.Is(err, boom) || len(res.Repaired) != 0 {
		t.Fatalf("res=%+v err=%v, want the Report error and nothing counted repaired", res, err)
	}
	if got := rawOutputPaths(t, ctx, sqlDB, id); got != before {
		t.Errorf("row committed without its backup record: %s -> %s", before, got)
	}
}

// TestRepairOutputPaths_HealthyRowExcludedBeforeStat pins the SQL pre-filter:
// a row whose single entry equals its own {outdir, filename} is never
// classified, even with its outdir gone (which a stat would count unfixable).
func TestRepairOutputPaths_HealthyRowExcludedBeforeStat(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "healthy", "01.flac")
	seedRowWithOutputPaths(t, ctx, sqlDB, libID, path, "healthy", nil) // queue derives the canonical entry
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'failed'`)
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	res, err := New(sqlDB).RepairOutputPaths(ctx, RepairOptions{})
	if err != nil || len(res.Repaired)+res.SkippedAmbiguous+res.SkippedUnfixable+res.SkippedStatError+res.SkippedMalformed+res.SkippedRaced != 0 {
		t.Errorf("res=%+v err=%v, want the healthy row excluded before any stat", res, err)
	}
}

// Invalid UTF-8 cannot be put on disk under APFS, so the skip is pinned on the
// predicate alone.
func TestRelinkShape_RejectsInvalidUTF8(t *testing.T) {
	ok := models.OutputPath{Outdir: "/m/Old", Filename: "01.flac"}
	if !relinkShape("/m/New", "01.flac", "/m/New/01.flac", ok) {
		t.Fatal("valid relink shape rejected")
	}
	bad := "\xff"
	if relinkShape("/m/New", "01.flac", "/m/New/01.flac", models.OutputPath{Outdir: "/m/Old" + bad, Filename: "01.flac"}) ||
		relinkShape("/m/New", "01.flac"+bad, "/m/New/01.flac"+bad, models.OutputPath{Outdir: "/m/Old", Filename: "01.flac" + bad}) ||
		relinkShape("/m/New"+bad, "01.flac", "/m/New"+bad+"/01.flac", ok) {
		t.Error("invalid UTF-8 accepted")
	}
	// source_path alone: outdir, filename and the entry are all valid, so only
	// the source_path check can reject it. The directory still matches outdir,
	// so the shape test itself would pass.
	if relinkShape("/m/New", "01.flac", "/m/New/01"+bad+".flac", ok) {
		t.Error("invalid UTF-8 in source_path alone accepted")
	}
}
