package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedRow inserts a scan_results row for filePath under libraryID with the given
// scan-side status, enqueues a linked work_queue item (writing the junction via
// the ScanResultID link), then forces the work_queue row to wqStatus. It writes
// a real file at filePath so os.Stat reflects presence. No identity (mbid/isrc)
// is stored -- use seedRowWithIdentity for the relink/retain test paths.
func seedRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, srStatus, wqStatus string) {
	t.Helper()
	seedRowWithIdentity(t, ctx, sqlDB, libraryID, filePath, srStatus, wqStatus, "", "")
}

// seedRowWithIdentity is seedRow plus a stored mbid/isrc on the scan_results
// row, exercising the #640 identity columns (migration 037).
func seedRowWithIdentity(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, srStatus, wqStatus, mbid, isrc string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		libraryID, filePath, "Artist", "Title", srStatus, filepath.Dir(filePath), filepath.Base(filePath), mbid, isrc)
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	srID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: filepath.Base(filePath)},
		SourcePath:   filePath,
		OutputPaths:  []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = ? WHERE id = ?`, wqStatus, item.ID); err != nil {
		t.Fatalf("set wq status: %v", err)
	}
	return srID
}

// seedPresentScanResult inserts a bare, unlinked scan_results row for a file
// that already exists on disk (as if a rescan had already discovered a moved
// file at its new location, before the worker enqueued anything for it),
// carrying the given identity. This is the "present-file candidate" side of a
// relink match.
func seedPresentScanResult(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, mbid, isrc string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write present source: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		libraryID, filePath, "Artist", "Title", filepath.Dir(filePath), filepath.Base(filePath), mbid, isrc,
	); err != nil {
		t.Fatalf("insert present scan_result: %v", err)
	}
}

func rowCounts(t *testing.T, ctx context.Context, sqlDB *sql.DB) (scanResults, workQueue, junction int) {
	t.Helper()
	q := func(query string) int {
		var n int
		if err := sqlDB.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}
	return q(`SELECT count(*) FROM scan_results`),
		q(`SELECT count(*) FROM work_queue`),
		q(`SELECT count(*) FROM work_queue_scan_results`)
}

func workQueueSourcePath(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64) string {
	t.Helper()
	var path string
	if err := sqlDB.QueryRowContext(ctx, `SELECT source_path FROM work_queue WHERE id = ?`, id).Scan(&path); err != nil {
		t.Fatalf("query source_path: %v", err)
	}
	return path
}

func openSeeded(t *testing.T) (context.Context, *sql.DB, int64, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	lib, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	return ctx, sqlDB, lib.ID, root
}

// TestPrunePath_WedgedRowDeletedWhenSourceGone: a failed work_queue row wedged
// against a processing scan_results row whose source file was removed, with NO
// stored identity, is pruned across both tables and the junction even under
// the reactive PolicyRelinkOrRetain -- absent identity has nothing to relink or
// defer, so it stays subject to the same genuine-delete outcome relink-or-
// retain reserves for identity-present-no-match rows. Wait: per #640 AC2,
// identity-ABSENT rows must be retained, not deleted -- see the retain variant
// below, which supersedes the old always-delete assumption this test name
// predates. This test now asserts the NEW behavior: retained, not deleted.
func TestPrunePath_RetainsWedgedRowWithNoIdentity(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistA", "AlbumA", "01. gone.flac")
	seedRow(t, ctx, sqlDB, libID, gone, "processing", "failed")

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(gone))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("pruned scan=%d wq=%d, want 0/0 (no identity: retained, not deleted)", res.ScanResults, res.WorkItems)
	}
	if len(res.Retained) != 1 {
		t.Fatalf("retained %d rows, want 1", len(res.Retained))
	}
	sr, wq, j := rowCounts(t, ctx, sqlDB)
	if sr != 1 || wq != 1 || j != 1 {
		t.Fatalf("after prune scan=%d wq=%d junction=%d, want 1/1/1 (row kept)", sr, wq, j)
	}
}

// TestSweep_GenuineDeleteWhenIdentityPresentButUnmatched: AC3 -- a gone row
// whose identity IS present but matches no candidate anywhere in the library
// still prunes under PolicyFull (the periodic sweep / CLI), so the table does
// not grow without bound.
func TestSweep_GenuineDeleteWhenIdentityPresentButUnmatched(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistDone", "01. done.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-nomatch", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanResults != 1 || res.WorkItems != 1 {
		t.Fatalf("pruned scan=%d wq=%d, want 1/1 (identity present, no match anywhere: genuine delete)", res.ScanResults, res.WorkItems)
	}
	if sr, wq, j := rowCounts(t, ctx, sqlDB); sr != 0 || wq != 0 || j != 0 {
		t.Fatalf("after prune scan=%d wq=%d junction=%d, want 0/0/0 (no leaked done row)", sr, wq, j)
	}
}

// TestSweep_SkipsUnavailableRoot: when a library root is present but EMPTY (the
// realistic unmounted-share case -- the mountpoint directory still exists but is
// empty), its rows are NOT pruned even though every child source os.Stats as
// missing. A bare os.Stat(root) would read the empty mountpoint as "present"; the
// non-empty guard is what prevents the mass deletion.
func TestSweep_SkipsUnavailableRoot(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistX", "01. x.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "processing", "failed", "mbid-x", "")
	// Remove the root's CONTENTS but keep the root directory itself, simulating an
	// unmounted share whose mountpoint remains present but empty.
	if err := os.RemoveAll(filepath.Join(root, "ArtistX")); err != nil {
		t.Fatalf("empty root: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("test setup: root should be empty, has %d entries", len(entries))
	}
	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("pruned %d/%d with an unavailable root, want 0/0", res.ScanResults, res.WorkItems)
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("rows deleted despite unavailable root: scan=%d wq=%d", sr, wq)
	}
}

// TestPrunePath_SkipsPresentSource: a row whose source file still exists is
// never touched.
func TestPrunePath_SkipsPresentSource(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	present := filepath.Join(root, "ArtistB", "01. present.flac")
	seedRow(t, ctx, sqlDB, libID, present, "pending", "pending")

	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(present))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 || len(res.Retained) != 0 || len(res.Relinked) != 0 {
		t.Fatalf("acted on scan=%d wq=%d retained=%d relinked=%d, want all 0 (source present)", res.ScanResults, res.WorkItems, len(res.Retained), len(res.Relinked))
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("rows removed despite present source: scan=%d wq=%d", sr, wq)
	}
}

// TestPrunePath_DefersInFlightProcessing: a gone source whose linked work_queue
// row is still 'processing' (the worker owns it) is deferred -- neither the
// work_queue row nor its scan_results row is touched, avoiding a half-pruned
// in-flight item.
func TestPrunePath_DefersInFlightProcessing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistC", "01. inflight.flac")
	seedRow(t, ctx, sqlDB, libID, gone, "processing", "processing")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(gone))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 || len(res.Retained) != 0 {
		t.Fatalf("acted on an in-flight item: scan=%d wq=%d retained=%d, want all 0", res.ScanResults, res.WorkItems, len(res.Retained))
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("in-flight rows removed: scan=%d wq=%d", sr, wq)
	}
}

// TestSweep_DirectoryVsExact: a single-file rename inside a surviving directory
// (the file is gone but its directory remains) is caught by Granularity: Exact
// but not by Granularity: Directory. Both rows carry no identity, so the caught
// row is retained (not deleted) under the new #640 policy.
func TestSweep_DirectoryVsExact(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "ArtistD", "AlbumD")
	surviving := filepath.Join(album, "01. kept.flac")
	renamed := filepath.Join(album, "02. renamed-away.flac")
	seedRow(t, ctx, sqlDB, libID, surviving, "pending", "pending")
	seedRow(t, ctx, sqlDB, libID, renamed, "pending", "pending")
	// The album directory survives (kept.flac remains); only renamed-away.flac is gone.
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("remove renamed: %v", err)
	}

	p := New(sqlDB)
	// Directory granularity: album dir exists, so nothing is touched.
	dirRes, err := p.Sweep(ctx, SweepOptions{Granularity: Directory})
	if err != nil {
		t.Fatalf("Sweep dir: %v", err)
	}
	if dirRes.ScanResults != 0 || len(dirRes.Retained) != 0 {
		t.Fatalf("directory sweep acted on %d/%d, want 0/0 (dir survives)", dirRes.ScanResults, len(dirRes.Retained))
	}
	// Exact granularity: the renamed-away file is gone and carries no identity,
	// so it is retained (not pruned) under the new policy.
	exactRes, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep exact: %v", err)
	}
	if exactRes.ScanResults != 0 || len(exactRes.Retained) != 1 {
		t.Fatalf("exact sweep scan=%d retained=%d, want 0/1 (no identity: retained)", exactRes.ScanResults, len(exactRes.Retained))
	}
	if sr, _, _ := rowCounts(t, ctx, sqlDB); sr != 2 {
		t.Fatalf("after exact sweep scan_results=%d, want 2 (both rows survive: kept.flac present, renamed-away.flac retained)", sr)
	}
}

// TestPrunePath_PrefixBoundary: PrunePath scoped to ".../Foo" must not reconcile
// rows under the sibling ".../Foobar", even when both sources are gone -- the
// scope is a path-boundary containment, not a string prefix.
func TestPrunePath_PrefixBoundary(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	fooGone := filepath.Join(root, "Foo", "01. a.flac")
	barGone := filepath.Join(root, "Foobar", "01. b.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, fooGone, "processing", "failed", "mbid-foo", "")
	seedRowWithIdentity(t, ctx, sqlDB, libID, barGone, "processing", "failed", "mbid-bar", "")
	// Both source files are gone, but Foobar keeps the root non-empty.
	if err := os.Remove(fooGone); err != nil {
		t.Fatalf("remove foo: %v", err)
	}
	if err := os.Remove(barGone); err != nil {
		t.Fatalf("remove bar: %v", err)
	}
	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Join(root, "Foo"))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if len(res.Retained) != 1 || res.Retained[0].SourcePath != fooGone {
		t.Fatalf("retained %v, want exactly [%s] (only Foo, not Foobar)", res.Retained, fooGone)
	}
	// Foobar's row must survive untouched (identity present, not yet resolved
	// under the reactive relink-or-retain policy -- and out of scope besides).
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 2 || wq != 2 {
		t.Fatalf("sibling Foobar row touched: scan=%d wq=%d, want 2/2", sr, wq)
	}
}

// TestSweep_LibraryScoped: with LibraryID set, only the target library's rows
// are reconciled; a vanished source in another library is left untouched.
func TestSweep_LibraryScoped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	rootA := filepath.Join(dir, "libA")
	rootB := filepath.Join(dir, "libB")
	libA, err := library.New(sqlDB).Add(ctx, rootA, "libA", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add libA: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, rootB, "libB", models.LibrarySettings{}); err != nil {
		t.Fatalf("add libB: %v", err)
	}
	goneA := filepath.Join(rootA, "ArtistA", "01. a.flac")
	goneB := filepath.Join(rootB, "ArtistB", "01. b.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libA.ID, goneA, "processing", "failed", "mbid-a", "")
	// libB row: seed under library_id 2 (the second Add). Its file is also gone.
	seedRowWithIdentity(t, ctx, sqlDB, libA.ID+1, goneB, "processing", "failed", "mbid-b", "")
	if err := os.Remove(goneA); err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if err := os.Remove(goneB); err != nil {
		t.Fatalf("remove B: %v", err)
	}

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{LibraryID: &libA.ID, Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep scoped: %v", err)
	}
	if res.ScanResults != 1 || res.WorkItems != 1 {
		t.Fatalf("scoped sweep pruned scan=%d wq=%d, want 1/1 (only libA)", res.ScanResults, res.WorkItems)
	}
	// libB's gone row must survive the libA-scoped sweep.
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("scoped sweep touched the other library: scan=%d wq=%d, want 1/1", sr, wq)
	}
}

// TestSweep_DryRunReportsWithoutMutating: dry-run computes and reports the prune
// set (via the Report hook) but leaves the database untouched.
func TestSweep_DryRunReportsWithoutMutating(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistE", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "processing", "failed", "mbid-e-nomatch", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var reported int
	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{
		Granularity: Exact,
		DryRun:      true,
		Report:      func(PrunedRow) error { reported++; return nil },
	})
	if err != nil {
		t.Fatalf("Sweep dry-run: %v", err)
	}
	if res.ScanResults != 1 || reported != 1 {
		t.Fatalf("dry-run reported scan=%d hook=%d, want 1/1", res.ScanResults, reported)
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("dry-run mutated the DB: scan=%d wq=%d, want 1/1", sr, wq)
	}
}

// TestSweep_RelinkOnExactMBIDMatch: the ticket's core scenario -- a bulk move
// within a library. The old row (carrying detector telemetry via
// SetInstrumentalResult-shaped columns, simulated here via work_queue status)
// is gone from its old path, but a file bearing the same MBID is present
// elsewhere in the library (as a bare, unlinked scan_results row -- as if a
// rescan had already discovered it). The exact tier relinks: work_queue's
// source_path moves to the new location, the stale scan_results row is
// deleted, and the work_queue row itself -- and everything on it -- survives.
func TestSweep_RelinkOnExactMBIDMatch(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistF", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistF", "AlbumNew", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "mbid-f-shared", "")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-f-shared", "")

	p := New(sqlDB)
	var relinked []RelinkedRow
	res, err := p.Sweep(ctx, SweepOptions{
		Granularity:    Exact,
		ReportRelinked: func(rr RelinkedRow) error { relinked = append(relinked, rr); return nil },
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("relink also pruned scan=%d wq=%d, want 0/0 (the row survives, it moves)", res.ScanResults, res.WorkItems)
	}
	if len(res.Relinked) != 1 || len(relinked) != 1 {
		t.Fatalf("relinked %d rows (hook saw %d), want 1", len(res.Relinked), len(relinked))
	}
	if res.Relinked[0].OldPath != oldPath || res.Relinked[0].NewPath != newPath {
		t.Fatalf("relinked %+v, want old=%s new=%s", res.Relinked[0], oldPath, newPath)
	}
	// The surviving work_queue row (still 'done', telemetry untouched by this
	// package) now points at the new path.
	var wqID int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue WHERE status = 'done'`).Scan(&wqID); err != nil {
		t.Fatalf("query surviving work_queue row: %v", err)
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, wqID); got != newPath {
		t.Fatalf("work_queue.source_path = %q, want %q", got, newPath)
	}
	// Exactly one scan_results row remains (the present one at newPath); the
	// stale gone-path row was deleted as part of the relink.
	var remainingPath string
	if err := sqlDB.QueryRowContext(ctx, `SELECT file_path FROM scan_results`).Scan(&remainingPath); err != nil {
		t.Fatalf("query remaining scan_result: %v", err)
	}
	if remainingPath != newPath {
		t.Fatalf("remaining scan_results.file_path = %q, want %q", remainingPath, newPath)
	}
}

