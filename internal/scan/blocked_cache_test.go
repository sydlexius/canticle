package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

// A cached body blocked for the scanned track reads as a miss (#1394), keyed on
// the scanned identity, not the cached provider track's; others still hit.
func TestEnqueuePending_BlockedCacheEntryIsAMiss(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)
	song := models.Song{
		Track:  models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title"},
		Lyrics: models.Lyrics{LyricsBody: "wrong words\nsecond line\n"},
	}
	store := lyricblock.NewStore(sqlDB, nil)
	for _, name := range []string{"Blocked", "Fine"} {
		if err := repo.Store(ctx, "Scanned "+name, "Title", 0, encodeCachedSong(t, song)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Add(ctx, sqlDB, lyricblock.Block{ArtistKey: "Scanned Blocked", TitleKey: "Title", Fingerprint: lyricblock.SongFingerprints(song)[0]}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name             string
		wantEnq, wantHit int
	}{{"Blocked", 1, 0}, {"Fine", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakePendingStore{results: []models.ScanResult{{ID: 1, FilePath: "/music/a.flac", Track: models.Track{ArtistName: "Scanned " + tc.name, TrackName: "Title"}}}}
			e := scan.Enqueuer{Results: res, Cache: repo, Queue: &fakeWorkQueue{}, Priority: 5, Blocks: store}
			enq, hits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
			if err != nil {
				t.Fatal(err)
			}
			if enq != tc.wantEnq || hits != tc.wantHit {
				t.Fatalf("enqueued=%d cacheHits=%d; want %d and %d", enq, hits, tc.wantEnq, tc.wantHit)
			}
		})
	}
}
