package prune

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestSweep_RelinkRewritesOutputPaths is the direct regression test for
// issue #921: a clean single-entry relink must rewrite output_paths to the
// new location, not just source_path/outdir/filename. Before the fix,
// output_paths kept the pre-move directory, which internal/worker's
// outputPaths() prefers verbatim over outdir/filename -- so the relinked row
// kept writing to (and re-fetching for) the vanished directory forever.
func TestSweep_RelinkRewritesOutputPaths(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	oldPath := filepath.Join(root, "Artist921", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "Artist921", "AlbumNew", "01. track.flac")
	wqID := seedRowWithOutputPaths(t, ctx, sqlDB, libID, oldPath, "mbid-921-single",
		[]models.OutputPath{{Outdir: filepath.Dir(oldPath), Filename: "01. track.flac"}})
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-921-single", "")

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1", len(res.Relinked))
	}

	got := workQueueOutputPaths(t, ctx, sqlDB, wqID)
	want := []models.OutputPath{{Outdir: filepath.Dir(newPath), Filename: "01. track.flac"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("output_paths after relink = %+v, want %+v (THE #921 BUG: output_paths must follow the relink, not stay on the pre-move directory)", got, want)
	}
}

// TestSweep_RelinkPreservesUnrelatedOutputPathEntries covers the multi-entry
// shape output_paths can legitimately carry: internal/identityrepair's
// mergeQueueRows unions two work_queue rows' output_paths when their
// (artist_key, title_key) collide, so a single row can hold more than one
// real write destination. This test asserts the relink rewrites ONLY the
// entry that matched the row's pre-relink location and leaves every other
// entry untouched -- rewriting or dropping the unrelated entry would silently
// stop that other file from ever being written again.
func TestSweep_RelinkPreservesUnrelatedOutputPathEntries(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	oldPath := filepath.Join(root, "Artist921M", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "Artist921M", "AlbumNew", "01. track.flac")
	// A second, wholly unrelated destination this row is ALSO responsible for
	// writing (e.g. a duplicate catalog entry unioned in by identityrepair).
	otherEntry := models.OutputPath{Outdir: filepath.Join(root, "SomeOtherAlbum"), Filename: "duplicate.lrc"}

	wqID := seedRowWithOutputPaths(t, ctx, sqlDB, libID, oldPath, "mbid-921-multi",
		[]models.OutputPath{
			{Outdir: filepath.Dir(oldPath), Filename: "01. track.flac"},
			otherEntry,
		})
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-921-multi", "")

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1", len(res.Relinked))
	}

	got := workQueueOutputPaths(t, ctx, sqlDB, wqID)
	if len(got) != 2 {
		t.Fatalf("output_paths after relink = %+v, want 2 entries (the unrelated entry must survive)", got)
	}
	wantRelinked := models.OutputPath{Outdir: filepath.Dir(newPath), Filename: "01. track.flac"}
	var sawRelinked, sawOther bool
	for _, p := range got {
		switch p {
		case wantRelinked:
			sawRelinked = true
		case otherEntry:
			sawOther = true
		}
	}
	if !sawRelinked {
		t.Errorf("output_paths = %+v; missing the relinked entry %+v", got, wantRelinked)
	}
	if !sawOther {
		t.Errorf("output_paths = %+v; the UNRELATED entry %+v was dropped or altered, but the relink has no information about it", got, otherEntry)
	}
}

