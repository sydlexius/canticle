package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

func strongTitle(t models.Track) bool {
	return instrumentaltitle.Classify(t.TrackName, t.AlbumName).Tier == instrumentaltitle.Strong
}

// A cached entry under a Strong instrumental-titled row reads as a miss (#1469):
// the row is enqueued, not marked done. Non-matching titles, a nil predicate, and
// a cappella, karaoke and sing-along titles (Classify reports None for them) keep the cache hit.
func TestEnqueuePending_TitleInstrumentalCacheEntryIsAMiss(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name             string
		title            string
		pred             func(models.Track) bool
		wantEnq, wantHit int
	}{
		{"strong enqueued", "Glass Harbor (Instrumental)", strongTitle, 1, 0},
		{"plain title unchanged", "Glass Harbor", strongTitle, 0, 1},
		{"nil predicate unchanged", "Glass Harbor (Instrumental)", nil, 0, 1},
		{"a cappella still cached", "Glass Harbor (A Cappella)", strongTitle, 0, 1},
		{"karaoke still cached", "Glass Harbor (Karaoke Version)", strongTitle, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB := openTestDB(t)
			repo := cache.New(sqlDB)
			song := models.Song{
				Track:  models.Track{ArtistName: "Tin Lantern Band", TrackName: tc.title},
				Lyrics: models.Lyrics{LyricsBody: "plain words\nsecond line\n"},
			}
			if err := repo.Store(ctx, "Tin Lantern Band", tc.title, 0, encodeCachedSong(t, song)); err != nil {
				t.Fatal(err)
			}
			res := &fakePendingStore{results: []models.ScanResult{{ID: 1, FilePath: "/music/a.flac", Track: models.Track{ArtistName: "Tin Lantern Band", TrackName: tc.title}}}}
			e := scan.Enqueuer{Results: res, Cache: repo, Queue: &fakeWorkQueue{}, Priority: 5, TitleInstrumental: tc.pred}
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
