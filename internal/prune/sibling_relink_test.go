package prune

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedSwap seeds a queue row for <dir>/01. a.mp3, removes the file, and indexes
// a present sibling per extension. It returns the gone path and the row id.
func seedSwap(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, dir, wqStatus string, exts ...string) (string, int64) {
	t.Helper()
	gone := filepath.Join(dir, "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, gone, "done", wqStatus)
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue WHERE source_path = ?`, gone).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	for _, ext := range exts {
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(dir, "01. a"+ext), "", "")
	}
	return gone, id
}

func linkedPaths(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64) string {
	t.Helper()
	var got sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT group_concat(library_id || ':' || file_path, '|') FROM (
           SELECT sr.library_id, sr.file_path FROM work_queue_scan_results j
           JOIN scan_results sr ON sr.id = j.scan_result_id WHERE j.work_queue_id = ? ORDER BY sr.library_id)`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got.String
}

// An in-place format swap: the settled row moves to the lone same-stem file,
// keeps its status and sidecar destination, and a dry run reports the same.
func TestSweep_SiblingSwapRelinksSettledRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "Artist", "Album")
	gone, id := seedSwap(t, ctx, sqlDB, libID, dir, "done", ".flac")
	flac := filepath.Join(dir, "01. a.flac")
	// Shares the name prefix but not the stem: never a sibling.
	seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(dir, "01. a.live.flac"), "", "")
	// Indexed but itself gone: never a target or an ambiguity. Its own stale row
	// has no queue row, so the tier leaves it retained, never called a relink.
	seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(dir, "01. a.ogg"), "", "")
	if err := os.Remove(filepath.Join(dir, "01. a.ogg")); err != nil {
		t.Fatal(err)
	}
	before := workQueueOutputPaths(t, ctx, sqlDB, id)

	dry, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, id); got != gone {
		t.Fatalf("dry run moved the row to %q", got)
	}
	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 1 || len(dry.Relinked) != 1 || len(res.Retained) != 1 || len(dry.Retained) != 1 || len(res.Pruned) != 0 {
		t.Fatalf("relinked dry=%d apply=%d, retained dry=%d apply=%d, pruned=%d; want 1/1, 1/1 (the scan-only row) and none pruned",
			len(dry.Relinked), len(res.Relinked), len(dry.Retained), len(res.Retained), len(res.Pruned))
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, id); got != flac {
		t.Errorf("source_path = %q, want the sibling %q", got, flac)
	}
	if st := workQueueStatus(t, ctx, sqlDB, id); st != "done" {
		t.Errorf("status = %q, want done (a swap never reopens a settled row)", st)
	}
	after := workQueueOutputPaths(t, ctx, sqlDB, id)
	if len(after) != 1 || after[0] != before[0] {
		t.Errorf("output_paths = %+v, want unchanged %+v (the sidecar did not move)", after, before)
	}
	if got, want := linkedPaths(t, ctx, sqlDB, id), "1:"+flac; got != want {
		t.Errorf("linked scan_results = %q, want %q (stale row gone, sibling linked)", got, want)
	}
}

