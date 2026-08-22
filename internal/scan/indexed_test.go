package scan_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

// Indexed is the read side of the relocated-file repair (#786): it answers
// whether a path already has a scan_results row, so the scanner can tell a file
// it has merely SKIPPED apart from one it has never seen.
//
// Integration-tested against real SQLite rather than a mock, per the repo
// convention: the property under test is a SQL predicate, and a mock would
// assert only that Go called Go.
func TestRepo_Indexed(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}

	const known = "/music/known.mp3"
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: known,
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "known.lrc",
		Status:   "done",
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// POSITIVE CONTROL: the seeded path must read as indexed. Without this, a
	// implementation that always answered false would satisfy the absent case
	// below and the test would be vacuous.
	got, err := scanRepo.Indexed(ctx, known)
	if err != nil {
		t.Fatalf("Indexed(known): %v", err)
	}
	if !got {
		t.Errorf("Indexed(%q) = false; want true (the row was just seeded)", known)
	}

	// The relocated-file case: a path with no row must read as not indexed.
	got, err = scanRepo.Indexed(ctx, "/music/moved/elsewhere.mp3")
	if err != nil {
		t.Fatalf("Indexed(absent): %v", err)
	}
	if got {
		t.Error("Indexed(absent path) = true; want false")
	}
}

// A row belonging to ANY library counts as indexed. The scanner asks "have I
// ever seen this path", and scan_results.file_path is unique per library, so a
// library-scoped answer would re-emit a path held by another library on every
// scan.
func TestRepo_IndexedIsNotLibraryScoped(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	libA, err := libRepo.Add(ctx, "/musicA", "A", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library A: %v", err)
	}
	if _, err := libRepo.Add(ctx, "/musicB", "B", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add library B: %v", err)
	}

	const p = "/musicA/song.mp3"
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{{
		FilePath: p,
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/musicA",
		Filename: "song.lrc",
		Status:   "done",
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	got, err := scanRepo.Indexed(ctx, p)
	if err != nil {
		t.Fatalf("Indexed: %v", err)
	}
	if !got {
		t.Errorf("Indexed(%q) = false; want true regardless of owning library", p)
	}
}

// An empty path must never read as indexed. file_path is NOT NULL but the
// column has no non-empty CHECK, so a caller passing "" must get a definite
// "no" rather than matching a malformed row.
func TestRepo_IndexedEmptyPathIsNotIndexed(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	scanRepo := scan.New(sqlDB)

	got, err := scanRepo.Indexed(ctx, "")
	if err != nil {
		t.Fatalf("Indexed(empty): %v", err)
	}
	if got {
		t.Error(`Indexed("") = true; want false`)
	}
}

// Indexed must SEEK, not scan. This asserts on the query PLAN because the cost
// is invisible to a correctness test: the pre-#786 schema answered this exact
// query correctly while walking the whole index, since 003's composite
// idx_scan_results_library_file leads with library_id and SQLite can only seek
// a leftmost prefix. Measured before migration 045:
//
//	SCAN scan_results USING COVERING INDEX idx_scan_results_library_file
//
// Indexed runs once per settled file per scan, so an index scan per file makes
// a scheduled walk quadratic in library size -- and a small test database hides
// it completely. If a future schema change drops idx_scan_results_file_path or
// reorders the composite, this reddens instead of quietly getting slow.
func TestRepo_IndexedUsesAnIndexSeek(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)

	rows, err := sqlDB.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT 1 FROM scan_results WHERE file_path = ? LIMIT 1`, "/music/x.flac")
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plans []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	if len(plans) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows; the probe proved nothing")
	}

	joined := strings.Join(plans, " | ")
	if !strings.Contains(joined, "SEARCH") {
		t.Errorf("plan does not SEARCH (a full index scan per settled file): %s", joined)
	}
	if !strings.Contains(joined, "idx_scan_results_file_path") {
		t.Errorf("plan does not use idx_scan_results_file_path: %s", joined)
	}
}