// TestSweep_RelinkOnExactISRCMatch: same as the MBID case, but the identity
// signal is ISRC-only (no MBID on either side), covering AC1's "or ISRC" half.
func TestSweep_RelinkOnExactISRCMatch(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistG", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistG", "AlbumNew", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "", "isrc-g-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "", "isrc-g-shared")

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 || res.Relinked[0].NewPath != newPath {
		t.Fatalf("Relinked = %+v, want one row to %s", res.Relinked, newPath)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("ISRC relink also pruned scan=%d wq=%d, want 0/0", res.ScanResults, res.WorkItems)
	}
}

// TestSweep_RetainsOnAmbiguousIdentity: AC2 -- when a gone row's identity
// matches MORE THAN ONE present candidate, it is retained and reported, never
// guessed at.
func TestSweep_RetainsOnAmbiguousIdentity(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistH", "AlbumOld", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "mbid-h-shared", "")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	dupeA := filepath.Join(root, "ArtistH", "Dup1", "01. track.flac")
	dupeB := filepath.Join(root, "ArtistH", "Dup2", "01. track.flac")
	seedPresentScanResult(t, ctx, sqlDB, libID, dupeA, "mbid-h-shared", "")
	seedPresentScanResult(t, ctx, sqlDB, libID, dupeB, "mbid-h-shared", "")

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 0 || res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("ambiguous identity acted on: relinked=%d scan=%d wq=%d, want 0/0/0", len(res.Relinked), res.ScanResults, res.WorkItems)
	}
	if len(res.Retained) != 1 || res.Retained[0].SourcePath != oldPath {
		t.Fatalf("retained %v, want exactly [%s]", res.Retained, oldPath)
	}
	// Nothing was deleted: the original gone row and both duplicate candidates
	// all survive.
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 3 || wq != 1 {
		t.Fatalf("after ambiguous sweep scan=%d wq=%d, want 3/1 (nothing deleted)", sr, wq)
	}
}

