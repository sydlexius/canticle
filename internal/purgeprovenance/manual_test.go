package purgeprovenance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const markSQL = `UPDATE work_queue SET manual_instrumental_at = '2026-09-01T00:00:00Z' WHERE id = ?`

// #1405: a sidecar linked to a manually marked row is never purged, even when
// --source names the manual lane; an unmarked sibling is still purged.
func TestRun_ManualMarkedRowIsNeverPurged(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "ArtistA")
	marked, plain := filepath.Join(dir, "marked.lrc"), filepath.Join(dir, "plain.lrc")
	writeSidecar(t, marked, "manual")
	writeSidecar(t, plain, "manual")
	_, wqMarked := seedTrack(t, ctx, sqlDB, libID, dir, "marked.lrc", "done")
	_, wqPlain := seedTrack(t, ctx, sqlDB, libID, dir, "plain.lrc", "done")
	if _, err := sqlDB.ExecContext(ctx, markSQL, wqMarked); err != nil {
		t.Fatal(err)
	}

	res, err := New(sqlDB).Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "manual"}, LibraryID: &libID})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedManual != 1 || res.Deleted != 1 {
		t.Fatalf("SkippedManual=%d Deleted=%d, want 1/1", res.SkippedManual, res.Deleted)
	}
	if _, err := os.Stat(marked); err != nil {
		t.Errorf("marked sidecar must survive: %v", err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Errorf("unmarked sidecar should be purged, stat err = %v", err)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wqMarked); got != "done" {
		t.Errorf("marked row status = %q, want done (not requeued)", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wqPlain); got != "deferred" {
		t.Errorf("unmarked row status = %q, want deferred", got)
	}
}

// #1405: a row marked after the index snapshot aborts the reset transaction, so
// nothing is requeued and the sidecar is not deleted.
func TestResetRowsOnce_RefusesRowMarkedSinceTheIndex(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "ArtistA")
	sr, wq := seedTrack(t, ctx, sqlDB, libID, dir, "one.lrc", "done")
	if _, err := sqlDB.ExecContext(ctx, markSQL, wq); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := New(sqlDB).resetRowsOnce(ctx, []int64{sr}, []int64{wq}, nil, "manual")
	if !errors.Is(err, errProvenanceChangedUnderfoot) {
		t.Fatalf("err = %v, want errProvenanceChangedUnderfoot", err)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wq); got != "done" {
		t.Errorf("row status = %q, want done", got)
	}
}
