package queue

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// linkInNewLibrary adds a second scan_results row in a new library for wqID and
// links it, making wqID a row shared across libraries.
func linkInNewLibrary(t *testing.T, sqlDB *sql.DB, wqID int64, libPath, filePath string) (libID int64) {
	t.Helper()
	libID, srID := insertLibraryAndScanResult(t, sqlDB, libPath, filePath)
	if _, err := sqlDB.Exec(
		`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wqID, srID,
	); err != nil {
		t.Fatalf("link scan_result: %v", err)
	}
	return libID
}

// previewFixture: lib A has 2 retired rows (one shared with lib B), lib B has
// the shared one plus 1 own, one retired row is unlinked, and a done row that
// carries the sentinel text is not retired. Row a1 is linked to TWO
// scan_results in lib A (album plus compilation copy): it must count once for
// lib A and must not read as shared.
func previewFixture(t *testing.T) (*DBQueue, int64, int64) {
	t.Helper()
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	libA, srA1 := insertLibraryAndScanResult(t, sqlDB, "/a", "/a/1.flac")
	a1 := makeRetiredRow(t, ctx, q, srA1, "-a1")
	_, srA1b := insertLibraryAndScanResult(t, sqlDB, "/a3", "/a3/1-copy.flac")
	if _, err := sqlDB.Exec(`UPDATE scan_results SET library_id = ? WHERE id = ?`, libA, srA1b); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, a1, srA1b); err != nil {
		t.Fatal(err)
	}
	_, srA2 := insertLibraryAndScanResult(t, sqlDB, "/a2", "/a2/2.flac")
	if _, err := sqlDB.Exec(`UPDATE scan_results SET library_id = ? WHERE id = ?`, libA, srA2); err != nil {
		t.Fatal(err)
	}
	shared := makeRetiredRow(t, ctx, q, srA2, "-shared")
	libB := linkInNewLibrary(t, sqlDB, shared, "/b", "/b/2.flac")
	_, srB := insertLibraryAndScanResult(t, sqlDB, "/b2", "/b2/3.flac")
	if _, err := sqlDB.Exec(`UPDATE scan_results SET library_id = ? WHERE id = ?`, libB, srB); err != nil {
		t.Fatal(err)
	}
	makeRetiredRow(t, ctx, q, srB, "-b1")

	// Unlinked retired row.
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "U", TrackName: "U"}}, PriorityScan); err != nil {
		t.Fatal(err)
	}
	u, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RetireMiss(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	// A done row (different last_error) and a deferred row: both excluded.
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "D", TrackName: "D"}}, PriorityScan); err != nil {
		t.Fatal(err)
	}
	d, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	// Carry the sentinel text in the wrong status: only status separates it
	// from a retired row.
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET last_error = ? WHERE id = ?`, missLimitReachedError, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, d.ID, srA1); err != nil {
		t.Fatal(err)
	}
	return q, libA, libB
}

func TestRecheckRetiredPreview_Counts(t *testing.T) {
	ctx := context.Background()
	q, libA, libB := previewFixture(t)

	p, err := q.RecheckRetiredPreview(ctx)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Total != 4 {
		t.Errorf("Total = %d; want 4 (a1, shared, b1, unlinked)", p.Total)
	}
	if p.Shared != 1 {
		t.Errorf("Shared = %d; want 1", p.Shared)
	}
	if p.Unlinked != 1 {
		t.Errorf("Unlinked = %d; want 1", p.Unlinked)
	}
	got := map[int64]RetiredLibraryCount{}
	for _, c := range p.Libraries {
		got[c.LibraryID] = c
	}
	// a1 is linked twice in lib A and counts once; the shared row counts in both.
	if len(p.Libraries) != 2 || got[libA].Count != 2 || got[libB].Count != 2 {
		t.Errorf("Libraries = %+v; want lib %d=2, lib %d=2 (shared row counts in both)", p.Libraries, libA, libB)
	}
	if got[libA].Shared != 1 || got[libB].Shared != 1 {
		t.Errorf("per-library Shared = A:%d B:%d; want 1 and 1 (a1's same-library double link is not shared)",
			got[libA].Shared, got[libB].Shared)
	}
}

// TestRecheckRetiredPreview_PerLibraryShared: lib A has one unshared row while
// libs B and C share a different one. A's confirm screen must say zero of its
// rows touch another library, even though the global Shared is 1.
func TestRecheckRetiredPreview_PerLibraryShared(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	libA, srA := insertLibraryAndScanResult(t, sqlDB, "/a", "/a/1.flac")
	makeRetiredRow(t, ctx, q, srA, "-a")
	libB, srB := insertLibraryAndScanResult(t, sqlDB, "/b", "/b/1.flac")
	bc := makeRetiredRow(t, ctx, q, srB, "-bc")
	libC := linkInNewLibrary(t, sqlDB, bc, "/c", "/c/1.flac")

	p, err := q.RecheckRetiredPreview(ctx)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Shared != 1 {
		t.Errorf("global Shared = %d; want 1", p.Shared)
	}
	got := map[int64]RetiredLibraryCount{}
	for _, c := range p.Libraries {
		got[c.LibraryID] = c
	}
	if len(got) != 3 {
		t.Fatalf("Libraries = %+v; want 3", p.Libraries)
	}
	for id, want := range map[int64]int64{libA: 0, libB: 1, libC: 1} {
		if got[id].Count != 1 || got[id].Shared != want {
			t.Errorf("lib %d = %+v; want Count=1 Shared=%d", id, got[id], want)
		}
	}
}

func TestRecheckRetiredPreview_Empty(t *testing.T) {
	p, err := NewDBQueue(openQueueTestDB(t)).RecheckRetiredPreview(context.Background())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Total != 0 || p.Shared != 0 || p.Unlinked != 0 || len(p.Libraries) != 0 {
		t.Errorf("empty preview = %+v; want zero", p)
	}
}

// TestRecheckRetiredPreviewMatchesRevive pins the preview to what
// RecheckRetired really does, for the all-libraries case and a scoped case, on
// a fresh copy of the same fixture each time.
func TestRecheckRetiredPreviewMatchesRevive(t *testing.T) {
	ctx := context.Background()

	q, _, _ := previewFixture(t)
	p, err := q.RecheckRetiredPreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cnt, err := q.CountRecheckRetired(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cnt != p.Total {
		t.Errorf("CountRecheckRetired(nil) = %d; preview Total = %d", cnt, p.Total)
	}
	n, err := q.RecheckRetired(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != p.Total {
		t.Errorf("RecheckRetired(nil) revived %d; preview Total = %d", n, p.Total)
	}
	after, err := q.RecheckRetiredPreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != 0 || len(after.Libraries) != 0 {
		t.Errorf("preview after revive = %+v; want empty", after)
	}

	q2, _, _ := previewFixture(t)
	p2, err := q2.RecheckRetiredPreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p2.Libraries {
		qq, _, _ := previewFixture(t)
		id := c.LibraryID
		cnt, err := qq.CountRecheckRetired(ctx, &id)
		if err != nil {
			t.Fatal(err)
		}
		if cnt != c.Count {
			t.Errorf("CountRecheckRetired(lib %d) = %d; preview = %d", id, cnt, c.Count)
		}
		n, err := qq.RecheckRetired(ctx, &id)
		if err != nil {
			t.Fatal(err)
		}
		if n != c.Count {
			t.Errorf("RecheckRetired(lib %d) revived %d; preview = %d", id, n, c.Count)
		}
	}
}
