package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

func TestRepo_UpsertRoundTripsAlbumMetadata(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	repo := scan.New(sqlDB)
	in := models.ScanResult{
		FilePath: "/music/song.mp3",
		Track: models.Track{
			ArtistName:  "Lady Gaga feat. Bradley Cooper",
			TrackName:   "Shallow",
			AlbumName:   "A Star Is Born",
			AlbumArtist: "Lady Gaga",
		},
		Outdir:   "/music",
		Filename: "song.lrc",
	}
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{in}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.ListByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows; want 1", len(got))
	}
	if got[0].Track.AlbumName != "A Star Is Born" {
		t.Errorf("AlbumName = %q; want %q", got[0].Track.AlbumName, "A Star Is Born")
	}
	if got[0].Track.AlbumArtist != "Lady Gaga" {
		t.Errorf("AlbumArtist = %q; want %q", got[0].Track.AlbumArtist, "Lady Gaga")
	}
}
