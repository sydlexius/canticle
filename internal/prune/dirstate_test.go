package prune

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// countingPruner returns a Pruner whose Directory-sweep stats are recorded.
func countingPruner(sqlDB *sql.DB) (*Pruner, *[]string) {
	p := New(sqlDB)
	var calls []string
	p.stat = func(path string) (fs.FileInfo, error) {
		calls = append(calls, path)
		return os.Stat(path)
	}
	return p, &calls
}

// age sets dir's mtime well past the settle margin, as a directory nobody has
// touched lately has.
func age(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func sweepDir(t *testing.T, ctx context.Context, p *Pruner) Result {
	t.Helper()
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Directory})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

func storedDirs(t *testing.T, ctx context.Context, sqlDB *sql.DB) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM prune_dir_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// THE #1262 ACCEPTANCE TEST: an in-folder .mp3 -> .flac swap in a directory
// the sweep had already recorded is relinked by the periodic sweep.
func TestSweepDirectory_RelinksInFolderSwap(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "Artist", "Album")
	mp3 := filepath.Join(dir, "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, mp3, "done", "done")
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue WHERE source_path = ?`, mp3).Scan(&id); err != nil {
		t.Fatal(err)
	}
	age(t, dir)
	p := New(sqlDB)
	if res := sweepDir(t, ctx, p); len(res.Relinked)+len(res.Retained)+len(res.Pruned) != 0 {
		t.Fatalf("clean sweep acted: %+v", res)
	}

	if err := os.Remove(mp3); err != nil {
		t.Fatal(err)
	}
	flac := filepath.Join(dir, "01. a.flac")
	seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
	old := time.Now().Add(-2 * time.Hour) // settled, and not the mtime already stored
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	res := sweepDir(t, ctx, p)
	if len(res.Relinked) != 1 || len(res.Pruned) != 0 {
		t.Fatalf("relinked=%d pruned=%d, want 1/0", len(res.Relinked), len(res.Pruned))
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, id); got != flac {
		t.Errorf("source_path = %q, want %q", got, flac)
	}
	// The relinked row is no longer gone, so the directory is stored with no
	// scan mark: a later scan insert must not re-examine it.
	var mark sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT gone_scan_id FROM prune_dir_state WHERE dir = ?`, dir).Scan(&mark); err != nil || mark.Valid {
		t.Fatalf("gone_scan_id = %v (err %v), want the directory stored with NULL", mark, err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(root, "Elsewhere", "new.flac"), "", "")
	p2, calls := countingPruner(sqlDB)
	sweepDir(t, ctx, p2)
	for _, c := range *calls {
		if filepath.Dir(c) == dir {
			t.Errorf("an unrelated scan insert re-examined the relinked directory: statted %q", c)
		}
	}
}

// A directory whose mtime is inside the settle margin (or in the future) is
// examined but not recorded, so a change landing in the same timestamp granule
// as the examination is still seen; it is recorded once the mtime has aged.
func TestSweepDirectory_YoungMtimeIsNotRecorded(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(album, "01. a.mp3"), "done", "done")
	for _, mtime := range []time.Time{time.Now(), time.Now().Add(24 * time.Hour)} {
		if err := os.Chtimes(album, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		p, calls := countingPruner(sqlDB)
		sweepDir(t, ctx, p)
		if n := storedDirs(t, ctx, sqlDB); n != 0 || len(*calls) != 2 {
			t.Fatalf("mtime %v: stored=%d stats=%d, want 0 stored and 2 stats", mtime, n, len(*calls))
		}
	}
	age(t, album)
	sweepDir(t, ctx, New(sqlDB))
	if n := storedDirs(t, ctx, sqlDB); n != 1 {
		t.Fatalf("stored directories = %d, want 1 once the mtime has aged", n)
	}
}

// seedAlbums seeds n aged albums of three identity-bearing done tracks each.
func seedAlbums(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, root string, n int) []string {
	t.Helper()
	var dirs []string
	for a := 0; a < n; a++ {
		dir := filepath.Join(root, fmt.Sprintf("Artist%d", a), "Album")
		for i := 0; i < 3; i++ {
			seedRowWithIdentity(t, ctx, sqlDB, libID, filepath.Join(dir, fmt.Sprintf("%d-%02d.mp3", a, i)), "done", "done", fmt.Sprintf("mbid-%d-%d", a, i), "")
		}
		age(t, dir)
		dirs = append(dirs, dir)
	}
	return dirs
}

