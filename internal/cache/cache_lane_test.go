package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

// TestCacheEntryKeepsItsLaneAndFetchTime is #1207 over real SQLite: the lane
// and fetch time of the fetch that stored an entry round-trip through
// Store/Lookup, a later fetch from another lane overwriting the key reports
// THAT lane, and Upstream (#850) is never restored.
func TestCacheEntryKeepsItsLaneAndFetchTime(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))
	track := models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"}
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(48 * time.Hour)

	store := func(lane string, at time.Time) {
		t.Helper()
		encoded, err := lyrics.EncodeCachedSong(models.Song{
			Track:       track,
			Lyrics:      models.Lyrics{LyricsBody: "words from " + lane},
			WinningLane: lane,
			Upstream:    "lyricfind",
			FetchedAt:   at,
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := repo.Store(ctx, track.ArtistName, track.TrackName, 0, encoded); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	lookup := func() models.Song {
		t.Helper()
		raw, err := repo.Lookup(ctx, track.ArtistName, track.TrackName, 0)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		return lyrics.DecodeCachedSong(raw, track)
	}

	store("innertube", t1)
	got := lookup()
	if got.WinningLane != "innertube" || !got.FetchedAt.Equal(t1) {
		t.Errorf("hit = lane %q fetched %v; want innertube %v", got.WinningLane, got.FetchedAt, t1)
	}
	if got.Upstream != "" {
		t.Errorf("Upstream = %q; a cache hit must never restore it (#850)", got.Upstream)
	}

	store("petitlyrics", t2)
	got = lookup()
	if got.WinningLane != "petitlyrics" || !got.FetchedAt.Equal(t2) || got.Lyrics.LyricsBody != "words from petitlyrics" {
		t.Errorf("overwritten hit = lane %q fetched %v body %q; want the later petitlyrics fetch at %v",
			got.WinningLane, got.FetchedAt, got.Lyrics.LyricsBody, t2)
	}
}
