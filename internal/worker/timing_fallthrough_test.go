package worker

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/lyrics"
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

// TestRunOnce_CategoricalWithTransportFailureSettles: a transport failure is an
// ANSWER (the lane was reached, or the request shape is refused, which no wait
// fixes), so the refused result settles on the first pass as before #950.
func TestRunOnce_CategoricalWithTransportFailureSettles(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(400, "wrong recording")}
	secondary := &fakeFetcher{err: errors.New("connection refused")}
	rig, w := newFallthroughRig(t, primary, secondary)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v; want nil", err)
	}
	if status, lane, outcome := rig.row(t); status != "done" || lane != providers.Musixmatch || outcome != "categorical" {
		t.Fatalf("row = (status %q, lane %q, timing %q); want (done, musixmatch, categorical)", status, lane, outcome)
	}
	if got, ok := rig.cached(t); ok {
		t.Fatalf("cache holds %+v; a timing-refused lyric must never be cached", got)
	}
}

// byTitleFetcher answers per track title, so two queued rows can get different
// results from one lane.
type byTitleFetcher map[string]models.Song

func (f byTitleFetcher) FindLyrics(_ context.Context, t models.Track) (models.Song, error) {
	return f[t.TrackName], nil
}

// TestRunOnce_CategoricalWithUntriedLaneWaitsBounded is the #950 part-2 path
// end to end over a real SQLite queue: row 1's only result is refused and the
// other lane fails auth (its breaker then stays open), so that lane never
// answers. Row 1 must be parked via DeferRefused WITHOUT ending the drain pass
// or charging attempts/miss_count, row 2 must be processed while it waits, and
// once the refused_waits budget is spent row 1 settles done + categorical as
// before #950: the refused song handed to the writer (which quarantines it),
// never cached.
func TestRunOnce_CategoricalWithUntriedLaneWaitsBounded(t *testing.T) {
	ctx := context.Background()
	secondary := &fakeFetcher{err: musixmatch.ErrUnauthorized}
	rig, w := newFallthroughRig(t, byTitleFetcher{
		"Synthetic Title": fallthroughSong(400, "wrong recording"),
		"Other Title":     fallthroughSong(90, "right recording"),
	}, secondary)
	row2, err := rig.q.Enqueue(ctx, models.Inputs{
		Track:  models.Track{ArtistName: "Other Artist", TrackName: "Other Title"},
		Outdir: "/out", Filename: "other.lrc", SourcePath: "/library/other.flac",
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue row 2: %v", err)
	}
	type counters struct{ waits, attempts, misses int }
	read := func() counters {
		var c counters
		if err := rig.db.QueryRow(`SELECT refused_waits, attempts, miss_count FROM work_queue WHERE id = ?`, rig.id).
			Scan(&c.waits, &c.attempts, &c.misses); err != nil {
			t.Fatalf("read counters: %v", err)
		}
		return c
	}
	rewind := func() {
		if _, err := rig.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, rig.id); err != nil {
			t.Fatalf("rewind: %v", err)
		}
	}
	w.consecutiveFailures = 2 // a stale failure streak; a parked refusal is not a failure
	logs := captureLogs(t)

	// Pass 1: row 1 is parked and the pass does NOT idle.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("pass 1 = %v; want nil (a partial outage must not end the drain pass)", err)
	}
	if status, _, outcome := rig.row(t); status != queue.StatusDeferred || outcome != "" {
		t.Fatalf("row 1 after pass 1 = (%q, timing %q); want (deferred, unset)", status, outcome)
	}
	if c := read(); c != (counters{waits: 1}) {
		t.Fatalf("row 1 counters = %+v; want refused_waits 1 and attempts/miss_count untouched", c)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a lane answered)", w.consecutiveFailures)
	}
	if len(rig.writer.songs) != 0 {
		t.Fatalf("written = %+v; want nothing while waiting", rig.writer.songs)
	}
	// Pass 2: row 1 waits out its DeferRefused window, so row 2 is claimed.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	var s2 string
	if err := rig.db.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, row2.ID).Scan(&s2); err != nil || s2 != queue.StatusDone {
		t.Fatalf("row 2 status = %q (%v); want done (row 1 must not starve it)", s2, err)
	}
	// Row 1 is re-dequeued and re-parked until the budget is spent.
	for want := 2; want <= maxRefusedWaits; want++ {
		rewind()
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("wait %d: %v", want, err)
		}
		if status, _, _ := rig.row(t); status != queue.StatusDeferred || read().waits != want {
			t.Fatalf("wait %d: row 1 = (%q, refused_waits %d); want (deferred, %d)", want, status, read().waits, want)
		}
	}
	rewind()
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("settling pass: %v", err)
	}
	// I1: a settle with a lane still untried is findable at Warn, naming the lane.
	if rec := findLog(*logs, slog.LevelWarn, "lane still untried"); rec == nil ||
		rec.attrs["untried_lane"].String() != providers.PetitLyrics || rec.attrs["id"].Int64() != rig.id {
		t.Fatalf("settle log = %+v; want a Warn naming untried_lane %q and the row id", rec, providers.PetitLyrics)
	}
	if status, lane, outcome := rig.row(t); status != queue.StatusDone || lane != providers.Musixmatch || outcome != "categorical" {
		t.Fatalf("row 1 = (%q, %q, %q); want (done, musixmatch, categorical) once the budget is spent", status, lane, outcome)
	}
	if c := read(); c.waits != 0 || c.attempts != 0 || c.misses != 0 {
		t.Fatalf("row 1 counters after settle = %+v; want all zero", c)
	}
	if len(rig.writer.songs) != 2 || rig.writer.songs[0].Subtitles.Lines[0].Text != "right recording" {
		t.Fatalf("written = %+v; want row 2's lyric, then row 1's refused one", rig.writer.songs)
	}
	if last := rig.writer.songs[1]; !lyrics.RefusedByTimingGuard(last, last.AudioDurationSeconds) {
		t.Fatalf("row 1 handed %+v; want a song the writer's guard quarantines (writes nothing)", last)
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

// refusedUntriedFakeWorker is a worker over the fake queue whose one row's only
// result is timing-refused while the fallback lane fails auth (never answers),
// so the dispatch returns ErrTimingRefusedUntried.
func refusedUntriedFakeWorker(q *fakeQueue) *Worker {
	q.items = []queue.WorkItem{{ID: 91, Inputs: models.Inputs{
		Track:  models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Outdir: "/out", Filename: "track.lrc", SourcePath: "/library/track.flac",
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{song: fallthroughSong(400, "wrong recording")}, &capturingWriter{})
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{err: musixmatch.ErrUnauthorized}))
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	return w
}

// TestRunOnce_DeferRefusedErrors (#950 review M2): a row that is no longer
// processing is left alone (no fail), while any other queue error takes the
// ordinary fail path.
func TestRunOnce_DeferRefusedErrors(t *testing.T) {
	t.Run("no longer processing", func(t *testing.T) {
		q := &fakeQueue{deferRefusedErr: sql.ErrNoRows}
		w := refusedUntriedFakeWorker(q)
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce = %v; want nil (the row moved on)", err)
		}
		if len(q.failed) != 0 || len(q.completed) != 0 {
			t.Fatalf("failed %v completed %v; want the row left alone", q.failed, q.completed)
		}
	})
	t.Run("queue error", func(t *testing.T) {
		q := &fakeQueue{deferRefusedErr: errors.New("disk full")}
		w := refusedUntriedFakeWorker(q)
		_ = w.RunOnce(context.Background())
		if len(q.failed) != 1 || q.failed[0] != 91 {
			t.Fatalf("failed = %v; want [91] (a queue error takes the fail path)", q.failed)
		}
	})
}

// TestRunOnce_RefusedUntriedStampsDetectorTelemetry (#950 review M3): the
// detector's not-instrumental telemetry is stamped BEFORE the row is parked,
// so the re-dispatch after the wait reuses it instead of re-running YAMNet.
func TestRunOnce_RefusedUntriedStampsDetectorTelemetry(t *testing.T) {
	q := &fakeQueue{}
	w := refusedUntriedFakeWorker(q)
	w.EnableAudioDetector(&fakeStoredDecider{version: "v1", detectRes: detector.Result{
		Instrumental: false, Version: "v1", Confidence: 0.4, VocalConfidence: 0.7, WinningVocalClass: "Singing", Reusable: true,
	}})
	w.SetInstrumentalDetectionDefault(true)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v", err)
	}
	if len(q.deferred) != 1 {
		t.Fatalf("deferred = %v; want the row parked", q.deferred)
	}
	if len(q.instrumentalStamps) != 1 || q.instrumentalStamps[0].Tel.DetectorVersion != "v1" {
		t.Fatalf("instrumentalStamps = %+v; want the v1 telemetry stamped before the park", q.instrumentalStamps)
	}
}
