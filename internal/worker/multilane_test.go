package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// delayFetcher is a concurrency-safe fetcher with a configurable per-call delay
// that honors context cancellation, used to drive parallel-mode races at the
// worker level. Each lane gets its own instance, so calls is per-lane.
type delayFetcher struct {
	song  models.Song
	err   error
	delay time.Duration
	calls int32
}

func (f *delayFetcher) FindLyrics(ctx context.Context, _ models.Track) (models.Song, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return models.Song{}, ctx.Err()
		}
	}
	if f.err != nil {
		return models.Song{}, f.err
	}
	return f.song, nil
}

// capturingWriter records the songs handed to it so a test can assert which lane's
// result (synced vs unsynced) the orchestrator committed.
type capturingWriter struct{ songs []models.Song }

func (w *capturingWriter) WriteLRC(s models.Song, _ string, _ string) error {
	w.songs = append(w.songs, s)
	return nil
}

func syncedTrackSong(track models.Track) models.Song {
	return models.Song{Track: track, Subtitles: models.Synced{Lines: []models.Lines{{Text: "synced"}}}}
}

func unsyncedTrackSong(track models.Track) models.Song {
	return models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "unsynced body"}}
}

// TestProvidersModeDefaultsToOrdered asserts a freshly constructed worker uses
// ordered dispatch (parallel is strictly opt-in).
func TestProvidersModeDefaultsToOrdered(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	if w.mode != orchestrator.ModeOrdered {
		t.Fatalf("default mode = %q; want %q", w.mode, orchestrator.ModeOrdered)
	}
}

// TestSetProvidersModeInvalidRollsBack asserts that an unknown mode (which would
// fail the orchestrator rebuild) is rolled back, so w.mode never diverges from the
// live orchestrator and later setters keep working.
func TestSetProvidersModeInvalidRollsBack(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetProvidersMode("parallel")
	w.SetProvidersMode("sequential") // invalid: rebuild fails, must roll back

	if w.mode != orchestrator.ModeParallel {
		t.Fatalf("mode = %q; want %q (invalid mode must roll back to the prior valid mode)", w.mode, orchestrator.ModeParallel)
	}
	// A subsequent valid setter must still succeed (the orchestrator was not wedged).
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{}))
	if len(w.lanes) != 2 {
		t.Fatalf("lanes = %d; want 2 (later setters must still work after a rejected mode)", len(w.lanes))
	}
}

// TestRunOnceParallelSyncedPreemptsUnsynced verifies that in parallel mode a
// faster unsynced primary result is held long enough for a slower synced fallback
// to preempt it, so the committed (written) result is the synced one.
func TestRunOnceParallelSyncedPreemptsUnsynced(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     30,
		Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	primary := &delayFetcher{song: unsyncedTrackSong(track)}
	secondary := &delayFetcher{song: syncedTrackSong(track), delay: 40 * time.Millisecond}
	writer := &capturingWriter{}

	w := New(q, &fakeCache{}, primary, writer)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondary))
	w.SetProvidersMode(orchestrator.ModeParallel) // after fallback: order-independent rebuild
	w.SetRaceWait(500 * time.Millisecond)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(writer.songs) != 1 {
		t.Fatalf("writes = %d; want 1", len(writer.songs))
	}
	if len(writer.songs[0].Subtitles.Lines) == 0 {
		t.Fatalf("written song = %+v; want the synced result (synced preempts held unsynced within the window)", writer.songs[0])
	}
}

// TestRunOnceParallelWindowElapsesCommitsUnsynced verifies that when the synced
// upgrade arrives after the race window, the held unsynced result is committed.
func TestRunOnceParallelWindowElapsesCommitsUnsynced(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     31,
		Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	primary := &delayFetcher{song: unsyncedTrackSong(track)}
	secondary := &delayFetcher{song: syncedTrackSong(track), delay: 200 * time.Millisecond}
	writer := &capturingWriter{}

	w := New(q, &fakeCache{}, primary, writer)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondary))
	w.SetProvidersMode(orchestrator.ModeParallel)
	w.SetRaceWait(20 * time.Millisecond)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(writer.songs) != 1 {
		t.Fatalf("writes = %d; want 1", len(writer.songs))
	}
	if writer.songs[0].Lyrics.LyricsBody == "" || len(writer.songs[0].Subtitles.Lines) != 0 {
		t.Fatalf("written song = %+v; want the unsynced result (window elapsed before the synced upgrade)", writer.songs[0])
	}
}

// TestRunOnceFallsBackToSecondaryLaneOnPrimaryMiss verifies that when the
// primary (Musixmatch) lane returns a benign miss, the orchestrator advances to
// the registered fallback lane and its suitable result is used.
func TestRunOnceFallsBackToSecondaryLaneOnPrimaryMiss(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     10,
		Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	primary := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrNotFound)}
	secondaryFetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "secondary lyrics"},
	}}
	writer := &fakeWriter{}

	w := New(q, &fakeCache{}, primary, writer)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondaryFetcher))

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("primary calls = %d; want 1", primary.calls)
	}
	if secondaryFetcher.calls != 1 {
		t.Fatalf("secondary calls = %d; want 1 (fallback should be consulted on a primary miss)", secondaryFetcher.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 10 {
		t.Fatalf("completed = %v; want [10] (secondary hit completes the item, not deferred)", q.completed)
	}
	if len(writer.writes) != 1 {
		t.Fatalf("writes = %d; want 1 (secondary lyrics written)", len(writer.writes))
	}
}

// TestRunOncePrimaryHitSkipsSecondaryLane verifies that a suitable primary
// result short-circuits dispatch: the fallback lane is never consulted.
func TestRunOncePrimaryHitSkipsSecondaryLane(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     11,
		Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	primary := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "primary lyrics"},
	}}
	secondaryFetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "secondary lyrics"},
	}}

	w := New(q, &fakeCache{}, primary, &fakeWriter{})
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondaryFetcher))

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("primary calls = %d; want 1", primary.calls)
	}
	if secondaryFetcher.calls != 0 {
		t.Fatalf("secondary calls = %d; want 0 (a suitable primary hit must not consult the fallback)", secondaryFetcher.calls)
	}
}