// TestPrunePath_RelinkOrRetainNeverGenuineDeletes: the reactive path (#640 AC5
// / Design Choice 3) never performs a genuine delete even when identity is
// present and currently unmatched -- it retains, deferring to the periodic
// sweep, to avoid racing a rescan of the file's new location that has not yet
// run.
func TestPrunePath_RelinkOrRetainNeverGenuineDeletes(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistI", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "mbid-i-nomatch-yet", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(gone))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("reactive path genuinely deleted: scan=%d wq=%d, want 0/0", res.ScanResults, res.WorkItems)
	}
	if len(res.Retained) != 1 {
		t.Fatalf("retained %d rows, want 1", len(res.Retained))
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("reactive path mutated rows: scan=%d wq=%d, want 1/1 (kept)", sr, wq)
	}
}

// TestSweep_RelinkDryRunReportsWithoutMutating: a relink candidate under
// DryRun is reported via ReportRelinked but the database is left untouched.
func TestSweep_RelinkDryRunReportsWithoutMutating(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistJ", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistJ", "AlbumNew", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "mbid-j-shared", "")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-j-shared", "")

	var reported int
	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{
		Granularity:    Exact,
		DryRun:         true,
		ReportRelinked: func(RelinkedRow) error { reported++; return nil },
	})
	if err != nil {
		t.Fatalf("Sweep dry-run: %v", err)
	}
	if len(res.Relinked) != 1 || reported != 1 {
		t.Fatalf("dry-run relinked=%d hook=%d, want 1/1", len(res.Relinked), reported)
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, mustWorkQueueID(t, ctx, sqlDB)); got != oldPath {
		t.Fatalf("dry-run mutated source_path to %q, want unchanged %q", got, oldPath)
	}
	if sr, _, _ := rowCounts(t, ctx, sqlDB); sr != 2 {
		t.Fatalf("dry-run mutated scan_results: %d rows, want 2 (both survive)", sr)
	}
}

