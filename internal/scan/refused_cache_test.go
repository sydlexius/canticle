package scan_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/scan"
)

// guardLine builds a text-bearing synced cue at a whole second. Mirrors
// internal/worker's own guardLine helper (timing_guard_test.go), duplicated
// here rather than exported cross-package since it is a one-line fixture.
func guardLine(sec int, text string) models.Lines {
	return models.Lines{
		Text: text,
		Time: models.Time{Total: float64(sec), Minutes: sec / 60, Seconds: sec % 60},
	}
}

func encodeCachedSong(t *testing.T, song models.Song) string {
	t.Helper()
	b, err := json.Marshal(song)
	if err != nil {
		t.Fatalf("marshal cached song: %v", err)
	}
	return string(b)
}

// TestEnqueuePending_RefusedCategoricalCacheEntryIsAMiss is the #952 regression:
// a lyrics_cache row written before #950/#951 can hold a lyric the accept-time
// timing guard would quarantine (its last text cue overruns the audio duration
// past timing.CategoricalRatio). EnqueuePending must treat such a row as a
// cache MISS -- falling through to the timing-verdict suppression and enqueue
// path exactly as a sql.ErrNoRows miss would -- rather than marking the scan
// row done forever with nothing ever written and no lane ever consulted.
func TestEnqueuePending_RefusedCategoricalCacheEntryIsAMiss(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	const trackLength = 100 // seconds
	track := models.Track{ArtistName: "A", TrackName: "Overrun", TrackLength: trackLength}

	// Last cue at 400s against a 100s track: ratio 4.0 >= timing.CategoricalRatio
	// (1.5), so DecidePromotion judges this Categorical -> Quarantine.
	refused := models.Song{
		// ArtistName/TrackName must be non-empty: lyrics.DecodeCachedSong uses
		// their presence to distinguish a real cached Song JSON from legacy plain
		// lyrics text (see its doc comment); an empty identity here would decode
		// to a plain-text fallback and silently drop Subtitles.
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: trackLength},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(10, "wrong recording"), guardLine(400, "wrong recording")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(trackLength), encodeCachedSong(t, refused)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/overrun.flac",
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- a timing-refused cache entry must be treated as a miss and enqueued", enqueued)
	}
	if cacheHits != 0 {
		t.Fatalf("cacheHits=%d; want 0 -- a refused entry must not be counted as a served cache hit", cacheHits)
	}
	if len(work.inputs) != 1 {
		t.Fatalf("enqueued inputs=%+v; want exactly 1", work.inputs)
	}
	if len(store.status) != 1 || store.status[0].status != scan.StatusProcessing {
		t.Fatalf("status calls=%+v; want a single processing reservation, never done", store.status)
	}
	if hits, lookups := repo.CacheStats(); hits != 0 || lookups != 1 {
		t.Fatalf("CacheStats=(hits %d, lookups %d); want (0, 1) -- a refusal must not inflate the served-hit rate", hits, lookups)
	}
}

// TestEnqueuePending_WellTimedCacheEntryStillShortCircuits is the control for
// the above: a cached lyric the timing guard would NOT refuse must still mark
// the row done without enqueueing, exactly as before #952.
func TestEnqueuePending_WellTimedCacheEntryStillShortCircuits(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	const trackLength = 100
	track := models.Track{ArtistName: "A", TrackName: "WellTimed", TrackLength: trackLength}

	ok := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: trackLength},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(90, "right recording")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(trackLength), encodeCachedSong(t, ok)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       2,
		FilePath: "/music/welltimed.flac",
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 0 || len(work.inputs) != 0 {
		t.Fatalf("enqueued=%d items=%d; want 0 -- a well-timed cache entry must still short-circuit", enqueued, len(work.inputs))
	}
	if cacheHits != 1 {
		t.Fatalf("cacheHits=%d; want 1", cacheHits)
	}
	if len(store.status) != 1 || store.status[0].status != scan.StatusDone {
		t.Fatalf("status calls=%+v; want a single done stamp", store.status)
	}
	if hits, lookups := repo.CacheStats(); hits != 1 || lookups != 1 {
		t.Fatalf("CacheStats=(hits %d, lookups %d); want (1, 1)", hits, lookups)
	}
}

// TestEnqueuePending_UnknownDurationRefusedEntryStillServed pins the fail-open
// rule: when the scan result's own TrackLength is unknown (0), the timing guard
// cannot judge the cached lyric at all (timing.Evaluate returns
// UnknownDuration for durationSeconds<=0), so it must serve the cache exactly
// as before -- an overrunning cue is no longer evidence of anything when there
// is no known duration to overrun.
func TestEnqueuePending_UnknownDurationRefusedEntryStillServed(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	track := models.Track{ArtistName: "A", TrackName: "Unknown", TrackLength: 0}

	// The cached song ALSO carries no known TrackLength, so guardDurationSeconds
	// falls back to 0 too: there is nothing to compare the 400s cue against.
	cachedNoDuration := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title"},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(400, "irrelevant without a duration")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cachedNoDuration)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       3,
		FilePath: "/music/unknown.flac",
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 0 || len(work.inputs) != 0 {
		t.Fatalf("enqueued=%d items=%d; want 0 -- unknown duration must fail open and serve the cache", enqueued, len(work.inputs))
	}
	if cacheHits != 1 {
		t.Fatalf("cacheHits=%d; want 1", cacheHits)
	}
}

// An unknown FILE duration does not by itself make a cached entry servable:
// the writer's guard falls back to the cached song's own catalog length
// (guardDurationSeconds), so the scan side must refuse what the writer would
// quarantine. Accepting it here would mark the track done with nothing written,
// the failure #952 removes. The expectation is pinned to the writer's own
// decision rather than restated, so the two cannot drift apart silently.
func TestEnqueuePending_UnknownFileDurationJudgedAgainstCachedLength(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	track := models.Track{ArtistName: "A", TrackName: "UnknownFile", TrackLength: 0}
	cached := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: 100},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(10, "wrong recording"), guardLine(400, "wrong recording")}},
	}
	if decision, _, _ := lyrics.DecidePromotion(cached); decision != lyrics.Quarantine {
		t.Fatalf("fixture: writer decision = %v; want quarantine (the premise of this test)", decision)
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       4,
		FilePath: "/music/unknown-file.flac",
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 || cacheHits != 0 {
		t.Fatalf("enqueued=%d cacheHits=%d; want 1 and 0 -- an entry the writer quarantines on the catalog-length fallback must not be served", enqueued, cacheHits)
	}
}