// Two present same-stem siblings are ambiguous, a sibling owned by another row
// is a conflict, and an in-flight row belongs to the worker: none moves.
func TestSweep_SiblingRelinkDeclines(t *testing.T) {
	t.Run("two siblings", func(t *testing.T) {
		ctx, sqlDB, libID, root := openSeeded(t)
		gone, id := seedSwap(t, ctx, sqlDB, libID, filepath.Join(root, "A", "B"), "failed", ".flac", ".m4a")
		res := sweepExact(t, ctx, sqlDB)
		if len(res.Relinked) != 0 || len(res.Retained) != 1 || !strings.Contains(res.Retained[0].Reason, "several present files share this file's name") {
			t.Fatalf("relinked=%d retained=%+v; want 0 and one retain naming the ambiguity", len(res.Relinked), res.Retained)
		}
		if got := workQueueSourcePath(t, ctx, sqlDB, id); got != gone {
			t.Errorf("source_path = %q, want unchanged", got)
		}
	})
	// The owner is junction-linked to the sibling, or (linkless) merely sits at
	// it by source_path: either way no second row is moved onto that path.
	for _, linkless := range []bool{false, true} {
		ctx, sqlDB, libID, root := openSeeded(t)
		dir := filepath.Join(root, "A", "B")
		flac := filepath.Join(dir, "01. a.flac")
		gone, id := seedSwap(t, ctx, sqlDB, libID, dir, "failed")
		if linkless {
			seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
			seedWorkQueueOnlyGoneRow(t, ctx, sqlDB, flac, "Someone Else", "Another Title")
		} else {
			seedPresentScanResultOwnedByOtherRow(t, ctx, sqlDB, libID, flac, "", "")
		}
		dry, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		res := sweepExact(t, ctx, sqlDB)
		for name, r := range map[string]Result{"dry run": dry, "apply": res} {
			if len(r.Relinked) != 0 || len(r.Retained) != 1 || !strings.Contains(r.Retained[0].Reason, "already belongs to a different queue row") {
				t.Errorf("linkless=%v %s: relinked=%d retained=%+v; want 0 and one retain naming the owner conflict", linkless, name, len(r.Relinked), r.Retained)
			}
		}
		if got := workQueueSourcePath(t, ctx, sqlDB, id); got != gone {
			t.Errorf("linkless=%v: source_path = %q, want unchanged", linkless, got)
		}
	}
	t.Run("processing row", func(t *testing.T) {
		ctx, sqlDB, libID, root := openSeeded(t)
		gone, id := seedSwap(t, ctx, sqlDB, libID, filepath.Join(root, "A", "B"), "processing", ".flac")
		res := sweepExact(t, ctx, sqlDB)
		if sr, _, j := rowCounts(t, ctx, sqlDB); len(res.Relinked) != 0 || sr != 2 || j != 1 || workQueueSourcePath(t, ctx, sqlDB, id) != gone {
			t.Errorf("in-flight row changed: relinked=%d scan_results=%d junction=%d", len(res.Relinked), sr, j)
		}
	})
	t.Run("unavailable root", func(t *testing.T) {
		ctx, sqlDB, libID, root := openSeeded(t)
		gone, id := seedSwap(t, ctx, sqlDB, libID, filepath.Join(root, "A", "B"), "failed", ".flac")
		if err := os.RemoveAll(filepath.Join(root, "A")); err != nil {
			t.Fatal(err)
		}
		res := sweepExact(t, ctx, sqlDB)
		if sr, _, _ := rowCounts(t, ctx, sqlDB); len(res.Relinked)+len(res.Retained) != 0 || sr != 2 ||
			workQueueSourcePath(t, ctx, sqlDB, id) != gone || workQueueStatus(t, ctx, sqlDB, id) != "failed" {
			t.Errorf("an empty root changed rows: relinked=%d retained=%d scan_results=%d", len(res.Relinked), len(res.Retained), sr)
		}
	})
}

// A row prune retired before the replacement was indexed is reopened by the
// sibling relink exactly as by any other relink; a hand-edited one moves but
// stays settled (#1228).
func TestSweep_SiblingRelinkReopensRetiredRowUnlessEdited(t *testing.T) {
	for _, edited := range []bool{false, true} {
		ctx, sqlDB, libID, root := openSeeded(t)
		dir := filepath.Join(root, "A", "B")
		keep := filepath.Join(root, "keep.mp3") // keeps the root populated
		seedPresentScanResult(t, ctx, sqlDB, libID, keep, "", "")
		_, id := seedSwap(t, ctx, sqlDB, libID, dir, "failed")
		if res := sweepExact(t, ctx, sqlDB); len(res.Retained) != 1 || !res.Retained[0].Retired {
			t.Fatalf("first sweep did not retire the row: %+v", res.Retained)
		}
		if edited {
			if err := queue.NewDBQueue(sqlDB).SetLyricEdit(ctx, id, 250); err != nil {
				t.Fatal(err)
			}
		}
		flac := filepath.Join(dir, "01. a.flac")
		seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
		res := sweepExact(t, ctx, sqlDB)
		want, held := "pending", 0
		if edited {
			want, held = "done", 1
		}
		if got := workQueueSourcePath(t, ctx, sqlDB, id); len(res.Relinked) != 1 || got != flac {
			t.Fatalf("edited=%v: relinked=%d source_path=%q, want 1 and the sibling", edited, len(res.Relinked), got)
		}
		if st := workQueueStatus(t, ctx, sqlDB, id); st != want || res.EditHeld != held {
			t.Errorf("edited=%v: status=%q EditHeld=%d, want %q and %d", edited, st, res.EditHeld, want, held)
		}
	}
}