// TestFallbackLanesHaveIndependentBreakers verifies each lane owns a distinct
// breaker (never a shared pool), so tripping one cannot pause a sibling.
func TestFallbackLanesHaveIndependentBreakers(t *testing.T) {
	primary := &fakeFetcher{}
	w := New(&fakeQueue{}, &fakeCache{}, primary, &fakeWriter{})
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{}))

	if len(w.lanes) != 2 {
		t.Fatalf("lanes = %d; want 2", len(w.lanes))
	}
	if w.lanes[0].Breaker() == w.lanes[1].Breaker() {
		t.Fatal("primary and fallback lanes share a breaker; each lane must own an independent breaker")
	}
}

// TestCircuitConfigAppliesToFallbackLanes verifies the circuit-config setters
// fan out to EVERY lane's breaker, including a fallback registered after them,
// and that a non-positive value is ignored rather than fanned out.
func TestCircuitConfigAppliesToFallbackLanes(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetCircuitOpenDuration(7 * time.Minute)
	w.SetCircuitBackoff(90*time.Second, 7*time.Minute)
	// Non-positive values are ignored (no panic, no override of the stored config).
	w.SetCircuitOpenDuration(0)
	w.SetCircuitBackoff(0, 0)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{}))

	if len(w.lanes) != 2 {
		t.Fatalf("lanes = %d; want 2", len(w.lanes))
	}
	// Trip each lane's breaker once; the open window must reflect the configured
	// 90s trip-1 backoff (the fallback inherited the same parameters), proving the
	// config reached the fallback breaker and not just the primary.
	for i, l := range w.lanes {
		b := l.Breaker()
		res := b.Trip()
		if res.Window != 90*time.Second {
			t.Fatalf("lane %d trip-1 window = %v; want 90s (configured backoff base must reach every lane)", i, res.Window)
		}
	}
}

// TestFallbackBreakerIndependentOfPrimary asserts that tripping the primary
// lane's breaker leaves the sibling fallback lane available (no shared pool).
func TestFallbackBreakerIndependentOfPrimary(t *testing.T) {
	primary := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrRateLimited)}
	w := New(&fakeQueue{}, &fakeCache{}, primary, &fakeWriter{})
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{}))
	primaryLane, secondaryLane := w.lanes[0], w.lanes[1]

	// Trip the primary by driving its rate-limited fetcher through the lane.
	if _, err := primaryLane.FindLyrics(context.Background(), models.Track{}, ""); err == nil {
		t.Fatal("primary FindLyrics returned nil error; expected the rate-limit signal")
	}
	if got := primaryLane.Breaker().Allow(); got != circuit.StateOpen {
		t.Fatalf("primary breaker = %v; want open after a rate-limit trip", got)
	}
	if got := secondaryLane.Breaker().Allow(); got != circuit.StateClosed {
		t.Fatalf("secondary breaker = %v; want closed (a primary trip must not pause the sibling)", got)
	}
}

// TestFallbackBreakersConcurrentlyDrivable race-tests that the two lanes' breakers
// can be driven concurrently without interference: the primary is hammered with
// trips while the secondary records successes, and the secondary stays closed.
func TestFallbackBreakersConcurrentlyDrivable(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{}))
	primaryBreaker, secondaryBreaker := w.lanes[0].Breaker(), w.lanes[1].Breaker()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); primaryBreaker.Trip() }()
		go func() { defer wg.Done(); secondaryBreaker.RecordSuccess() }()
	}
	wg.Wait()
	if got := secondaryBreaker.Allow(); got != circuit.StateClosed {
		t.Fatalf("secondary breaker = %v; want closed (only the primary was tripped)", got)
	}
}

// TestRunOnceStaleProvidersVersionBypassesCache verifies that a dequeued item
// whose stored providers_version differs from the worker's configured
// generation skips the cache and re-fetches against the current provider set.
func TestRunOnceStaleProvidersVersionBypassesCache(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	// The cache holds a previously-vetted result; a fresh fetch would override it.
	cached, err := encodeSong(models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "stale cached"}})
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:               20,
		ProvidersVersion: 7, // enqueued under an older provider set
		Inputs:           models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "fresh"}}}

	w := New(q, &fakeCache{hit: cached}, fetcher, &fakeWriter{})
	w.SetProvidersVersion(42) // current generation differs from the item's 7

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d; want 1 (stale generation must bypass the cache and re-fetch)", fetcher.calls)
	}
}

// TestRunOnceMatchingProvidersVersionUsesCache verifies that when the item's
// stored generation equals the worker's configured generation, the cache hit is
// honored and no provider fetch occurs.
func TestRunOnceMatchingProvidersVersionUsesCache(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	cached, err := encodeSong(models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "cached"}})
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:               21,
		ProvidersVersion: 42,
		Inputs:           models.Inputs{Track: track, Outdir: "out", Filename: "artist-title.lrc"},
	}}}
	fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "fresh"}}}

	w := New(q, &fakeCache{hit: cached}, fetcher, &fakeWriter{})
	w.SetProvidersVersion(42)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("fetcher calls = %d; want 0 (matching generation must honor the cache hit)", fetcher.calls)
	}
}
