package scan_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
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
//
// No Durations store is wired here, so the real file duration is unresolvable;
// per the doubt-routes-to-worker rule (see duration_source_test.go) a synced
// entry is refused on that basis alone, which happens to agree with what the
// catalog-length fallback would have decided too. See
// TestEnqueuePending_ScanSideJudgesAgainstRealFileDuration for the case where a
// resolved real duration is what actually catches the refusal.
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

// TestEnqueuePending_WellTimedCacheEntryStillShortCircuits,
// TestEnqueuePending_UnknownDurationRefusedEntryStillServed, and
// TestEnqueuePending_UnknownFileDurationJudgedAgainstCachedLength used to live
// here. All three hand-set Track.TrackLength on the scan result and relied on
// RefusedByTimingGuard's catalog-length fallback to judge a cached entry -- a
// premise that does not hold in production, where ListPendingByLibrary never
// selects a duration column and Track.TrackLength is always 0 (a Copilot
// finding on PR #966, tracked as a #952 follow-up). The scan-side check now
// resolves the file's REAL duration via an injected audiodur.Store instead of
// ever consulting that fallback; their coverage moved to
// duration_source_test.go:
//   - TestEnqueuePending_WellTimedEntryServedWithKnownDuration (the well-timed
//     case, now judged against a resolved real duration)
//   - TestEnqueuePending_NoRecordedDurationRoutesDoubtToWorker and
//     TestEnqueuePending_NilDurationsRoutesSyncedDoubtToWorker (the
//     unknown-duration cases: a synced entry is no longer served on faith when
//     the real duration cannot be resolved -- doubt routes to the worker,
//     which always re-derives it)