// Overlapping libraries each hold a row for the vanished file and one for the
// replacement: one relink links the queue row to BOTH replacements and removes
// both stale rows.
func TestSweep_SiblingRelinkCoversOverlappingLibraries(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A", "B")
	lib2, err := library.New(sqlDB).Add(ctx, filepath.Join(root, "A"), "inner", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	mp3 := filepath.Join(dir, "01. a.mp3")
	// Identity that matches nothing: before #1262 this row was deleted outright.
	seedRowWithIdentity(t, ctx, sqlDB, libID, mp3, "done", "done", "mbid-old-rip", "")
	id := mustWorkQueueID(t, ctx, sqlDB)
	stale2 := seedPresentScanResult(t, ctx, sqlDB, lib2.ID, mp3, "", "")
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, stale2); err != nil {
		t.Fatal(err)
	}
	flac := filepath.Join(dir, "01. a.flac")
	seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
	seedPresentScanResult(t, ctx, sqlDB, lib2.ID, flac, "", "")
	if err := os.Remove(mp3); err != nil {
		t.Fatal(err)
	}
	if res := sweepExact(t, ctx, sqlDB); len(res.Relinked) != 1 {
		t.Fatalf("relinked=%d retained=%+v, want 1", len(res.Relinked), res.Retained)
	}
	want := "1:" + flac + "|2:" + flac
	if got := linkedPaths(t, ctx, sqlDB, id); got != want || workQueueSourcePath(t, ctx, sqlDB, id) != flac {
		t.Errorf("linked scan_results = %q, want %q (both libraries' replacement rows, no stale ones)", got, want)
	}
	if sr, _, _ := rowCounts(t, ctx, sqlDB); sr != 2 {
		t.Errorf("scan_results = %d, want 2 (both stale rows removed)", sr)
	}
	// A stale scan_results row with NO queue row is not the tier's: nothing would
	// move, so it is retained as before, never reported as a relink or deleted.
	seedPresentScanResult(t, ctx, sqlDB, lib2.ID, mp3, "", "")
	if err := os.Remove(mp3); err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{true, false} {
		res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dryRun})
		if err != nil {
			t.Fatal(err)
		}
		if sr, _, _ := rowCounts(t, ctx, sqlDB); len(res.Relinked) != 0 || len(res.Retained) != 1 || sr != 3 {
			t.Errorf("scan-only stale row, dry=%v: relinked=%d retained=%d scan_results=%d, want 0, 1 and 3", dryRun, len(res.Relinked), len(res.Retained), sr)
		}
	}
}

// A sibling carrying a DIFFERENT MBID or ISRC is another recording: retained,
// never relinked or deleted. A key on one side only is no contradiction, and a
// sibling relink's record carries no identity (none drove it).
func TestSweep_SiblingRelinkDeclinesContradictingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, mbid, isrc, sibMBID, sibISRC string
		relink                             bool
	}{
		{"mbid differs", "mbid-one", "", "mbid-two", "", false},
		{"isrc differs", "", "ISRC-ONE", "", "ISRC-TWO", false},
		{"sibling untagged", "mbid-one", "ISRC-ONE", "", "", true},
		{"other key type only", "mbid-one", "", "", "ISRC-TWO", true},
	} {
		ctx, sqlDB, libID, root := openSeeded(t)
		gone := filepath.Join(root, "A", "B", "01. a.mp3")
		seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", tc.mbid, tc.isrc)
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "A", "B", "01. a.flac"), tc.sibMBID, tc.sibISRC)
		res := sweepExact(t, ctx, sqlDB)
		if tc.relink {
			if len(res.Relinked) != 1 || res.Relinked[0].MBID+res.Relinked[0].ISRC != "" {
				t.Errorf("%s: relinked=%+v retained=%d pruned=%d; want one relink carrying no identity", tc.name, res.Relinked, len(res.Retained), len(res.Pruned))
			}
		} else if len(res.Relinked)+len(res.Pruned) != 0 || len(res.Retained) != 1 || !strings.Contains(res.Retained[0].Reason, "different MBID or ISRC") {
			t.Errorf("%s: relinked=%d pruned=%d retained=%+v; want only a retain naming the contradiction", tc.name, len(res.Relinked), len(res.Pruned), res.Retained)
		}
	}
}

// A sibling that cannot be statted (a symlink loop: ELOOP, not ENOENT) is
// neither absent nor present: the row is retained, never relinked or deleted.
func TestSweep_SiblingStatErrorRetains(t *testing.T) {
	for _, mbid := range []string{"", "mbid-x"} {
		ctx, sqlDB, libID, root := openSeeded(t)
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "keep.mp3"), "", "")
		gone := filepath.Join(root, "A", "B", "01. a.mp3")
		seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", mbid, "")
		flac := filepath.Join(root, "A", "B", "01. a.flac")
		seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
		if err := errors.Join(os.Remove(gone), os.Remove(flac), os.Symlink("01. a.flac", flac)); err != nil {
			t.Fatal(err)
		}
		for _, dryRun := range []bool{true, false} {
			res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dryRun})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Relinked)+len(res.Pruned) != 0 || len(res.Retained) != 1 || !strings.Contains(res.Retained[0].Reason, "could not be read") {
				t.Errorf("mbid=%q dry=%v: relinked=%d pruned=%d retained=%+v; want only a retain naming the unreadable file", mbid, dryRun, len(res.Relinked), len(res.Pruned), res.Retained)
			}
		}
	}
}