// outside lists the statted paths that are neither a directory in dirs nor
// under dirs[0], the one album the test changes.
func outside(calls, dirs []string) (out []string) {
	for _, c := range calls {
		if !slices.Contains(dirs, c) && filepath.Dir(c) != dirs[0] {
			out = append(out, c)
		}
	}
	return out
}

// THE COST TEST: the first sweep stats every row file once; after that, and
// across a restart (a fresh Pruner), an unchanged directory costs one stat.
// One identity-bearing row gone inside a surviving directory is retained
// without statting anything outside that directory (no identity pool), the
// directory is recorded anyway, and every later sweep is back to one stat per
// directory. A replacement indexed later WITHOUT the directory mtime moving is
// still found, because a scan advanced the scan_results high-water mark.
func TestSweepDirectory_RetainedRowConvergesToOneStatPerDirectory(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirs := seedAlbums(t, ctx, sqlDB, libID, root, 4)
	for _, want := range []int{16, 4} {
		p, calls := countingPruner(sqlDB)
		if sweepDir(t, ctx, p); len(*calls) != want {
			t.Fatalf("clean sweep made %d stats, want %d: %v", len(*calls), want, *calls)
		}
	}
	victim := filepath.Join(dirs[0], "0-00.mp3")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	age(t, dirs[0])
	fi, _ := os.Stat(dirs[0])

	for pass, want := range []int{7, 4, 4} { // 4 directories, plus the album's 3 rows once
		p, calls := countingPruner(sqlDB)
		res := sweepDir(t, ctx, p)
		if extra := outside(*calls, dirs); len(extra) != 0 {
			t.Fatalf("pass %d statted outside the changed directory: %v", pass, extra)
		}
		if len(*calls) != want {
			t.Fatalf("pass %d: %d stats, want %d: %v", pass, len(*calls), want, *calls)
		}
		if retained := len(res.Retained); len(res.Pruned) != 0 || (pass == 0) != (retained == 1) {
			t.Fatalf("pass %d: retained=%d pruned=%d, want a retain on the first pass only", pass, retained, len(res.Pruned))
		}
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 12 || wq != 12 || storedDirs(t, ctx, sqlDB) != 4 {
		t.Fatalf("scan=%d wq=%d stored=%d, want 12/12/4", sr, wq, storedDirs(t, ctx, sqlDB))
	}

	// A scan indexes the replacement; the directory's mtime reads as before.
	flac := filepath.Join(dirs[0], "0-00.flac")
	seedPresentScanResult(t, ctx, sqlDB, libID, flac, "", "")
	if err := os.Chtimes(dirs[0], fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	p, calls := countingPruner(sqlDB)
	res := sweepDir(t, ctx, p)
	if len(res.Relinked) != 1 || res.Relinked[0].NewPath != flac {
		t.Fatalf("relinked = %+v, want the row moved to %q", res.Relinked, flac)
	}
	if extra := outside(*calls, dirs); len(extra) != 0 {
		t.Fatalf("relink statted outside the changed directory: %v", extra)
	}
}

// A settled identity-less gone row is silent, and its directory still
// converges (hostile review 2). An eligible one is retained with a reason that
// does not defer to this very sweep, and is not retired here (hostile review
// 3). A directory whose gone row is in flight is NOT recorded: nothing but the
// next sweep would look at it again.
func TestSweepDirectory_IdentitylessGoneRowsConverge(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	paths := map[string]string{"done": "", "pending": "", "processing": ""}
	for status := range paths {
		dir := filepath.Join(root, status, "Album")
		paths[status] = filepath.Join(dir, status+" 01.mp3")
		seedRow(t, ctx, sqlDB, libID, paths[status], status, status)
		seedRow(t, ctx, sqlDB, libID, filepath.Join(dir, status+" 02.mp3"), "done", "done")
		if err := os.Remove(paths[status]); err != nil {
			t.Fatal(err)
		}
		age(t, dir)
	}
	for pass, want := range []int{9, 5, 5} { // the in-flight directory is examined every time
		p, calls := countingPruner(sqlDB)
		res := sweepDir(t, ctx, p)
		if len(*calls) != want {
			t.Fatalf("pass %d: %d stats, want %d: %v", pass, len(*calls), want, *calls)
		}
		if pass > 0 {
			if len(res.Retained) != 0 {
				t.Fatalf("pass %d re-reported %+v", pass, res.Retained)
			}
			continue
		}
		if len(res.Retained) != 1 || res.Retained[0].SourcePath != paths["pending"] || res.Retained[0].Retired ||
			strings.Contains(res.Retained[0].Reason, "deferred") {
			t.Fatalf("retained = %+v, want only the pending row, unretired, with no deferral", res.Retained)
		}
	}
	if n := storedDirs(t, ctx, sqlDB); n != 2 {
		t.Errorf("stored directories = %d, want 2 (the in-flight one left out)", n)
	}
}

// A relink a worker raced (the row went in flight between the read and the
// apply) leaves its directory unrecorded, so the next sweep looks again.
func TestSweepDirectory_RacedRelinkIsNotRecorded(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "Artist", "Album")
	_, id := seedSwap(t, ctx, sqlDB, libID, dir, "done", ".flac")
	age(t, dir)
	// A second swap that sorts FIRST and relinks cleanly: only the raced
	// directory may be left unrecorded, not whichever relink came first.
	clean := filepath.Join(root, "AAA", "Album")
	cleanMP3 := filepath.Join(clean, "02. b.mp3")
	seedRow(t, ctx, sqlDB, libID, cleanMP3, "done", "done")
	if err := os.Remove(cleanMP3); err != nil {
		t.Fatal(err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, filepath.Join(clean, "02. b.ogg"), "", "")
	age(t, clean)
	p := New(sqlDB)
	p.stat = func(path string) (fs.FileInfo, error) {
		if filepath.Ext(path) == ".flac" { // the sibling's stat: after the read, before the apply
			if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
		return os.Stat(path)
	}
	if res := sweepDir(t, ctx, p); len(res.Relinked) != 1 || res.RelinkChanged != 1 {
		t.Fatalf("relinked=%d changed=%d, want 1/1", len(res.Relinked), res.RelinkChanged)
	}
	var stored string
	if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(group_concat(dir, '|'), '') FROM prune_dir_state`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != clean {
		t.Fatalf("stored directories = %q, want only the cleanly relinked %q", stored, clean)
	}
}

// A failure to store the directory state must not hide a reconciliation that
// already committed (hostile review 5).
func TestSweepDirectory_SaveFailureKeepsCommittedResult(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	seedRowWithIdentity(t, ctx, sqlDB, libID, filepath.Join(album, "01.mp3"), "done", "done", "mbid-nowhere", "")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(root, "Other", "01.mp3"), "done", "done")
	if err := os.RemoveAll(album); err != nil {
		t.Fatal(err)
	}
	p := New(sqlDB)
	p.stat = func(path string) (fs.FileInfo, error) {
		if path == album { // a directory stat: no query is open around it
			if _, err := sqlDB.ExecContext(ctx, `DROP TABLE prune_dir_state`); err != nil {
				t.Fatal(err)
			}
		}
		return os.Stat(path)
	}
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Directory})
	if err != nil || len(res.Pruned) != 1 || res.WorkItems != 1 {
		t.Fatalf("err=%v pruned=%d work=%d, want the committed delete reported with a nil error", err, len(res.Pruned), res.WorkItems)
	}
}

// A removed directory keeps its pre-#1262 outcome (a genuine delete for an
// identity that matches nothing), costs one stat, and loses its stored state.
func TestSweepDirectory_RemovedDirectoryStillPrunes(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	seedRowWithIdentity(t, ctx, sqlDB, libID, filepath.Join(album, "01. a.mp3"), "done", "done", "mbid-nowhere", "")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(root, "Other", "01. kept.mp3"), "done", "done")
	age(t, album)
	age(t, filepath.Join(root, "Other"))
	sweepDir(t, ctx, New(sqlDB))
	if n := storedDirs(t, ctx, sqlDB); n != 2 {
		t.Fatalf("stored directories = %d, want 2", n)
	}
	if err := os.RemoveAll(album); err != nil {
		t.Fatal(err)
	}

	p, calls := countingPruner(sqlDB)
	res := sweepDir(t, ctx, p)
	if len(res.Pruned) != 1 || res.WorkItems != 1 {
		t.Fatalf("pruned=%d work=%d, want 1/1", len(res.Pruned), res.WorkItems)
	}
	// One per directory, plus the identity pool's stat of the one row carrying
	// an identity: a removed directory still builds the pool, as before #1262.
	if len(*calls) != 3 {
		t.Errorf("stats = %v, want one per directory and one for the pool", *calls)
	}
	if n := storedDirs(t, ctx, sqlDB); n != 1 {
		t.Errorf("stored directories = %d, want 1 (the removed one dropped)", n)
	}

	// State for a directory with no rows left is dropped, and so is state under
	// no configured root (a removed library, hostile review 6); state under a
	// configured root that is merely unavailable (here: empty) is kept.
	offline := filepath.Join(filepath.Dir(root), "offline")
	if err := os.MkdirAll(offline, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := library.New(sqlDB).Add(ctx, offline, "offline", models.LibrarySettings{}); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(offline, "Album")
	for _, dir := range []string{filepath.Join(root, "Rowless"), kept, filepath.Join(filepath.Dir(root), "removed", "Album")} {
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO prune_dir_state (dir, mtime_ns) VALUES (?, 1)`, dir); err != nil {
			t.Fatal(err)
		}
	}
	sweepDir(t, ctx, New(sqlDB))
	var left string
	if err := sqlDB.QueryRowContext(ctx, `SELECT group_concat(dir, '|') FROM (SELECT dir FROM prune_dir_state ORDER BY dir)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Other") + "|" + kept; left != want {
		t.Errorf("stored directories = %q, want %q", left, want)
	}
}

// A stat failure that is not not-exist reads as neither gone nor changed: on
// the directory nothing under it is stat'ed; on a row file the row is kept and
// the directory is not recorded. A dry run records nothing either.
func TestSweepDirectory_StatErrorsNeverReadAsGone(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	track := filepath.Join(album, "01. a.mp3")
	seedRowWithIdentity(t, ctx, sqlDB, libID, track, "done", "done", "mbid-nowhere", "")
	if err := os.Remove(track); err != nil { // gone for real: only the injected error protects it
		t.Fatal(err)
	}
	age(t, album)

	for _, failing := range []string{album, track} {
		p := New(sqlDB)
		var calls []string
		p.stat = func(path string) (fs.FileInfo, error) {
			calls = append(calls, path)
			if path == failing {
				return nil, &fs.PathError{Op: "stat", Path: path, Err: syscall.EIO}
			}
			return os.Stat(path)
		}
		res := sweepDir(t, ctx, p)
		if len(res.Pruned)+len(res.Retained)+len(res.Relinked) != 0 {
			t.Fatalf("stat error on %q acted: %+v", failing, res)
		}
		if failing == album && len(calls) != 1 {
			t.Errorf("unreadable directory: stats = %v, want the directory only", calls)
		}
		if n := storedDirs(t, ctx, sqlDB); n != 0 {
			t.Errorf("stat error on %q stored %d directories, want 0", failing, n)
		}
	}

	if err := os.WriteFile(track, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, album)
	// Neither a dry run nor a library-scoped sweep (it saw one library's rows
	// only) records a directory as examined.
	for name, opts := range map[string]SweepOptions{
		"dry run":        {Granularity: Directory, DryRun: true},
		"library-scoped": {Granularity: Directory, LibraryID: &libID},
	} {
		if _, err := New(sqlDB).Sweep(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if n := storedDirs(t, ctx, sqlDB); n != 0 {
			t.Errorf("%s stored %d directories, want 0", name, n)
		}
	}
}
