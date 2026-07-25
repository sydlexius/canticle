package purgeprovenance

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

// writeSidecar writes a minimal .lrc sidecar with an optional [source:] header.
func writeSidecar(t *testing.T, path, source string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "[00:01.00]hello\n"
	if source != "" {
		body = "[source:" + source + "]\n" + body
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// seedTrack inserts a scan_results row (status 'done') plus a linked
// work_queue item whose output is (outdir, filename), forcing the queue row's
// status to wqStatus. Returns the scan_result and work_queue ids.
func seedTrack(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, outdir, filename, wqStatus string) (srID, wqID int64) {
	t.Helper()
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status) VALUES (?, ?, 'Artist', 'Title', ?, ?, 'done')`,
		libraryID, filepath.Join(outdir, filename), outdir, filename)
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	srID, err = res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: filename},
		Outdir:       outdir,
		Filename:     filename,
		SourcePath:   filepath.Join(outdir, filename+".flac"),
		ScanResultID: srID,
		OutputPaths:  []models.OutputPath{{Outdir: outdir, Filename: filename}},
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = ? WHERE id = ?`, wqStatus, item.ID); err != nil {
		t.Fatalf("set wq status: %v", err)
	}
	return srID, item.ID
}

func openSeeded(t *testing.T) (ctx context.Context, sqlDB *sql.DB, libID int64, root string) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	root = filepath.Join(dir, "music")
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

func rowStatus(t *testing.T, ctx context.Context, sqlDB *sql.DB, table string, id int64) string {
	t.Helper()
	var status string
	if err := sqlDB.QueryRowContext(ctx, "SELECT status FROM "+table+" WHERE id = ?", id).Scan(&status); err != nil { //nolint:gosec // table is a test literal
		t.Fatalf("status %s id=%d: %v", table, id, err)
	}
	return status
}

// TestRun_SourceExactMatch: --source matches only sidecars whose [source:] tag
// equals the requested value exactly, and leaves other sources untouched.
func TestRun_SourceExactMatch(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	writeSidecar(t, filepath.Join(dirA, "one.lrc"), "musixmatch")
	writeSidecar(t, filepath.Join(dirA, "two.lrc"), "petitlyrics")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")
	seedTrack(t, ctx, sqlDB, libID, dirA, "two.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("got matched=%d deleted=%d, want 1/1", res.Matched, res.Deleted)
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "one.lrc")); !os.IsNotExist(statErr) {
		t.Errorf("one.lrc should be deleted")
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "two.lrc")); statErr != nil {
		t.Errorf("two.lrc should survive: %v", statErr)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "pending" {
		t.Errorf("scan_results status = %q, want pending", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "deferred" {
		t.Errorf("work_queue status = %q, want deferred", got)
	}
}

// TestRun_NoSourceCohort: --no-source matches only sidecars carrying no
// [source:] tag (the inherited/foreign cohort).
func TestRun_NoSourceCohort(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	writeSidecar(t, filepath.Join(dirA, "tagged.lrc"), "musixmatch")
	writeSidecar(t, filepath.Join(dirA, "foreign.lrc"), "")
	seedTrack(t, ctx, sqlDB, libID, dirA, "tagged.lrc", "done")
	seedTrack(t, ctx, sqlDB, libID, dirA, "foreign.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{NoSource: true}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("got matched=%d deleted=%d, want 1/1", res.Matched, res.Deleted)
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "foreign.lrc")); !os.IsNotExist(statErr) {
		t.Errorf("foreign.lrc should be deleted")
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "tagged.lrc")); statErr != nil {
		t.Errorf("tagged.lrc should survive: %v", statErr)
	}
}

