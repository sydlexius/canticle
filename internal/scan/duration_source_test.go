package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scan"
)

// writeTestAudioFile creates a real file at dir/name (content is irrelevant --
// only its identity, via os.Stat, matters) and returns its path.
func writeTestAudioFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not real audio, just needs to exist"), 0o600); err != nil {
		t.Fatalf("write test audio file: %v", err)
	}
	return path
}

// recordRealDuration banks seconds as path's exact audio duration, keyed
// exactly as the production wiring keys it (commands.scheduler): the canonical
// path plus the CURRENT file's mtime/size.
func recordRealDuration(ctx context.Context, t *testing.T, store *audiodur.Store, path string, seconds int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat test audio file: %v", err)
	}
	key := pathutil.CanonicalPath(path)
	if err := store.Record(ctx, key, info.ModTime().UnixNano(), info.Size(), seconds); err != nil {
		t.Fatalf("record duration: %v", err)
	}
}

// TestEnqueuePending_ScanSideJudgesAgainstRealFileDuration is the #952-follow-up
// production-shape regression (Copilot finding on PR #966): ListPendingByLibrary
// never selects a duration column (scan_results has none), so a pending row's
// Track.TrackLength is ALWAYS 0 in production -- never the value the fixtures in
// refused_cache_test.go hand-set. Without a resolved file duration, the accept
// check falls back to the CACHED song's own catalog TrackLength, which a lyric
// is rarely miscategorized against, so a genuinely categorical (wrong-recording)
// cached lyric was served as a hit and the row marked done forever with nothing
// written. Wiring a real audiodur.Store lookup must catch what the catalog-length
// fallback misses.
func TestEnqueuePending_ScanSideJudgesAgainstRealFileDuration(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)
	durations := audiodur.New(sqlDB, "test-reader-v1")

	dir := t.TempDir()
	path := writeTestAudioFile(t, dir, "overrun.flac")
	const realFileSeconds = 100
	recordRealDuration(ctx, t, durations, path, realFileSeconds)

	// Production shape: the scan result's own TrackLength is 0 (no column to
	// read it from), but the cached song carries its own catalog length (500s),
	// against which the 400s cue looks fine (ratio 0.8). Against the file's
	// REAL duration (100s) the same cue is categorical (ratio 4.0).
	track := models.Track{ArtistName: "A", TrackName: "Overrun", TrackLength: 0}
	cached := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: 500},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(10, "wrong recording"), guardLine(400, "wrong recording")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: path,
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5, Durations: durations}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- a cache entry categorical against the REAL file duration must be enqueued, not served", enqueued)
	}
	if cacheHits != 0 {
		t.Fatalf("cacheHits=%d; want 0", cacheHits)
	}
	if len(store.status) != 1 || store.status[0].status != scan.StatusProcessing {
		t.Fatalf("status calls=%+v; want a single processing reservation, never done", store.status)
	}
}

// TestEnqueuePending_NoRecordedDurationRoutesDoubtToWorker: when the resolved
// file duration is unknown (no audiodur row recorded for this file yet -- the
// common case for a track scanned before its first fetch), a SYNCED cached
// entry must NOT be accepted on the scan side: accepting it here is
// irreversible (the row is marked done and the track never reaches the
// worker, which is the one path that re-reads the real duration). Doubt
// routes to the worker instead.
func TestEnqueuePending_NoRecordedDurationRoutesDoubtToWorker(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)
	durations := audiodur.New(sqlDB, "test-reader-v1")

	dir := t.TempDir()
	path := writeTestAudioFile(t, dir, "unrecorded.flac")
	// Deliberately NOT recording a duration for path.

	track := models.Track{ArtistName: "A", TrackName: "Unrecorded", TrackLength: 0}
	cached := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: 500},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(90, "could be fine or could be wrong")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       2,
		FilePath: path,
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5, Durations: durations}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 || cacheHits != 0 {
		t.Fatalf("enqueued=%d cacheHits=%d; want (1, 0) -- an unresolvable file duration must route a synced entry to the worker", enqueued, cacheHits)
	}
}

// TestEnqueuePending_UnsyncedEntryServedWithoutDuration: an unsynced/plain
// cached entry carries no line timing to refuse at all, so it must still be
// served as a cache hit even when the file duration cannot be resolved --
// unlike the synced case above, there is no doubt to route anywhere.
func TestEnqueuePending_UnsyncedEntryServedWithoutDuration(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)
	durations := audiodur.New(sqlDB, "test-reader-v1")

	dir := t.TempDir()
	path := writeTestAudioFile(t, dir, "plain.flac")
	// No duration recorded for path.

	track := models.Track{ArtistName: "A", TrackName: "Plain", TrackLength: 0}
	cached := models.Song{
		Track:  models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title"},
		Lyrics: models.Lyrics{LyricsBody: "just some words, never timed"},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       3,
		FilePath: path,
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5, Durations: durations}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 0 || cacheHits != 1 {
		t.Fatalf("enqueued=%d cacheHits=%d; want (0, 1) -- an unsynced entry has no timing to refuse", enqueued, cacheHits)
	}
	if len(store.status) != 1 || store.status[0].status != scan.StatusDone {
		t.Fatalf("status calls=%+v; want a single done stamp", store.status)
	}
}

// TestEnqueuePending_WellTimedEntryServedWithKnownDuration is the positive
// control: a real, resolved file duration plus a synced entry that is NOT
// categorical against it must still short-circuit as a hit, exactly as
// before this change.
func TestEnqueuePending_WellTimedEntryServedWithKnownDuration(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)
	durations := audiodur.New(sqlDB, "test-reader-v1")

	dir := t.TempDir()
	path := writeTestAudioFile(t, dir, "welltimed.flac")
	const realFileSeconds = 100
	recordRealDuration(ctx, t, durations, path, realFileSeconds)

	track := models.Track{ArtistName: "A", TrackName: "WellTimed", TrackLength: 0}
	cached := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: 100},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(90, "right recording")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       4,
		FilePath: path,
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5, Durations: durations}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 0 || cacheHits != 1 {
		t.Fatalf("enqueued=%d cacheHits=%d; want (0, 1) -- a well-timed entry judged against the real duration must still short-circuit", enqueued, cacheHits)
	}
}

// TestEnqueuePending_NilDurationsRoutesSyncedDoubtToWorker pins the nil-seam
// contract: a scheduler built with no Durations store (should not happen in
// production, but must never panic) behaves exactly like "duration always
// unknown" -- a synced entry is routed to the worker, an unsynced one is
// still served.
func TestEnqueuePending_NilDurationsRoutesSyncedDoubtToWorker(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	track := models.Track{ArtistName: "A", TrackName: "NilDurations", TrackLength: 0}
	cached := models.Song{
		Track:     models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title", TrackLength: 500},
		Subtitles: models.Synced{Lines: []models.Lines{guardLine(400, "unjudgeable without a duration source")}},
	}
	if err := repo.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(0), encodeCachedSong(t, cached)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       5,
		FilePath: "/music/nil-durations.flac",
		Track:    track,
	}}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: repo, Queue: work, Priority: 5}

	enqueued, cacheHits, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 || cacheHits != 0 {
		t.Fatalf("enqueued=%d cacheHits=%d; want (1, 0) -- a nil Durations store must route a synced entry to the worker", enqueued, cacheHits)
	}
}
