package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/prune"
	"github.com/sydlexius/canticle/internal/queue"
)

// TestRelinkedRowWritesToNewOutputPath is the regression test for issue #921:
// a work_queue row's output_paths must be rewritten by prune's heuristic
// relink, not just its outdir/filename columns, because the worker's own
// outputPaths() prefers a non-empty output_paths verbatim and never falls
// back to outdir/filename once output_paths is populated (see outputPaths in
// worker.go). Before the #921 fix, relinkOne updated source_path/outdir/
// filename but left output_paths naming the pre-move directory, so a
// relinked row kept resolving to a vanished write target forever.
//
// This test exercises the REAL consumer path end to end: seed a row exactly
// as prune's own tests do (via a real SQLite db and queue.DBQueue.Enqueue, so
// output_paths is populated the same way production enqueue populates it),
// run prune.Sweep to perform the relink, Dequeue the row through the real
// queue (the same claim path the worker uses), and assert the package's own
// outputPaths(item.Inputs) -- not the raw output_paths column -- resolves to
// the NEW directory. Asserting only the database column would leave the
// worker's preference-order defect unverified; this asserts what the worker
// itself would open.
func TestRelinkedRowWritesToNewOutputPath(t *testing.T) {
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
	defer func() { _ = sqlDB.Close() }()
	lib, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	oldDir := filepath.Join(root, "ArtistX", "AlbumOld")
	oldPath := filepath.Join(oldDir, "01. track.flac")
	newDir := filepath.Join(root, "ArtistX", "AlbumNew")
	newPath := filepath.Join(newDir, "01. track.flac")

	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir old: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid) VALUES (?, ?, ?, ?, 'done', ?, ?, ?)`,
		lib.ID, oldPath, "Artist", "Title", oldDir, "01. track.flac", "mbid-921-shared")
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
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     oldDir,
		Filename:   "01. track.flac",
		SourcePath: oldPath,
		OutputPaths: []models.OutputPath{{
			Outdir:   oldDir,
			Filename: "01. track.flac",
		}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Mimic the production shape #921 reports: the row is 'failed' (a doomed
	// write already refused once) and dequeue-eligible.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	// The file moved: old location gone, new location present (as if a rescan
	// had already indexed it there under the same identity).
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write new: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)`,
		lib.ID, newPath, "Artist", "Title", newDir, "01. track.flac", "mbid-921-shared"); err != nil {
		t.Fatalf("insert present scan_result: %v", err)
	}

	p := prune.New(sqlDB)
	sweepRes, err := p.Sweep(ctx, prune.SweepOptions{Granularity: prune.Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(sweepRes.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1 (setup failed to trigger the relink path)", len(sweepRes.Relinked))
	}

	// Consume the row through the REAL queue claim path, exactly as the worker
	// does, then resolve its write target through the package's own
	// outputPaths() -- the function this whole defect lives in.
	dequeued, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if dequeued.ID != item.ID {
		t.Fatalf("dequeued id = %d, want %d", dequeued.ID, item.ID)
	}

	resolved := outputPaths(dequeued.Inputs)
	if len(resolved) != 1 {
		t.Fatalf("outputPaths() = %+v, want exactly one entry", resolved)
	}
	if resolved[0].Outdir != newDir {
		t.Errorf("outputPaths()[0].Outdir = %q, want %q (THE #921 BUG: the worker must consult the relinked location, not the pre-move one)", resolved[0].Outdir, newDir)
	}
	if resolved[0].Filename != "01. track.flac" {
		t.Errorf("outputPaths()[0].Filename = %q, want %q", resolved[0].Filename, "01. track.flac")
	}
}

// TestResurrectedRelinkWritesToNewOutputPath covers relinkOne's RESURRECT
// arm (#921 on the #740 flow): an identity-less row is retired by one sweep
// because its artist folder was renamed and the new location is not indexed
// yet; a rescan then indexes it, and the next sweep relinks AND resurrects
// the row. The resurrect UPDATE is a separate statement from the plain relink
// UPDATE, so it needs its own test: dropping output_paths from it alone
// leaves the worker resolving the pre-rename directory. Asserted through the
// worker's own outputPaths() on the dequeued item, not the column.
func TestResurrectedRelinkWritesToNewOutputPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	oldArtist := filepath.Join(root, "Old Artist")
	oldDir := filepath.Join(oldArtist, "Album")
	newDir := filepath.Join(root, "New Artist", "Album")
	const fn = "01. Winterlight.flac"
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, fn), []byte("audio"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lib, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	insertSR := func(path, status string) int64 {
		t.Helper()
		res, err := sqlDB.ExecContext(ctx,
			`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc)
			 VALUES (?, ?, ?, 'Winterlight', ?, ?, ?, '', '')`,
			lib.ID, path, filepath.Base(filepath.Dir(filepath.Dir(path))), status, filepath.Dir(path), fn)
		if err != nil {
			t.Fatalf("insert scan_result: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("scan_result id: %v", err)
		}
		return id
	}
	srID := insertSR(filepath.Join(oldDir, fn), "done")
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Old Artist", TrackName: "Winterlight"},
		Outdir:       oldDir,
		Filename:     fn,
		SourcePath:   filepath.Join(oldDir, fn),
		OutputPaths:  []models.OutputPath{{Outdir: oldDir, Filename: fn}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	// The #740 shape: the artist folder is renamed, so the old directory is gone.
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(newDir)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(oldArtist, filepath.Dir(newDir)); err != nil {
		t.Fatalf("rename: %v", err)
	}

	p := prune.New(sqlDB)
	res1, err := p.Sweep(ctx, prune.SweepOptions{Granularity: prune.Exact})
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	if len(res1.Retained) != 1 || !res1.Retained[0].Retired {
		t.Fatalf("sweep 1 did not retire the row (Retained=%d); setup missed the resurrect path", len(res1.Retained))
	}
	insertSR(filepath.Join(newDir, fn), "pending") // the rescan lands
	res2, err := p.Sweep(ctx, prune.SweepOptions{Granularity: prune.Exact})
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if len(res2.Relinked) != 1 {
		t.Fatalf("sweep 2: Relinked = %d, want 1", len(res2.Relinked))
	}

	dequeued, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v (the resurrected row must be eligible)", err)
	}
	if dequeued.ID != item.ID {
		t.Fatalf("dequeued id = %d, want %d", dequeued.ID, item.ID)
	}
	resolved := outputPaths(dequeued.Inputs)
	if len(resolved) != 1 || resolved[0].Outdir != newDir || resolved[0].Filename != fn {
		t.Errorf("outputPaths() = %+v, want exactly [{%s %s}] (THE #921 BUG on the resurrect arm: the worker must not resolve the pre-rename directory)", resolved, newDir, fn)
	}
}