func mustWorkQueueID(t *testing.T, ctx context.Context, sqlDB *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("query work_queue id: %v", err)
	}
	return id
}

// TestSetIdentityKeys_ISRCFirstHonorsOperatorOrder: SetIdentityKeys overrides
// the default mbid-first order, matching config.RealignConfig.IdentityKeys so
// prune and realign can never disagree about key precedence. With isrc first
// and a row carrying an isrc match plus an unrelated (non-matching) mbid, the
// isrc-first order still resolves it via the earlier-checked key.
func TestSetIdentityKeys_ISRCFirstHonorsOperatorOrder(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistK", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistK", "AlbumNew", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "mbid-k-unrelated", "isrc-k-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "", "isrc-k-shared")

	p := New(sqlDB)
	p.SetIdentityKeys([]string{"isrc", "mbid"})
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 || res.Relinked[0].NewPath != newPath {
		t.Fatalf("Relinked = %+v, want one row to %s", res.Relinked, newPath)
	}
}

// TestSetIdentityKeys_IgnoresEmptyOrInvalidOverride: passing no valid keys
// leaves the existing (default) order in place rather than disabling the
// exact tier altogether.
func TestSetIdentityKeys_IgnoresEmptyOrInvalidOverride(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	oldPath := filepath.Join(root, "ArtistL", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistL", "AlbumNew", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, oldPath, "done", "done", "mbid-l-shared", "")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, newPath, "mbid-l-shared", "")

	p := New(sqlDB)
	p.SetIdentityKeys([]string{"spotify_id", "not-a-real-key"})
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d rows, want 1 (default mbid/isrc order preserved)", len(res.Relinked))
	}
}