// TestRun_DryRunWritesAndDeletesNothing: with DryRun set, nothing is deleted
// and no database row is mutated, but the match is still reported.
func TestRun_DryRunWritesAndDeletesNothing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")

	var reported []Record
	p := New(sqlDB)
	res, err := p.Run(ctx, Options{
		Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID, DryRun: true,
		Report: func(r Record) error { reported = append(reported, r); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Deleted != 0 {
		t.Fatalf("got matched=%d deleted=%d, want 1/0", res.Matched, res.Deleted)
	}
	if len(reported) != 1 || reported[0].Path != path {
		t.Fatalf("reported = %+v, want one record for %s", reported, path)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("dry-run must not delete the sidecar: %v", statErr)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "done" {
		t.Errorf("dry-run must not mutate scan_results; status = %q", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "done" {
		t.Errorf("dry-run must not mutate work_queue; status = %q", got)
	}
}

// TestRun_ApplyDeletesAndRequeues: without DryRun (apply mode), the matched
// sidecar is deleted and its coupled rows are requeued.
func TestRun_ApplyDeletesAndRequeues(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")

	var reported []Record
	p := New(sqlDB)
	res, err := p.Run(ctx, Options{
		Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID,
		Report: func(r Record) error { reported = append(reported, r); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 1 || res.ScanResultsReset != 1 || res.WorkItemsRequeued != 1 {
		t.Fatalf("got deleted=%d srReset=%d wqRequeued=%d, want 1/1/1", res.Deleted, res.ScanResultsReset, res.WorkItemsRequeued)
	}
	if len(reported) != 1 {
		t.Fatalf("want 1 backup record, got %d", len(reported))
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("apply must delete the sidecar")
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "pending" {
		t.Errorf("scan_results status = %q, want pending", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "deferred" {
		t.Errorf("work_queue status = %q, want deferred", got)
	}
}

// TestRun_DryRunReportErrorIsCountedButHarmless: in a dry run, a Report error
// is counted as an Errors tally entry but the run continues (nothing to
// protect, since dry-run deletes nothing regardless).
func TestRun_DryRunReportErrorIsCountedButHarmless(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{
		Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID, DryRun: true,
		Report: func(Record) error { return os.ErrPermission },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Errors != 1 {
		t.Fatalf("got matched=%d errors=%d, want 1/1", res.Matched, res.Errors)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("dry-run must never delete regardless of Report error: %v", statErr)
	}
}

// TestRun_AlreadyDeletedSidecarStillResetsRows: if the matched sidecar
// vanished from disk between matching and the delete call (a benign
// out-of-band race), os.Remove's IsNotExist is treated as already-satisfied
// rather than an error, and the coupled rows are still reset.
func TestRun_AlreadyDeletedSidecarStillResetsRows(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{
		Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID,
		Report: func(Record) error {
			// Simulate an out-of-band removal that races ahead of our own delete.
			return os.Remove(path)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 1 || res.Errors != 0 {
		t.Fatalf("got deleted=%d errors=%d, want 1/0 (a pre-vanished file is not an error)", res.Deleted, res.Errors)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "pending" {
		t.Errorf("scan_results status = %q, want pending even when the file raced away first", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "deferred" {
		t.Errorf("work_queue status = %q, want deferred even when the file raced away first", got)
	}
}

// TestRun_LibraryScopedIndexExcludesOtherLibraries: with LibraryID set, a
// matching sidecar belonging to a DIFFERENT library's scan_results row is not
// linked (its scan_results/work_queue rows are outside the index), so its
// delete carries no requeue -- proving buildIndex actually applies the
// library scope rather than loading every library's rows regardless.
func TestRun_LibraryScopedIndexExcludesOtherLibraries(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	otherRoot := filepath.Join(filepath.Dir(root), "other-music")
	otherLib, err := library.New(sqlDB).Add(ctx, otherRoot, "other", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add other: %v", err)
	}

	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	// Seed the linkage under the OTHER library's id, while the file itself
	// lives under the first library's root (Run is scoped by LibraryID at the
	// database layer only -- Roots controls the filesystem walk).
	seedTrack(t, ctx, sqlDB, otherLib.ID, dirA, "one.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("got matched=%d deleted=%d, want 1/1", res.Matched, res.Deleted)
	}
	if res.ScanResultsReset != 0 || res.WorkItemsRequeued != 0 {
		t.Fatalf("got scanResultsReset=%d workItemsRequeued=%d, want 0/0 (row belongs to a different library, out of scope)", res.ScanResultsReset, res.WorkItemsRequeued)
	}
}

// TestRun_BackupWrittenBeforeDelete: the Report callback (which the CLI wires
// to a write+fsync of the JSONL backup) runs before the sidecar is removed
// from disk, and a Report failure leaves the sidecar in place (backup-first).
func TestRun_BackupFailureLeavesSidecarInPlace(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{
		Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID,
		Report: func(Record) error { return os.ErrPermission },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 0 || res.Errors != 1 {
		t.Fatalf("got deleted=%d errors=%d, want 0/1", res.Deleted, res.Errors)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("a failed backup must leave the sidecar in place: %v", statErr)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "done" {
		t.Errorf("scan_results must be untouched; status = %q", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "done" {
		t.Errorf("work_queue must be untouched; status = %q", got)
	}
}

// TestRun_ProcessingRowsSkipped: a matched sidecar whose linked work_queue row
// is 'processing' is left entirely alone -- no delete, no reset -- so the
// worker's in-flight write is never disturbed.
func TestRun_ProcessingRowsSkipped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	path := filepath.Join(dirA, "one.lrc")
	writeSidecar(t, path, "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "processing")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedProcessing != 1 || res.Deleted != 0 {
		t.Fatalf("got skippedProcessing=%d deleted=%d, want 1/0", res.SkippedProcessing, res.Deleted)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("processing row's sidecar must survive: %v", statErr)
	}
}

// TestRun_SymlinksSkipped: a symlinked sidecar is never followed or touched,
// even when it points at a matching file, and does not count as matched.
func TestRun_SymlinksSkipped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	realPath := filepath.Join(dirA, "real.lrc")
	writeSidecar(t, realPath, "musixmatch")
	linkPath := filepath.Join(dirA, "link.lrc")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	seedTrack(t, ctx, sqlDB, libID, dirA, "real.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedSymlink != 1 {
		t.Errorf("SkippedSymlink = %d, want 1", res.SkippedSymlink)
	}
	if res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("got matched=%d deleted=%d, want 1/1 (only the real file)", res.Matched, res.Deleted)
	}
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Errorf("symlink itself must survive untouched: %v", statErr)
	}
}

// TestRun_EnumerationConfinedToRoots: a sidecar outside the configured roots is
// never examined, matched, or touched, even though it is otherwise identical
// to an in-scope one.
func TestRun_EnumerationConfinedToRoots(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "outside.lrc")
	writeSidecar(t, outsidePath, "musixmatch")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 0 || res.Matched != 0 {
		t.Fatalf("got scanned=%d matched=%d, want 0/0 (root not walked)", res.Scanned, res.Matched)
	}
	if _, statErr := os.Stat(outsidePath); statErr != nil {
		t.Errorf("out-of-root sidecar must survive: %v", statErr)
	}
}

// TestRun_SummaryCounts: a mixed batch produces the right per-category totals
// in one run (scanned includes every file examined for matching, matched is
// the filtered subset, deleted only counts what was actually removed).
func TestRun_SummaryCounts(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	writeSidecar(t, filepath.Join(dirA, "a.lrc"), "musixmatch")
	writeSidecar(t, filepath.Join(dirA, "b.lrc"), "musixmatch")
	writeSidecar(t, filepath.Join(dirA, "c.lrc"), "petitlyrics")
	seedTrack(t, ctx, sqlDB, libID, dirA, "a.lrc", "done")
	seedTrack(t, ctx, sqlDB, libID, dirA, "b.lrc", "done")
	seedTrack(t, ctx, sqlDB, libID, dirA, "c.lrc", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", res.Scanned)
	}
	if res.Matched != 2 {
		t.Errorf("Matched = %d, want 2", res.Matched)
	}
	if res.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", res.Deleted)
	}
}

// TestRun_TwoSidecarsSameStemBothMatched: a track that carries both a .lrc and
// a .txt sidecar sharing the same output directory and filename stem (the
// --upgrade cohort: an old .txt left behind alongside a newly promoted .lrc,
// or vice versa) has BOTH sidecars matched and deleted, and BOTH linked
// scan_results/work_queue rows requeued -- proving the index key is genuinely
// extension-agnostic (canonical outdir + stem), not just "should work" by
// inspection.
func TestRun_TwoSidecarsSameStemBothMatched(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirA := filepath.Join(root, "ArtistA")
	writeSidecar(t, filepath.Join(dirA, "one.lrc"), "musixmatch")
	writeSidecar(t, filepath.Join(dirA, "one.txt"), "musixmatch")
	sr1, wq1 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.lrc", "done")
	sr2, wq2 := seedTrack(t, ctx, sqlDB, libID, dirA, "one.txt", "done")

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 2 || res.Matched != 2 || res.Deleted != 2 {
		t.Fatalf("got scanned=%d matched=%d deleted=%d, want 2/2/2", res.Scanned, res.Matched, res.Deleted)
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "one.lrc")); !os.IsNotExist(statErr) {
		t.Errorf("one.lrc should be deleted")
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "one.txt")); !os.IsNotExist(statErr) {
		t.Errorf("one.txt should be deleted")
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr1); got != "pending" {
		t.Errorf("scan_results (lrc) status = %q, want pending", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", sr2); got != "pending" {
		t.Errorf("scan_results (txt) status = %q, want pending", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq1); got != "deferred" {
		t.Errorf("work_queue (lrc) status = %q, want deferred", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq2); got != "deferred" {
		t.Errorf("work_queue (txt) status = %q, want deferred", got)
	}
}

// TestRun_SymlinkedDirectoryNotDescended: a symlinked directory inside a
// configured root is never descended into by the walk (a regression lock on
// filepath.WalkDir's documented behavior -- "WalkDir does not follow symbolic
// links" -- rather than a new production guard; this must pass unmodified
// against the existing Run implementation).
func TestRun_SymlinkedDirectoryNotDescended(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	realDir := filepath.Join(root, "RealArtist")
	writeSidecar(t, filepath.Join(realDir, "real.lrc"), "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, realDir, "real.lrc", "done")

	linkedDir := filepath.Join(root, "LinkedArtist")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	p := New(sqlDB)
	res, err := p.Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Exactly one match: the real file via its real path. If WalkDir followed
	// the symlinked directory, real.lrc would be seen a second time under
	// LinkedArtist/, doubling Scanned/Matched/Deleted.
	if res.Scanned != 1 || res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("got scanned=%d matched=%d deleted=%d, want 1/1/1 (symlinked dir must not be descended into)", res.Scanned, res.Matched, res.Deleted)
	}
	if _, statErr := os.Lstat(linkedDir); statErr != nil {
		t.Errorf("the symlinked directory itself must survive: %v", statErr)
	}
}

// TestFilter_Matches covers the pure predicate directly.
func TestFilter_Matches(t *testing.T) {
	cases := []struct {
		name   string
		filter Filter
		source string
		want   bool
	}{
		{"exact match", Filter{Source: "musixmatch"}, "musixmatch", true},
		{"exact mismatch", Filter{Source: "musixmatch"}, "petitlyrics", false},
		{"exact vs empty", Filter{Source: "musixmatch"}, "", false},
		{"no-source matches empty", Filter{NoSource: true}, "", true},
		{"no-source rejects tagged", Filter{NoSource: true}, "musixmatch", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.filter.matches(c.source); got != c.want {
				t.Errorf("matches(%q) = %v, want %v", c.source, got, c.want)
			}
		})
	}
}
