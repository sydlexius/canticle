package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

type seqResult struct {
	song models.Song
	err  error
}

type seqFetcher struct {
	results []seqResult
	i       int
}

func (f *seqFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	r := f.results[f.i]
	f.i++
	return r.song, r.err
}

// TestRun_HardFailureBacksOffThenBenignMissStopsIt reproduces the observed bug:
// a hard failure put the worker into a 1-minute backoff, but because a benign
// miss did not reset the consecutive-failure counter, the loop kept backing off
// before every subsequent poll. The fix resets the counter on a benign miss, so
// the backoff fires exactly once (after the failure) and not again.
func TestRun_HardFailureBacksOffThenBenignMissStopsIt(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 1, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}, Outdir: "o", Filename: "a.lrc"}},
		{ID: 2, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Two"}, Outdir: "o", Filename: "b.lrc"}},
	}}
	fetcher := &seqFetcher{results: []seqResult{
		// Item 1: a match is found, but the write fails -> a hard failure that
		// increments the consecutive-failure counter.
		{song: models.Song{Track: models.Track{ArtistName: "A", TrackName: "One"}, Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}}}},
		// Item 2: a benign miss (provider reachable, no lyrics).
		{err: fmt.Errorf("upstream: %w", musixmatch.ErrNotFound)},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{err: errors.New("disk full")})

	var delays []time.Duration
	w.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Exactly one backoff: after item 1's failure, before item 2 is polled. With
	// the bug, item 2's benign miss would leave the counter elevated and the
	// loop would back off again before the (empty) third poll -> 2 delays.
	if len(delays) != 1 {
		t.Fatalf("backoff delays = %v; want exactly 1 (benign miss must stop the backoff)", delays)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (benign miss resets it)", w.consecutiveFailures)
	}
	if len(q.failed) != 1 {
		t.Fatalf("failed = %v; want exactly the hard-failed item", q.failed)
	}
	if len(q.deferred) != 1 {
		t.Fatalf("deferred = %v; want exactly the benign-miss item", q.deferred)
	}
}
