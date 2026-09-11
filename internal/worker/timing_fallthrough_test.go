package worker

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// The audio file is 100s long; a cue at 400s is timed to another recording.
const fallthroughFileSeconds = 100

func fallthroughSong(lastCue int, text string) models.Song {
	return models.Song{
		Track: models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Subtitles: models.Synced{Lines: []models.Lines{
			guardLine(10, text), guardLine(lastCue, text),
		}},
	}
}

// fallthroughRig is a worker over REAL SQLite: the durable queue (which is also
// the provider recorder, so lane_attempts and provider_lane are the production
// tables) and the real lyrics cache.
type fallthroughRig struct {
	db     *sql.DB
	q      *queue.DBQueue
	cache  *cache.CacheRepo
	writer *capturingWriter
	id     int64
}

func newFallthroughRig(t *testing.T, primary musixmatch.Fetcher, secondary *fakeFetcher) (*fallthroughRig, *Worker) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Outdir:     "/out",
		Filename:   "track.lrc",
		SourcePath: "/library/track.flac",
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rig := &fallthroughRig{db: sqlDB, q: q, cache: cache.New(sqlDB), writer: &capturingWriter{}, id: item.ID}
	w := New(q, rig.cache, primary, rig.writer)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondary))
	w.SetProviderRecorder(q)
	// The file's own tags carry the duration the timing guard judges against.
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	return rig, w
}

