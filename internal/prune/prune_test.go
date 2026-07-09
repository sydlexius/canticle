package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/library"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

// seedRow inserts a scan_results row for filePath under libraryID with the given
// scan-side status, enqueues a linked work_queue item (writing the junction via
// the ScanResultID link), then forces the work_queue row to wqStatus. It writes
// a real file at filePath so os.Stat reflects presence. Returns the scan_result
// id and work_queue id.
func seedRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, srStatus, wqStatus string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status) VALUES (?, ?, ?, ?, ?)`,
		libraryID, filePath, "Artist", "Title", srStatus)
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

// TestPrunePath_WedgedRowDeletedWhenSourceGone: the ticket's target state --
// a failed work_queue row wedged against a processing scan_results row whose
// source file was removed -- is pruned across both tables and the junction.
func TestPrunePath_WedgedRowDeletedWhenSourceGone(t *testing.T) {
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
	if res.ScanResults != 1 || res.WorkItems != 1 {
		t.Fatalf("pruned scan=%d wq=%d, want 1/1", res.ScanResults, res.WorkItems)
	}
	sr, wq, j := rowCounts(t, ctx, sqlDB)
	if sr != 0 || wq != 0 || j != 0 {
		t.Fatalf("after prune scan=%d wq=%d junction=%d, want 0/0/0", sr, wq, j)
	}
}

// TestPrunePath_DeletesDoneRowWhenSourceGone: a moved track whose lyrics already
// completed is 'done' on the queue side; the reconciler must delete it (and its
// scan_results row) rather than leak the done row while orphaning scan_results.
func TestPrunePath_DeletesDoneRowWhenSourceGone(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistDone", "01. done.flac")
	seedRow(t, ctx, sqlDB, libID, gone, "done", "done")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(gone))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 1 || res.WorkItems != 1 {
		t.Fatalf("pruned scan=%d wq=%d, want 1/1 (done row must be deleted)", res.ScanResults, res.WorkItems)
	}
	if sr, wq, j := rowCounts(t, ctx, sqlDB); sr != 0 || wq != 0 || j != 0 {
		t.Fatalf("after prune scan=%d wq=%d junction=%d, want 0/0/0 (no leaked done row)", sr, wq, j)
	}
}

// TestSweep_SkipsUnavailableRoot: when a whole library root is gone (e.g. an
// unmounted mount), its rows are NOT pruned even though every child source
// os.Stats as missing -- the unmount must not be read as a mass deletion.
func TestSweep_SkipsUnavailableRoot(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistX", "01. x.flac")
	seedRow(t, ctx, sqlDB, libID, gone, "processing", "failed")
	// Remove the ENTIRE library root, simulating an unmounted/absent library.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove root: %v", err)
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
// never pruned.
func TestPrunePath_SkipsPresentSource(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	present := filepath.Join(root, "ArtistB", "01. present.flac")
	seedRow(t, ctx, sqlDB, libID, present, "pending", "pending")

	p := New(sqlDB)
	res, err := p.PrunePath(ctx, filepath.Dir(present))
	if err != nil {
		t.Fatalf("PrunePath: %v", err)
	}
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("pruned scan=%d wq=%d, want 0/0 (source present)", res.ScanResults, res.WorkItems)
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("rows removed despite present source: scan=%d wq=%d", sr, wq)
	}
}

// TestPrunePath_DefersInFlightProcessing: a gone source whose linked work_queue
// row is still 'processing' (the worker owns it) is deferred -- neither the
// work_queue row nor its scan_results row is deleted, avoiding a half-pruned
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
	if res.ScanResults != 0 || res.WorkItems != 0 {
		t.Fatalf("pruned an in-flight item: scan=%d wq=%d, want 0/0", res.ScanResults, res.WorkItems)
	}
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("in-flight rows removed: scan=%d wq=%d", sr, wq)
	}
}

// TestSweep_DirectoryVsExact: a single-file rename inside a surviving directory
// (the file is gone but its directory remains) is caught by Granularity: Exact
// but not by Granularity: Directory.
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
	// Directory granularity: album dir exists, so nothing is pruned.
	dirRes, err := p.Sweep(ctx, SweepOptions{Granularity: Directory})
	if err != nil {
		t.Fatalf("Sweep dir: %v", err)
	}
	if dirRes.ScanResults != 0 {
		t.Fatalf("directory sweep pruned %d, want 0 (dir survives)", dirRes.ScanResults)
	}
	// Exact granularity: the renamed-away file is gone, so its row is pruned.
	exactRes, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep exact: %v", err)
	}
	if exactRes.ScanResults != 1 || exactRes.WorkItems != 1 {
		t.Fatalf("exact sweep pruned scan=%d wq=%d, want 1/1", exactRes.ScanResults, exactRes.WorkItems)
	}
	if sr, _, _ := rowCounts(t, ctx, sqlDB); sr != 1 {
		t.Fatalf("after exact sweep scan_results=%d, want 1 (kept.flac survives)", sr)
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
	seedRow(t, ctx, sqlDB, libA.ID, goneA, "processing", "failed")
	// libB row: seed under library_id 2 (the second Add). Its file is also gone.
	seedRow(t, ctx, sqlDB, libA.ID+1, goneB, "processing", "failed")
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
	seedRow(t, ctx, sqlDB, libID, gone, "processing", "failed")
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
