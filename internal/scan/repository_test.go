package scan_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func TestRepo_UpsertAndListByLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}

	results := []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
		Status:   scan.StatusPending,
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert initial: %v", err)
	}
	results[0].Track.TrackName = "Updated Title"
	results[0].Status = scan.StatusDone
	if err := scanRepo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}

	got, err := scanRepo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListByLibrary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLibrary returned %d results; want 1", len(got))
	}
	if got[0].FilePath != "/music/a.mp3" {
		t.Errorf("FilePath = %q; want /music/a.mp3", got[0].FilePath)
	}
	if got[0].Track.TrackName != "Updated Title" {
		t.Errorf("TrackName = %q; want Updated Title", got[0].Track.TrackName)
	}
	if got[0].Outdir != "/music" || got[0].Filename != "a.lrc" {
		t.Errorf("output = %q/%q; want /music/a.lrc", got[0].Outdir, got[0].Filename)
	}
	// Default Upsert preserves status so periodic scans cannot clobber
	// terminal states recorded by the worker. Use ForceStatus for refreshes.
	if got[0].Status != scan.StatusPending {
		t.Errorf("Status = %q; want %q (default Upsert must not touch status on update)", got[0].Status, scan.StatusPending)
	}
}

func TestRepo_FindByTrackMatchesNormalizedKeys(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	results := []models.ScanResult{{
		FilePath: "/music/Bjork/Joga.mp3",
		Track:    models.Track{ArtistName: "Björk", TrackName: "Jóga"},
		Outdir:   "/music/Bjork",
		Filename: "Joga.lrc",
		Status:   scan.StatusPending,
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Webhook metadata that differs by diacritics and case must still match
	// because both sides go through NormalizeKey.
	got, err := scanRepo.FindByTrack(ctx, "bjork", "joga")
	if err != nil {
		t.Fatalf("FindByTrack: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FindByTrack returned %d; want 1", len(got))
	}
	if got[0].FilePath != "/music/Bjork/Joga.mp3" {
		t.Errorf("FilePath = %q; want /music/Bjork/Joga.mp3", got[0].FilePath)
	}

	none, err := scanRepo.FindByTrack(ctx, "Unknown", "Nothing")
	if err != nil {
		t.Fatalf("FindByTrack miss: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("FindByTrack miss returned %d; want 0", len(none))
	}
}

func TestRepo_FindByTrackOrdersNonTerminalFirst(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	// Same artist/title on two albums: one already done, one still pending.
	results := []models.ScanResult{
		{
			FilePath: "/music/live/song.mp3",
			Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
			Status:   scan.StatusDone,
		},
		{
			FilePath: "/music/studio/song.mp3",
			Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
			Status:   scan.StatusPending,
		},
	}
	if err := scanRepo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := scanRepo.FindByTrack(ctx, "Artist", "Song")
	if err != nil {
		t.Fatalf("FindByTrack: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FindByTrack returned %d; want 2", len(got))
	}
	if got[0].Status != scan.StatusPending {
		t.Errorf("first result status = %q; want pending (non-terminal rows first)", got[0].Status)
	}
}

func TestRepo_UpsertWithForceStatusOverwritesExisting(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	initial := []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Status:   scan.StatusDone,
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, initial, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert initial: %v", err)
	}
	// Forced refresh (--update / --upgrade) must re-eligible done rows for
	// re-fetch by promoting them back to pending.
	refresh := []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Status:   scan.StatusPending,
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, refresh, scan.UpsertOptions{ForceStatus: true}); err != nil {
		t.Fatalf("Upsert forced refresh: %v", err)
	}

	got, err := scanRepo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListByLibrary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLibrary returned %d results; want 1", len(got))
	}
	if got[0].Status != scan.StatusPending {
		t.Fatalf("Status = %q; want %q after ForceStatus refresh", got[0].Status, scan.StatusPending)
	}
}

func TestRepo_UpsertDefaultsStatus(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := scanRepo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListByLibrary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLibrary returned %d results; want 1", len(got))
	}
	if got[0].Status != scan.StatusPending {
		t.Fatalf("Status = %q; want %q", got[0].Status, scan.StatusPending)
	}
}