// Two gone rows, one sibling: the first in path order takes it. The loser is
// retained and retired while it is still work, and dropped silently when it is
// already settled; the dry run reports exactly what the apply does.
func TestSweep_SiblingRelinkLoserRetiresOrDrops(t *testing.T) {
	for status, retained := range map[string]int{"failed": 1, "done": 0} {
		ctx, sqlDB, libID, root := openSeeded(t)
		dir := filepath.Join(root, "A", "B")
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "keep.mp3"), "", "")
		for _, ext := range []string{".aac", ".mp3"} {
			gone := filepath.Join(dir, "01"+ext)
			seedRow(t, ctx, sqlDB, libID, gone, "done", status)
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET artist_key = artist_key || ?, title_key = title_key || ? WHERE source_path = ?`, ext, ext, gone)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
		}
		seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(dir, "01.flac"), "", "")
		for _, dryRun := range []bool{true, false} {
			res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: dryRun})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Relinked) != 1 || len(res.Retained) != retained || res.RelinkOwned != 1 {
				t.Fatalf("%s dry=%v: relinked=%d retained=%d owned=%d; want 1, %d, 1", status, dryRun, len(res.Relinked), len(res.Retained), res.RelinkOwned, retained)
			}
			if retained == 1 && (!res.Retained[0].WouldRetire || res.Retained[0].Retired == dryRun) {
				t.Errorf("%s dry=%v: loser = %+v; want it planned for retirement, retired only by the apply", status, dryRun, res.Retained[0])
			}
		}
	}
}

// A word-recheck-queued or upgrade-armed row moves to the sibling with its
// status and arming intact.
func TestSweep_SiblingRelinkKeepsArmedRows(t *testing.T) {
	for _, arm := range []string{
		`status='deferred', word_timing_state='queued'`,
		`status='pending', upgrade_queued=1, completed_at='2026-01-01T00:00:00Z'`,
	} {
		ctx, sqlDB, libID, root := openSeeded(t)
		dir := filepath.Join(root, "A", "B")
		_, id := seedSwap(t, ctx, sqlDB, libID, dir, "done", ".flac")
		execWQ(t, ctx, sqlDB, `UPDATE work_queue SET `+arm+` WHERE id = ?`, id)
		sweepExact(t, ctx, sqlDB)
		var kept int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM work_queue WHERE id = ? AND source_path = ? AND `+strings.ReplaceAll(arm, ",", " AND"), //nolint:gosec // reason: arm is a fixed test literal
			id, filepath.Join(dir, "01. a.flac")).Scan(&kept); err != nil || kept != 1 {
			t.Errorf("%s: row at the sibling with its arming = %d (err %v), want 1", arm, kept, err)
		}
	}
}

// The dry run makes the apply's ownership check (#1262): a target another queue
// row owns, and a target two gone rows both resolve to, are reported as declined
// by both, with identical counts, on an unchanged library.
func TestSweep_DryRunAndApplyAgreeOnDeclinedRelinks(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	for _, name := range []string{"one", "two"} { // both resolve to the same file
		gone := filepath.Join(root, "Old", name, "01. t.flac")
		seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "mbid-shared", "")
		if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET artist_key = artist_key || ?, title_key = title_key || ? WHERE source_path = ?`, name, name, gone); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "New", "01. t.flac"), "mbid-shared", "")
	owned := filepath.Join(root, "Old", "three", "02. u.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, owned, "done", "failed", "", "isrc-owned")
	if err := os.Remove(owned); err != nil {
		t.Fatal(err)
	}
	seedPresentScanResultOwnedByOtherRow(t, ctx, sqlDB, libID, filepath.Join(root, "New", "02. u.flac"), "", "isrc-owned")

	dry, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	res := sweepExact(t, ctx, sqlDB)
	for name, r := range map[string]Result{"dry run": dry, "apply": res} {
		if len(r.Relinked) != 1 || len(r.Retained) != 2 || r.RelinkOwned != 2 || r.RelinkChanged != 0 {
			t.Errorf("%s: relinked=%d retained=%d owned=%d changed=%d; want 1, 2, 2, 0",
				name, len(r.Relinked), len(r.Retained), r.RelinkOwned, r.RelinkChanged)
		}
	}
}
