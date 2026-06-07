package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

// captureFetcher records the Track it is queried with and returns a usable song
// so the worker proceeds to the cache store, letting tests assert that the query
// artist and the cache-store key use the same resolved value.
type captureFetcher struct{ got models.Track }

func (f *captureFetcher) FindLyrics(_ context.Context, t models.Track) (models.Song, error) {
	f.got = t
	return models.Song{Track: t, Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}}}, nil
}

func TestRunOnceResolvesAlbumArtistForQueryAndCacheKey(t *testing.T) {
	tests := []struct {
		name        string
		albumArtist string
		artist      string
		wantArtist  string
	}{
		{"album artist preferred over multi-artist track field", "Lady Gaga", "Lady Gaga feat. Bradley Cooper", "Lady Gaga"},
		{"various artists placeholder falls back to track artist", "Various Artists", "Real Artist", "Real Artist"},
		{"empty album artist falls back to track artist", "", "Just Artist", "Just Artist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &captureFetcher{}
			c := &fakeCache{}
			q := &fakeQueue{items: []queue.WorkItem{{
				ID: 1,
				Inputs: models.Inputs{
					Track:    models.Track{ArtistName: tt.artist, TrackName: "Title", AlbumName: "Some Album", AlbumArtist: tt.albumArtist},
					Outdir:   "o",
					Filename: "a.lrc",
				},
			}}}
			w := New(q, c, f, &fakeWriter{})
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			// The provider is queried with the resolved artist and the album hint.
			if f.got.ArtistName != tt.wantArtist {
				t.Fatalf("FindLyrics artist = %q; want %q", f.got.ArtistName, tt.wantArtist)
			}
			if f.got.AlbumName != "Some Album" {
				t.Fatalf("FindLyrics album = %q; want %q (q_album must pass through)", f.got.AlbumName, "Some Album")
			}
			// The cache is stored under the SAME resolved artist used for lookup,
			// so a later lookup hits instead of re-fetching.
			if len(c.stores) != 1 {
				t.Fatalf("cache stores = %d; want 1", len(c.stores))
			}
			if c.stores[0].artist != tt.wantArtist {
				t.Fatalf("cache store artist = %q; want %q (read/write keys must agree)", c.stores[0].artist, tt.wantArtist)
			}
		})
	}
}

// TestRunOnceBenignMissDeferFailureKeepsBackoff verifies that when a benign
// miss occurs but the deferral write fails, RunOnce surfaces the error AND
// leaves consecutiveFailures intact. Resetting the counter before the defer is
// durably recorded would silently drop the backoff state on the next run.
func TestRunOnceBenignMissDeferFailureKeepsBackoff(t *testing.T) {
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:     1,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}, Outdir: "out", Filename: "a.lrc"},
		}},
		deferErr: fmt.Errorf("queue write failed"),
	}
	fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrNotFound)}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.consecutiveFailures = 3

	if err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce = nil; want the requeue error to surface")
	}
	if w.consecutiveFailures != 3 {
		t.Fatalf("consecutiveFailures = %d; want 3 (a failed defer must not wipe backoff state)", w.consecutiveFailures)
	}
}

func TestRunOnceBenignMissResetsConsecutiveFailures(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     1,
		Inputs: models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}, Outdir: "out", Filename: "a.lrc"},
	}}}
	fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrNotFound)}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	// Simulate an earlier transient failure that elevated the counter; a healthy
	// provider round-trip that simply misses must clear it (issue: a stuck
	// counter pins the worker in permanent geometric backoff).
	w.consecutiveFailures = 3

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a benign miss is a healthy round-trip and must reset the counter)", w.consecutiveFailures)
	}
	if len(q.deferred) != 1 {
		t.Fatalf("deferred = %v; want exactly one (benign miss defers)", q.deferred)
	}
}