func (r *fallthroughRig) row(t *testing.T) (status, lane, timingOutcome string) {
	t.Helper()
	var l, to sql.NullString
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT status, provider_lane, timing_outcome FROM work_queue WHERE id = ?`, r.id).Scan(&status, &l, &to); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return status, l.String, to.String
}

func (r *fallthroughRig) attempts(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(), `SELECT lane, hit FROM lane_attempts WHERE queue_id = ?`, r.id)
	if err != nil {
		t.Fatalf("read lane_attempts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var lane string
		var hit int
		if err := rows.Scan(&lane, &hit); err != nil {
			t.Fatalf("scan lane_attempts: %v", err)
		}
		got[lane] = hit == 1
	}
	return got
}

func (r *fallthroughRig) cached(t *testing.T) (models.Song, bool) {
	t.Helper()
	s, err := r.cache.Lookup(context.Background(), "Synthetic Artist", "Synthetic Title",
		normalize.DurationBucket(fallthroughFileSeconds))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Song{}, false
	}
	if err != nil {
		t.Fatalf("cache lookup: %v", err)
	}
	return decodeSong(s, models.Track{}), true
}

// TestRunOnce_CategoricalFallsThroughToNextLane is the #950 regression end to
// end: the first lane's lyric is timed to a different recording, the second
// lane's fits. The second lane's lyric must be written, the row must settle
// done under the second lane with every consulted lane attributed, and the
// cache must hold the lyric that landed, never the refused one.
func TestRunOnce_CategoricalFallsThroughToNextLane(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(400, "wrong recording")}
	secondary := &fakeFetcher{song: fallthroughSong(90, "right recording")}
	rig, w := newFallthroughRig(t, primary, secondary)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if secondary.calls != 1 {
		t.Fatalf("second lane calls = %d; want 1 (a categorical first result must not end dispatch)", secondary.calls)
	}
	if len(rig.writer.songs) != 1 || rig.writer.songs[0].Subtitles.Lines[0].Text != "right recording" {
		t.Fatalf("written = %+v; want exactly the second lane's lyric", rig.writer.songs)
	}
	status, lane, outcome := rig.row(t)
	if status != "done" || lane != providers.PetitLyrics || outcome != "ok" {
		t.Fatalf("row = (status %q, lane %q, timing %q); want (done, %s, ok)", status, lane, outcome, providers.PetitLyrics)
	}
	att := rig.attempts(t)
	if len(att) != 2 || att[providers.Musixmatch] || !att[providers.PetitLyrics] {
		t.Fatalf("lane_attempts = %v; want musixmatch miss + petitlyrics hit", att)
	}
	got, ok := rig.cached(t)
	if !ok || got.Subtitles.Lines[0].Text != "right recording" {
		t.Fatalf("cache = %+v (present %v); want the lyric that landed", got, ok)
	}
}

// TestRunOnce_ExhaustedCategoricalIsNotCached: when every lane is refused or
// misses, the row settles as before (done + categorical, nothing written) and
// the refused lyric is NOT cached, where it would otherwise satisfy the next
// lookup for this key as though it were a good hit.
func TestRunOnce_ExhaustedCategoricalIsNotCached(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(400, "wrong recording")}
	secondary := &fakeFetcher{err: musixmatch.ErrNotFound}
	rig, w := newFallthroughRig(t, primary, secondary)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if secondary.calls != 1 {
		t.Fatalf("second lane calls = %d; want 1", secondary.calls)
	}
	status, lane, outcome := rig.row(t)
	if status != "done" || lane != providers.Musixmatch || outcome != "categorical" {
		t.Fatalf("row = (status %q, lane %q, timing %q); want (done, musixmatch, categorical)", status, lane, outcome)
	}
	if got, ok := rig.cached(t); ok {
		t.Fatalf("cache holds %+v; a timing-refused lyric must never be cached", got)
	}
}

// TestRunOnce_CachedCategoricalIsNotServed: a build before #950 cached the
// refused lyric ahead of the guard. Such an entry must read as a miss so the
// lanes are dispatched, or a re-queued row would re-hit it forever.
func TestRunOnce_CachedCategoricalIsNotServed(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(90, "right recording")}
	secondary := &fakeFetcher{err: musixmatch.ErrNotFound}
	rig, w := newFallthroughRig(t, primary, secondary)
	stale, err := encodeSong(fallthroughSong(400, "stale refused"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := rig.cache.Store(context.Background(), "Synthetic Artist", "Synthetic Title",
		normalize.DurationBucket(fallthroughFileSeconds), stale); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if primary.calls != 1 {
		t.Fatalf("primary calls = %d; want 1 (a cached categorical must not satisfy the lookup)", primary.calls)
	}
	if len(rig.writer.songs) != 1 || rig.writer.songs[0].Subtitles.Lines[0].Text != "right recording" {
		t.Fatalf("written = %+v; want the freshly fetched lyric", rig.writer.songs)
	}
	got, ok := rig.cached(t)
	if !ok || got.Subtitles.Lines[0].Text != "right recording" {
		t.Fatalf("cache = %+v (present %v); want the stale entry replaced by the landed lyric", got, ok)
	}
}

// TestRunOnce_CategoricalWithThrottledLaneSettles pins that this change cannot
// starve the queue: the only result is refused and the other lane is rate
// limited. The row settles done + categorical exactly as before #950 -- it is
// NOT released or failed, and the pass does not idle on a throttle.
func TestRunOnce_CategoricalWithThrottledLaneSettles(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(400, "wrong recording")}
	secondary := &fakeFetcher{err: musixmatch.ErrRateLimited}
	rig, w := newFallthroughRig(t, primary, secondary)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v; want nil (a refused result settles, it does not idle the pass)", err)
	}
	if secondary.calls != 1 {
		t.Fatalf("second lane calls = %d; want 1", secondary.calls)
	}
	if status, lane, outcome := rig.row(t); status != "done" || lane != providers.Musixmatch || outcome != "categorical" {
		t.Fatalf("row = (status %q, lane %q, timing %q); want (done, musixmatch, categorical)", status, lane, outcome)
	}
	if got, ok := rig.cached(t); ok {
		t.Fatalf("cache holds %+v; a timing-refused lyric must never be cached", got)
	}
}

// TestRunOnce_OverrunSkipsDetector is round-1 finding 2 end to end: the
// provider's MisSynced words are held, so the detector (appended last under
// the default ordering) is never run and cannot replace them with a marker.
func TestRunOnce_OverrunSkipsDetector(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(120, "real words")}
	secondary := &fakeFetcher{err: musixmatch.ErrNotFound}
	rig, w := newFallthroughRig(t, primary, secondary)
	det := &fakeDetector{instrumental: true, version: "9.9.9"}
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 0 {
		t.Fatalf("detector calls = %v; want none while a lyric is held", det.calls)
	}
	if len(rig.writer.songs) != 1 || rig.writer.songs[0].Track.Instrumental == 1 ||
		len(rig.writer.songs[0].Subtitles.Lines) == 0 || rig.writer.songs[0].Subtitles.Lines[0].Text != "real words" {
		t.Fatalf("written = %+v; want the provider's words", rig.writer.songs)
	}
	if status, _, outcome := rig.row(t); status != "done" || outcome != "mis_synced" {
		t.Fatalf("row = (status %q, timing %q); want (done, mis_synced)", status, outcome)
	}
}