// relinkNoMatch seeds a done row whose outdir/filename is A1 but whose
// output_paths column is set verbatim to raw (so it matches NO entry), moves
// the file A1 -> A2, sweeps, and returns the decoded output_paths plus A2's
// directory. raw is built from the library root so entries can live under it.
func relinkNoMatch(t *testing.T, rawFor func(root string) string) ([]models.OutputPath, string) {
	t.Helper()
	ctx, sqlDB, libID, root := openSeeded(t)
	raw := rawFor(root)
	oldPath := filepath.Join(root, "Artist921N", "A1", "01. track.flac")
	newPath := filepath.Join(root, "Artist921N", "A2", "01. track.flac")
	wqID := seedRowWithOutputPaths(t, ctx, sqlDB, libID, oldPath, "mbid-921-nomatch", nil)
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET output_paths = ? WHERE id = ?`, raw, wqID); err != nil {
		t.Fatalf("set output_paths: %v", err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-921-nomatch", "")
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1", len(res.Relinked))
	}
	return workQueueOutputPaths(t, ctx, sqlDB, wqID), filepath.Dir(newPath)
}

func mustJSON(t *testing.T, paths []models.OutputPath) string {
	t.Helper()
	b, err := json.Marshal(paths)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestSweep_RelinkNoMatchPolicy pins relinkOutputPaths' no-match branch: when
// no output_paths entry equals the pre-relink (outdir, filename), an entry is
// dropped ONLY if its Outdir stats as fs.ErrNotExist; everything else is kept
// in its original order and the new destination is appended last.
func TestSweep_RelinkNoMatchPolicy(t *testing.T) {
	const fn = "01. track.flac"

	// The reproduced merged-row case (identityrepair.mergeQueueRows): A0 is a
	// stale entry whose directory is gone, B is another file's real
	// destination whose directory exists. Replacing the list wholesale dropped
	// B permanently.
	t.Run("merged row keeps existing dir, drops missing dir", func(t *testing.T) {
		var b models.OutputPath
		got, newDir := relinkNoMatch(t, func(root string) string {
			a0 := models.OutputPath{Outdir: filepath.Join(root, "Gone", "A0"), Filename: fn}
			b = models.OutputPath{Outdir: filepath.Join(root, "Other", "B"), Filename: "dup.flac"}
			if err := os.MkdirAll(b.Outdir, 0o755); err != nil {
				t.Fatalf("mkdir B: %v", err)
			}
			return mustJSON(t, []models.OutputPath{a0, b})
		})
		want := []models.OutputPath{b, {Outdir: newDir, Filename: fn}}
		if mustJSON(t, got) != mustJSON(t, want) {
			t.Errorf("output_paths = %+v, want %+v (B must survive, stale A0 must go, new entry appended)", got, want)
		}
	})

	for _, raw := range []string{"", "null", "[]"} {
		t.Run("empty column "+raw, func(t *testing.T) {
			got, newDir := relinkNoMatch(t, func(string) string { return raw })
			want := []models.OutputPath{{Outdir: newDir, Filename: fn}}
			if mustJSON(t, got) != mustJSON(t, want) {
				t.Errorf("output_paths = %+v, want %+v", got, want)
			}
		})
	}

	// A stat error that is NOT ErrNotExist (here EACCES from a 000 parent) is
	// what an unavailable mount looks like; it must never delete a destination.
	t.Run("non-ErrNotExist stat error keeps the entry", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions; EACCES cannot be produced")
		}
		var c models.OutputPath
		got, newDir := relinkNoMatch(t, func(root string) string {
			locked := filepath.Join(root, "Locked")
			c = models.OutputPath{Outdir: filepath.Join(locked, "Inner"), Filename: "c.flac"}
			if err := os.MkdirAll(c.Outdir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			if _, err := os.Stat(c.Outdir); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("precondition: stat(%q) = %v, want a non-ErrNotExist error", c.Outdir, err)
			}
			return mustJSON(t, []models.OutputPath{c})
		})
		want := []models.OutputPath{c, {Outdir: newDir, Filename: fn}}
		if mustJSON(t, got) != mustJSON(t, want) {
			t.Errorf("output_paths = %+v, want %+v (an unstattable-but-not-missing dir must be kept)", got, want)
		}
	})
}

// TestRelinkOne_DeclinesWhenOutputPathsChangedSinceGather pins the
// optimistic-concurrency guard: relinkOne derives the new output_paths from
// the gathered snapshot, so if the column changed in between, the UPDATE must
// match zero rows and the candidate is declined rather than overwriting the
// newer value.
func TestRelinkOne_DeclinesWhenOutputPathsChangedSinceGather(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "Artist921C", "Old", "01. track.flac")
	newPath := filepath.Join(root, "Artist921C", "New", "01. track.flac")
	entry := models.OutputPath{Outdir: filepath.Dir(oldPath), Filename: "01. track.flac"}
	wqID := seedRowWithOutputPaths(t, ctx, sqlDB, libID, oldPath, "mbid-921-cas", []models.OutputPath{entry})
	srID := seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-921-cas", "")
	var current string
	if err := sqlDB.QueryRowContext(ctx, `SELECT output_paths FROM work_queue WHERE id = ?`, wqID).Scan(&current); err != nil {
		t.Fatalf("read output_paths: %v", err)
	}
	target := presentRowDetail{scanResultID: srID, filePath: newPath, outdir: filepath.Dir(newPath), filename: "01. track.flac"}
	run := func(snapshot string) relinkDecision {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		c := &candidate{workItems: []workRow{{id: wqID, rawOutputPaths: snapshot, inputs: models.Inputs{
			Outdir: entry.Outdir, Filename: entry.Filename, SourcePath: oldPath, OutputPaths: []models.OutputPath{entry},
		}}}}
		d, err := relinkOne(ctx, tx, c, target)
		if err != nil {
			t.Fatalf("relinkOne: %v", err)
		}
		return d
	}
	if d := run(`[{"outdir":"/stale","filename":"x"}]`); !strings.Contains(d.reason, "changed since it was read") {
		t.Errorf("stale snapshot: reason = %q, want a decline naming the changed row", d.reason)
	}
	if got := workQueueOutputPaths(t, ctx, sqlDB, wqID); len(got) != 1 || got[0] != entry {
		t.Errorf("output_paths = %+v after a declined relink, want unchanged %+v", got, entry)
	}
	// Positive control: the matching snapshot applies, so the decline above is
	// the guard and not some other precondition.
	if d := run(current); d.reason != "" {
		t.Errorf("current snapshot: reason = %q, want the relink to apply", d.reason)
	}
}
