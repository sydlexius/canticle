package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// writeSettledMovedFile creates the shape a RELOCATED track actually has on
// disk: a tagged audio file that arrived at its new path WITH its sidecar,
// because a directory move carries both. That is what makes the scan skip it.
func writeSettledMovedFile(t *testing.T, path, artist, title string) {
	t.Helper()
	writeSettledMovedFileExt(t, path, artist, title, ".lrc")
}

func writeSettledMovedFileExt(t *testing.T, path, artist, title, ext string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir moved dir: %v", err)
	}
	base := filepath.Base(path)
	if err := testutil.WriteFLACFileWithComments(dir, base, 44100, 44100*30,
		map[string]string{"ARTIST": artist, "TITLE": title}); err != nil {
		t.Fatalf("write moved fixture: %v", err)
	}
	stem := base[:len(base)-len(filepath.Ext(base))]
	if err := os.WriteFile(filepath.Join(dir, stem+ext), []byte("[00:01.00]x"), 0o600); err != nil {
		t.Fatalf("write moved sidecar: %v", err)
	}
}

// scanAndPersist runs a real serve-mode scan over root and upserts what it
// found, mirroring scan.Scheduler.scanAndPersist. withIndex selects whether the
// #786 seam is wired, which is the single variable under test.
func scanAndPersist(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, root string, withIndex bool) int {
	t.Helper()
	repo := scan.New(sqlDB)
	var opts []scanner.Option
	if withIndex {
		opts = append(opts, scanner.WithIndexStore(repo))
	}
	sc := scanner.NewScanner(opts...)

	results, err := sc.ScanLibrary(ctx, root, scanner.ScanOptions{MaxDepth: 100})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	for i := range results {
		results[i].LibraryID = libID
		if results[i].Status == "" {
			results[i].Status = scan.StatusPending
		}
	}
	if err := repo.Upsert(ctx, libID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return len(results)
}

func indexedAt(t *testing.T, ctx context.Context, sqlDB *sql.DB, path string) bool {
	t.Helper()
	ok, err := scan.New(sqlDB).Indexed(ctx, path)
	if err != nil {
		t.Fatalf("Indexed: %v", err)
	}
	return ok
}

// THE #786 END-TO-END TEST, and the reason it exists as its own case: every
// other test in this change verifies ONE half in isolation. The scanner tests
// prove a settled-but-unindexed file is emitted; the repo tests prove Indexed
// answers correctly; the #740 tests prove prune relinks when the pool contains
// the target. NONE of them prove the halves COMPOSE -- and composition is
// precisely where this bug lived: skipping settled work is right, and building
// the pool from scan_results is right, but together they guaranteed a moved
// file could never be relinked.
//
// Runs the REAL scan and the REAL sweep against real SQLite and real files.
func TestRelocatedFileIsIndexedThenRelinked(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	gone := filepath.Join(root, "Old Artist Name", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist Name", "Album", "01. Winterlight.flac")

	// The row canticle already has, pointing at the pre-move path.
	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	// The move: the old path disappears, the new one appears WITH its sidecar.
	if err := os.RemoveAll(filepath.Join(root, "Old Artist Name")); err != nil {
		t.Fatalf("remove old tree: %v", err)
	}
	writeSettledMovedFile(t, moved, "New Artist Name", goneTitle)

	// PRECONDITION: the moved file is unknown to the index. If this ever reads
	// true the test is not exercising the relocated-file case at all.
	if indexedAt(t, ctx, sqlDB, moved) {
		t.Fatal("precondition failed: the moved path is already indexed")
	}

	scanAndPersist(t, ctx, sqlDB, libID, root, true)

	// HALF ONE: the scan indexed a file it also (correctly) skipped fetching.
	if !indexedAt(t, ctx, sqlDB, moved) {
		t.Fatal("the relocated file was not indexed; prune's pool cannot contain it")
	}

	// HALF TWO: with the pool now populated, the sweep relinks instead of retiring.
	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1 (the moved file is indexed, so the tier should re-attach it)", len(res.Relinked))
	}
	if res.Relinked[0].NewPath != moved {
		t.Errorf("relinked onto %q; want %q", res.Relinked[0].NewPath, moved)
	}

	// The row must be back in the eligible set, not left terminal.
	var status, lastErr string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, last_error FROM work_queue WHERE source_path = ?`, moved,
	).Scan(&status, &lastErr); err != nil {
		t.Fatalf("read relinked row: %v", err)
	}
	if status == "done" && lastErr == unresolvableGoneError {
		t.Error("row is still retired as unactionable after a successful relink")
	}
}

// THE NEGATIVE CONTROL, and the load-bearing half of the pair. Without the
// index seam the identical scenario must FAIL to relink -- proving the test
// above passes because of the fix rather than because the sweep would have
// relinked anyway. A positive assertion alone cannot distinguish those.
func TestRelocatedFileWithoutIndexStoreIsNeverRelinked(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	gone := filepath.Join(root, "Old Artist Name", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist Name", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.RemoveAll(filepath.Join(root, "Old Artist Name")); err != nil {
		t.Fatalf("remove old tree: %v", err)
	}
	writeSettledMovedFile(t, moved, "New Artist Name", goneTitle)

	scanAndPersist(t, ctx, sqlDB, libID, root, false)

	if indexedAt(t, ctx, sqlDB, moved) {
		t.Fatal("CONTROL FAILED: the moved file was indexed with no index store wired")
	}
	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 0 {
		t.Fatalf("CONTROL FAILED: Relinked = %d with no index store; the pool should be empty of the target", len(res.Relinked))
	}
}

// A settled file the index ALREADY knows must not be re-emitted by the scan.
// This is the steady-state property that keeps the repair affordable: without
// it every scan would re-tag the whole library and keep the array awake, the
// exact symptom the idle-disk work exists to remove.
func TestAlreadyIndexedSettledFileIsNotReEmitted(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	present := filepath.Join(root, "Artist", "Album", "01. Harbor Bells.flac")
	writeSettledMovedFile(t, present, "Artist", "Harbor Bells")

	// First scan indexes it.
	if n := scanAndPersist(t, ctx, sqlDB, libID, root, true); n != 1 {
		t.Fatalf("first scan emitted %d results; want 1", n)
	}
	// Second scan must emit nothing: the row now exists.
	if n := scanAndPersist(t, ctx, sqlDB, libID, root, true); n != 0 {
		t.Fatalf("second scan emitted %d results; want 0 (already indexed)", n)
	}
}

// Indexing a settled file must NOT queue a fetch for it. The row is written
// 'done' precisely because EnqueuePending drains the pending rows, so a
// 'pending' stamp here would send the worker after lyrics already on disk --
// changing what the scan DOES, which this path may never do.
func TestIndexedSettledFileIsNotEnqueuedForFetching(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	present := filepath.Join(root, "Artist", "Album", "01. Harbor Bells.flac")
	writeSettledMovedFile(t, present, "Artist", "Harbor Bells")

	scanAndPersist(t, ctx, sqlDB, libID, root, true)

	var status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM scan_results WHERE file_path = ?`, present,
	).Scan(&status); err != nil {
		t.Fatalf("read indexed row: %v", err)
	}
	if status != scan.StatusDone {
		t.Errorf("indexed settled row status = %q; want %q (a pending row would be enqueued for a fetch)", status, scan.StatusDone)
	}

	var pending int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scan_results WHERE library_id = ? AND status = 'pending'`, libID,
	).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending scan_results = %d; want 0", pending)
	}
}

// Stamping an indexed row 'done' must NOT freeze the track out of future
// upgrades. This is the obvious worry about the #786 status choice: if 'done'
// were terminal, indexing a settled .txt would permanently deny it the synced
// promotion --upgrade exists to deliver, turning a bookkeeping fix into silent
// data loss.
//
// It does not, because --upgrade drives the upsert with ForceStatus, which
// overwrites the stored status rather than preserving it (repository.go's
// baseUpsert appends `status = excluded.status` only under that flag). Verified
// end to end rather than argued from the flag: an ordinary scan indexes the row
// 'done', and a subsequent --upgrade pass reopens the same file and returns it
// to 'pending'.
func TestIndexedSettledRowIsStillUpgradeable(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	p := filepath.Join(root, "Artist", "Album", "01. Harbor Bells.flac")
	// A settled unsynced .txt: the upgrade-eligible population.
	writeSettledMovedFileExt(t, p, "Artist", "Harbor Bells", ".txt")

	repo := scan.New(sqlDB)
	sc := scanner.NewScanner(scanner.WithIndexStore(repo))

	scanPass := func(upgrade bool) int {
		t.Helper()
		res, err := sc.ScanLibrary(ctx, root, scanner.ScanOptions{MaxDepth: 100, Upgrade: upgrade})
		if err != nil {
			t.Fatalf("ScanLibrary(upgrade=%v): %v", upgrade, err)
		}
		for i := range res {
			res[i].LibraryID = libID
			if res[i].Status == "" {
				res[i].Status = scan.StatusPending
			}
		}
		// --upgrade is what sets ForceStatus in the real scheduler.
		if err := repo.Upsert(ctx, libID, res, scan.UpsertOptions{ForceStatus: upgrade}); err != nil {
			t.Fatalf("Upsert(upgrade=%v): %v", upgrade, err)
		}
		return len(res)
	}
	statusOf := func() string {
		t.Helper()
		var st string
		if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE file_path = ?`, p).Scan(&st); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return st
	}

	scanPass(false)
	if got := statusOf(); got != scan.StatusDone {
		t.Fatalf("after an ordinary scan status = %q; want %q", got, scan.StatusDone)
	}

	if n := scanPass(true); n != 1 {
		t.Fatalf("--upgrade emitted %d results; want 1 (it must reopen the settled .txt)", n)
	}
	if got := statusOf(); got != scan.StatusPending {
		t.Errorf("after --upgrade status = %q; want %q -- the indexed row is frozen out of upgrades", got, scan.StatusPending)
	}
}