func TestRepo_UpsertPreservesExistingStatusWhenStatusUnspecified(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	initial := []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Status:   scan.StatusDone,
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, initial, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert initial: %v", err)
	}
	update := []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Updated Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
	}}
	if err := scanRepo.Upsert(ctx, lib.ID, update, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}

	got, err := scanRepo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListByLibrary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLibrary returned %d results; want 1", len(got))
	}
	if got[0].Status != scan.StatusDone {
		t.Fatalf("Status = %q; want %q", got[0].Status, scan.StatusDone)
	}
	if got[0].Track.TrackName != "Updated Title" {
		t.Fatalf("TrackName = %q; want Updated Title", got[0].Track.TrackName)
	}
}

func TestRepo_ListByLibrary_IsolatedByLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	libA, err := libRepo.Add(ctx, "/music/a", "A", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library A: %v", err)
	}
	libB, err := libRepo.Add(ctx, "/music/b", "B", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library B: %v", err)
	}
	filePath := "/shared/track.mp3"
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{{
		FilePath: filePath,
		Track:    models.Track{ArtistName: "Artist A", TrackName: "Title A"},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert library A: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libB.ID, []models.ScanResult{{
		FilePath: filePath,
		Track:    models.Track{ArtistName: "Artist B", TrackName: "Title B"},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert library B: %v", err)
	}

	gotA, err := scanRepo.ListByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("ListByLibrary A: %v", err)
	}
	gotB, err := scanRepo.ListByLibrary(ctx, libB.ID)
	if err != nil {
		t.Fatalf("ListByLibrary B: %v", err)
	}
	if len(gotA) != 1 || gotA[0].Track.ArtistName != "Artist A" {
		t.Fatalf("library A results = %+v; want Artist A only", gotA)
	}
	if len(gotB) != 1 || gotB[0].Track.ArtistName != "Artist B" {
		t.Fatalf("library B results = %+v; want Artist B only", gotB)
	}
}

func TestRepo_ListWithFilters(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	libA, err := libRepo.Add(ctx, "/music/a", "A", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library A: %v", err)
	}
	libB, err := libRepo.Add(ctx, "/music/b", "B", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library B: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{
		{FilePath: "/music/a/1.mp3", Track: models.Track{ArtistName: "A1", TrackName: "T1"}, Status: scan.StatusPending},
		{FilePath: "/music/a/2.mp3", Track: models.Track{ArtistName: "A2", TrackName: "T2"}, Status: scan.StatusDone},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libB.ID, []models.ScanResult{
		{FilePath: "/music/b/1.mp3", Track: models.Track{ArtistName: "B1", TrackName: "T1"}, Status: scan.StatusPending},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}

	// No filter returns all rows.
	all, err := scanRepo.List(ctx, scan.Filter{})
	if err != nil {
		t.Fatalf("List no filter: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List no filter = %d rows; want 3", len(all))
	}

	// Filter by library.
	libAOnly, err := scanRepo.List(ctx, scan.Filter{LibraryID: &libA.ID})
	if err != nil {
		t.Fatalf("List by library: %v", err)
	}
	if len(libAOnly) != 2 {
		t.Fatalf("List(libA) = %d rows; want 2", len(libAOnly))
	}

	// Filter by library + status.
	pendingA, err := scanRepo.List(ctx, scan.Filter{LibraryID: &libA.ID, Status: scan.StatusPending})
	if err != nil {
		t.Fatalf("List pending in A: %v", err)
	}
	if len(pendingA) != 1 || pendingA[0].FilePath != "/music/a/1.mp3" {
		t.Fatalf("List pending in A = %+v; want one row for /music/a/1.mp3", pendingA)
	}

	// Filter by status only.
	pending, err := scanRepo.List(ctx, scan.Filter{Status: scan.StatusPending})
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("List pending = %d rows; want 2", len(pending))
	}
}

func TestRepo_ClearByLibraryOnlyAffectsTargetedLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	libA, err := libRepo.Add(ctx, "/music/a", "A", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library A: %v", err)
	}
	libB, err := libRepo.Add(ctx, "/music/b", "B", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library B: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{
		{FilePath: "/music/a/1.mp3", Track: models.Track{ArtistName: "A", TrackName: "1"}},
		{FilePath: "/music/a/2.mp3", Track: models.Track{ArtistName: "A", TrackName: "2"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libB.ID, []models.ScanResult{
		{FilePath: "/music/b/1.mp3", Track: models.Track{ArtistName: "B", TrackName: "1"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}

	count, err := scanRepo.CountByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("CountByLibrary A: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountByLibrary A = %d; want 2", count)
	}

	deleted, err := scanRepo.ClearByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("ClearByLibrary A: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("ClearByLibrary A deleted %d; want 2", deleted)
	}

	leftA, err := scanRepo.ListByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("ListByLibrary A: %v", err)
	}
	if len(leftA) != 0 {
		t.Fatalf("library A still has %d scan_results; want 0", len(leftA))
	}
	leftB, err := scanRepo.ListByLibrary(ctx, libB.ID)
	if err != nil {
		t.Fatalf("ListByLibrary B: %v", err)
	}
	if len(leftB) != 1 {
		t.Fatalf("library B = %d scan_results; want 1 (ClearByLibrary leaked across libraries)", len(leftB))
	}

	// The library row itself is still there.
	if _, err := libRepo.Get(ctx, libA.ID); err != nil {
		t.Fatalf("library A row removed by ClearByLibrary: %v", err)
	}
}

func TestRepo_ListPendingByLibraryAndSetStatus(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	results := []models.ScanResult{
		{
			FilePath: "/music/a.mp3",
			Track:    models.Track{ArtistName: "Artist A", TrackName: "Title A"},
			Outdir:   "/music",
			Filename: "a.lrc",
			Status:   scan.StatusPending,
		},
		{
			FilePath: "/music/b.mp3",
			Track:    models.Track{ArtistName: "Artist B", TrackName: "Title B"},
			Status:   scan.StatusDone,
		},
	}
	if err := scanRepo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	pending, err := scanRepo.ListPendingByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListPendingByLibrary: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending results = %+v; want 1 result", pending)
	}
	if pending[0].Filename != "a.lrc" {
		t.Fatalf("pending filename = %q; want a.lrc", pending[0].Filename)
	}

	if err := scanRepo.SetStatus(ctx, []int64{pending[0].ID}, scan.StatusProcessing); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	pending, err = scanRepo.ListPendingByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListPendingByLibrary after SetStatus: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending results after SetStatus = %+v; want none", pending)
	}
}

func TestRepo_ClearByLibraryTx_CommitsViaCaller(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	libA, err := libRepo.Add(ctx, "/music/a", "A", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library A: %v", err)
	}
	libB, err := libRepo.Add(ctx, "/music/b", "B", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library B: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{
		{FilePath: "/music/a/1.mp3", Track: models.Track{ArtistName: "A", TrackName: "1"}},
		{FilePath: "/music/a/2.mp3", Track: models.Track{ArtistName: "A", TrackName: "2"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libB.ID, []models.ScanResult{
		{FilePath: "/music/b/1.mp3", Track: models.Track{ArtistName: "B", TrackName: "1"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	deleted, err := scanRepo.ClearByLibraryTx(ctx, tx, libA.ID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("ClearByLibraryTx: %v", err)
	}
	if deleted != 2 {
		_ = tx.Rollback()
		t.Fatalf("ClearByLibraryTx deleted = %d; want 2", deleted)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	leftA, err := scanRepo.ListByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("ListByLibrary A: %v", err)
	}
	if len(leftA) != 0 {
		t.Fatalf("library A still has %d scan_results after committed tx; want 0", len(leftA))
	}
	leftB, err := scanRepo.ListByLibrary(ctx, libB.ID)
	if err != nil {
		t.Fatalf("ListByLibrary B: %v", err)
	}
	if len(leftB) != 1 {
		t.Fatalf("library B has %d scan_results; want 1 (ClearByLibraryTx leaked across libraries)", len(leftB))
	}
}

func TestRepo_ClearByLibraryTx_ErrorsWhenTableMissing(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	scanRepo := scan.New(sqlDB)

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Drop scan_results inside the same transaction so the DELETE fails; the
	// error must be surfaced to the caller rather than swallowed.
	if _, err := tx.ExecContext(ctx, `DROP TABLE scan_results`); err != nil {
		t.Fatalf("drop scan_results: %v", err)
	}
	if _, err := scanRepo.ClearByLibraryTx(ctx, tx, 1); err == nil {
		t.Fatalf("ClearByLibraryTx with missing table: want error, got nil")
	}
}

func TestRepo_ClearByLibraryTx_RollbackPreservesRows(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{
		{FilePath: "/music/1.mp3", Track: models.Track{ArtistName: "A", TrackName: "1"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := scanRepo.ClearByLibraryTx(ctx, tx, lib.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ClearByLibraryTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	left, err := scanRepo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("ListByLibrary: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("library scan_results after rollback = %d; want 1 (delete must roll back with the tx)", len(left))
	}
}
