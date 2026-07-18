package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/backoff"
	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/verification"
)

// logRecord captures one emitted log line's level, message, and attrs for
// assertions.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

// captureHandler is a minimal slog.Handler that records every line's level,
// message, and attributes. Assertions match on the stable message string; the
// attrs are recorded so tests can verify structured fields (e.g. backoff,
// next_retry) without depending on a particular text format.
type captureHandler struct{ recs *[]logRecord }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	*h.recs = append(*h.recs, logRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// findLog returns the first captured record matching level and containing
// msgSub, or nil if none matched.
func findLog(recs []logRecord, level slog.Level, msgSub string) *logRecord {
	for i := range recs {
		if recs[i].level == level && strings.Contains(recs[i].msg, msgSub) {
			return &recs[i]
		}
	}
	return nil
}

func attrDuration(r *logRecord, key string) time.Duration {
	return r.attrs[key].Duration()
}

func attrTime(r *logRecord, key string) time.Time {
	return r.attrs[key].Time()
}

// captureLogs installs a recording slog handler as the default for the duration
// of the test and restores the previous default on cleanup. The repo has no
// other slog-level capture harness, so worker tests that assert Info-vs-Warn
// routing rely on this. Returns a pointer to the slice so assertions read the
// final state after the code under test has run.
func captureLogs(t *testing.T) *[]logRecord {
	t.Helper()
	prev := slog.Default()
	var recs []logRecord
	slog.SetDefault(slog.New(&captureHandler{recs: &recs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &recs
}

// hasLog reports whether any captured record matches level and contains msgSub.
func hasLog(recs []logRecord, level slog.Level, msgSub string) bool {
	for _, r := range recs {
		if r.level == level && strings.Contains(r.msg, msgSub) {
			return true
		}
	}
	return false
}

// fakeQueue models DBQueue's status transitions for tests. Dequeue moves an
// item out of the pending pool and into processing; Complete/Fail/Release
// remove it from processing. Release additionally records the ID so tests
// can assert that an item was returned to the pending pool without a failure
// being recorded against it.
type fakeQueue struct {
	items              []queue.WorkItem
	processing         []queue.WorkItem
	completed          []int64
	failed             []int64
	released           []int64
	deferred           []int64
	retired            []int64
	outcomeTypes       map[int64]string
	failCauses         []error
	deferCauses        []error
	deferDurations     []time.Duration
	completeErr        error
	failErr            error
	deferErr           error
	releaseErr         error
	retireErr          error
	setProviderLaneErr error
	instrumentalStamps []instrumentalStamp
	instrumentalErr    error
}

func (q *fakeQueue) Dequeue(_ context.Context) (queue.WorkItem, error) {
	if len(q.items) == 0 {
		return queue.WorkItem{}, sql.ErrNoRows
	}
	item := q.items[0]
	q.items = q.items[1:]
	q.processing = append(q.processing, item)
	return item, nil
}

func (q *fakeQueue) Complete(_ context.Context, id int64) error {
	if q.completeErr != nil {
		return q.completeErr
	}
	q.removeFromProcessing(id)
	q.completed = append(q.completed, id)
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id int64, cause error) (queue.WorkItem, error) {
	if q.failErr != nil {
		return queue.WorkItem{}, q.failErr
	}
	q.removeFromProcessing(id)
	q.failed = append(q.failed, id)
	q.failCauses = append(q.failCauses, cause)
	return queue.WorkItem{ID: id, Status: queue.StatusFailed}, nil
}

func (q *fakeQueue) Defer(_ context.Context, id int64, retryAfter time.Duration, cause error) (queue.WorkItem, error) {
	if q.deferErr != nil {
		return queue.WorkItem{}, q.deferErr
	}
	q.removeFromProcessing(id)
	q.deferred = append(q.deferred, id)
	q.deferCauses = append(q.deferCauses, cause)
	q.deferDurations = append(q.deferDurations, retryAfter)
	return queue.WorkItem{ID: id, Status: queue.StatusDeferred}, nil
}

func (q *fakeQueue) Release(_ context.Context, id int64) error {
	if q.releaseErr != nil {
		return q.releaseErr
	}
	q.removeFromProcessing(id)
	q.released = append(q.released, id)
	return nil
}

func (q *fakeQueue) RetireMiss(_ context.Context, id int64) (queue.WorkItem, error) {
	if q.retireErr != nil {
		return queue.WorkItem{}, q.retireErr
	}
	q.removeFromProcessing(id)
	q.retired = append(q.retired, id)
	return queue.WorkItem{ID: id, Status: queue.StatusDone}, nil
}

// instrumentalStamp captures one SetInstrumentalResult call so tests can
// assert both the result flag and the telemetry that round-tripped through
// the stamp path.
type instrumentalStamp struct {
	ID     int64
	Result int
	Tel    queue.InstrumentalTelemetry
}

func (q *fakeQueue) SetInstrumentalResult(_ context.Context, id int64, result int, tel queue.InstrumentalTelemetry) error {
	if q.instrumentalErr != nil {
		return q.instrumentalErr
	}
	q.instrumentalStamps = append(q.instrumentalStamps, instrumentalStamp{ID: id, Result: result, Tel: tel})
	return nil
}

func (q *fakeQueue) SetOutcomeType(_ context.Context, id int64, outcomeType string) error {
	if q.outcomeTypes == nil {
		q.outcomeTypes = make(map[int64]string)
	}
	q.outcomeTypes[id] = outcomeType
	return nil
}

func (q *fakeQueue) SetProviderLane(_ context.Context, _ int64, _ string) error {
	if q.setProviderLaneErr != nil {
		return q.setProviderLaneErr
	}
	return nil
}

func (q *fakeQueue) removeFromProcessing(id int64) {
	for i, item := range q.processing {
		if item.ID == id {
			q.processing = append(q.processing[:i], q.processing[i+1:]...)
			return
		}
	}
}

type cacheStore struct {
	artist string
	title  string
	lyrics string
}

type fakeCache struct {
	hit    string
	err    error
	stores []cacheStore
}

func (c *fakeCache) Lookup(_ context.Context, _ string, _ string, _ int) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.hit == "" {
		return "", sql.ErrNoRows
	}
	return c.hit, nil
}

func (c *fakeCache) Store(_ context.Context, artist, title string, _ int, lyrics string) error {
	c.stores = append(c.stores, cacheStore{artist: artist, title: title, lyrics: lyrics})
	return nil
}

type fakeFetcher struct {
	song  models.Song
	err   error
	calls int
}

func (f *fakeFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	f.calls++
	if f.err != nil {
		return models.Song{}, f.err
	}
	return f.song, nil
}

type fakeProviderRecorder struct {
	hits        []string
	misses      []string
	attempts    map[int64][]models.LaneAttempt
	hitErr      error
	missErr     error
	attemptsErr error
}

func (r *fakeProviderRecorder) RecordProviderHit(_ context.Context, lane string) error {
	r.hits = append(r.hits, lane)
	return r.hitErr
}

func (r *fakeProviderRecorder) RecordProviderMiss(_ context.Context, lane string) error {
	r.misses = append(r.misses, lane)
	return r.missErr
}

func (r *fakeProviderRecorder) RecordLaneAttempts(_ context.Context, queueID int64, attempts []models.LaneAttempt) error {
	if r.attempts == nil {
		r.attempts = make(map[int64][]models.LaneAttempt)
	}
	r.attempts[queueID] = append(r.attempts[queueID], attempts...)
	return r.attemptsErr
}

type fakeWriter struct {
	writes []models.OutputPath
	songs  []models.Song
	err    error
}

func (w *fakeWriter) WriteLRC(song models.Song, filename string, outdir string) error {
	w.writes = append(w.writes, models.OutputPath{Outdir: outdir, Filename: filename})
	w.songs = append(w.songs, song)
	return w.err
}

type fakeVerifier struct {
	results []verificationResult
	calls   []verifierCall
}

type verifierCall struct {
	path string
	song models.Song
}

type verificationResult struct {
	accepted bool
	err      error
}

func (v *fakeVerifier) Verify(_ context.Context, path string, song models.Song) (verification.Result, error) {
	res := verificationResult{accepted: true}
	if len(v.calls) < len(v.results) {
		res = v.results[len(v.calls)]
	}
	v.calls = append(v.calls, verifierCall{path: path, song: song})
	if res.err != nil {
		return verification.Result{}, res.err
	}
	return verification.Result{Accepted: res.accepted, Similarity: 1}, nil
}

// fakeGuard is a test ScriptGuard. enabled toggles Enabled(); accept toggles
// Accept's verdict. calls records each Accept invocation's song so tests can
// assert the guard saw the fetched lyrics.
type fakeGuard struct {
	enabled bool
	accept  bool
	reason  string
	calls   []models.Song
}

func (g *fakeGuard) Enabled() bool { return g.enabled }

// selectiveGuard rejects only a song whose lyric body exactly matches
// rejectBody, accepting everything else (including a textless instrumental
// result) - unlike fakeGuard, which rejects unconditionally regardless of
// content. This models how a real script guard behaves: it screens lyric
// text, so it has nothing to reject on a detector-sourced instrumental.
type selectiveGuard struct {
	rejectBody string
	reason     string
	calls      []models.Song
}

func (g *selectiveGuard) Enabled() bool { return true }

func (g *selectiveGuard) Accept(song models.Song) (bool, string) {
	g.calls = append(g.calls, song)
	if song.Lyrics.LyricsBody == g.rejectBody {
		return false, g.reason
	}
	return true, ""
}

func (g *fakeGuard) Accept(song models.Song) (bool, string) {
	g.calls = append(g.calls, song)
	if g.accept {
		return true, ""
	}
	return false, g.reason
}

func TestRunOnceGuardRejectsTerminallyWithoutCacheOrWrite(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 80,
		Inputs: models.Inputs{
			Track:    track,
			Outdir:   "out",
			Filename: "artist-title.lrc",
		},
	}}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "wrong-language lyrics"},
	}}
	writer := &fakeWriter{}
	guard := &fakeGuard{enabled: true, accept: false, reason: "foreign-script share 0.90 exceeds 0.20"}

	w := New(q, cache, fetcher, writer)
	w.EnableGuard(guard)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(guard.calls) != 1 {
		t.Fatalf("guard calls = %d; want 1", len(guard.calls))
	}
	if len(writer.writes) != 0 {
		t.Fatalf("writes = %+v; want none (guard rejection must not write)", writer.writes)
	}
	if len(cache.stores) != 0 {
		t.Fatalf("cache stores = %d; want none (guard rejection must not cache)", len(cache.stores))
	}
	// Guard rejection is terminal/policy: Complete (not Fail, not Defer).
	if len(q.completed) != 1 || q.completed[0] != 80 {
		t.Fatalf("completed = %v; want [80] (terminal completion)", q.completed)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (guard rejection is not a retriable failure)", q.failed)
	}
	if len(q.deferred) != 0 {
		t.Fatalf("deferred = %v; want none (re-fetching yields the same wrong-language result)", q.deferred)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (guard rejection must not trip backoff)", w.consecutiveFailures)
	}
}

func TestRunOnceDisabledGuardProceedsNormally(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	for name, guard := range map[string]*fakeGuard{
		"nil guard":      nil,
		"disabled guard": {enabled: false, accept: false},
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{items: []queue.WorkItem{{
				ID: 81,
				Inputs: models.Inputs{
					Track:    track,
					Outdir:   "out",
					Filename: "artist-title.lrc",
				},
			}}}
			cache := &fakeCache{}
			fetcher := &fakeFetcher{song: models.Song{
				Track:  track,
				Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
			}}
			writer := &fakeWriter{}

			w := New(q, cache, fetcher, writer)
			if guard != nil {
				w.EnableGuard(guard)
			}

			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(writer.writes) != 1 {
				t.Fatalf("writes = %+v; want one (disabled guard must not block)", writer.writes)
			}
			if len(cache.stores) != 1 {
				t.Fatalf("cache stores = %d; want 1 (disabled guard must not block caching)", len(cache.stores))
			}
			if len(q.completed) != 1 || q.completed[0] != 81 {
				t.Fatalf("completed = %v; want [81]", q.completed)
			}
			if len(q.failed) != 0 {
				t.Fatalf("failed = %v; want none", q.failed)
			}
		})
	}
}

func TestRunOnceCacheHitAvoidsFetcherAndCompletes(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	song := models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "cached lyrics"},
	}
	cached, err := encodeSong(song)
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 1,
		Inputs: models.Inputs{
			Track:    track,
			Outdir:   "out",
			Filename: "artist-title.lrc",
		},
	}}}
	cache := &fakeCache{hit: cached}
	fetcher := &fakeFetcher{}
	writer := &fakeWriter{}

	w := New(q, cache, fetcher, writer)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if fetcher.calls != 0 {
		t.Fatalf("fetcher calls = %d; want 0", fetcher.calls)
	}
	if len(writer.writes) != 1 || writer.writes[0].Outdir != "out" || writer.writes[0].Filename != "artist-title.lrc" {
		t.Fatalf("writes = %+v; want one out/artist-title.lrc write", writer.writes)
	}
	if len(q.completed) != 1 || q.completed[0] != 1 {
		t.Fatalf("completed = %v; want [1]", q.completed)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none", q.failed)
	}
}

func TestRunOnceFetchesCachesWritesAllOutputsAndCompletes(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 2,
		Inputs: models.Inputs{
			Track: track,
			OutputPaths: []models.OutputPath{
				{Outdir: "out-a", Filename: "a.lrc"},
				{Outdir: "out-b", Filename: "b.lrc"},
			},
		},
	}}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	writer := &fakeWriter{}

	w := New(q, cache, fetcher, writer)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d; want 1", fetcher.calls)
	}
	if len(cache.stores) != 1 {
		t.Fatalf("cache stores = %d; want 1", len(cache.stores))
	}
	if cache.stores[0].artist != "Artist" || cache.stores[0].title != "Title" {
		t.Fatalf("cache store key = %+v; want Artist/Title", cache.stores[0])
	}
	if len(writer.writes) != 2 {
		t.Fatalf("writes = %d; want 2", len(writer.writes))
	}
	if writer.writes[0].Outdir != "out-a" || writer.writes[1].Outdir != "out-b" {
		t.Fatalf("writes = %+v; want both output paths", writer.writes)
	}
	if len(q.completed) != 1 || q.completed[0] != 2 {
		t.Fatalf("completed = %v; want [2]", q.completed)
	}
}

func TestRunOnceVerifiesLowConfidenceScannedFetch(t *testing.T) {
	track := models.Track{ArtistName: "Requested Artist", TrackName: "Requested Title"}
	fetched := models.Track{ArtistName: "Different Artist", TrackName: "Different Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 20,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "requested-title.lrc",
			SourcePath: "/music/requested-title.flac",
		},
	}}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  fetched,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	verifier := &fakeVerifier{}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.EnableVerification(verifier, 0.85)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(verifier.calls) != 1 {
		t.Fatalf("verifier calls = %d; want 1", len(verifier.calls))
	}
	if verifier.calls[0].path != "/music/requested-title.flac" {
		t.Fatalf("verifier path = %q; want source path", verifier.calls[0].path)
	}
	if verifier.calls[0].song.Track.ArtistName != fetched.ArtistName || verifier.calls[0].song.Track.TrackName != fetched.TrackName {
		t.Fatalf("verifier song track = %+v; want fetched track %+v", verifier.calls[0].song.Track, fetched)
	}
	if len(q.completed) != 1 || q.completed[0] != 20 {
		t.Fatalf("completed = %v; want [20]", q.completed)
	}
}

func TestRunOnceSkipsVerificationForHighConfidenceMatch(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 21,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "artist-title.lrc",
			SourcePath: "/music/artist-title.flac",
		},
	}}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	verifier := &fakeVerifier{}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.EnableVerification(verifier, 0.85)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(verifier.calls) != 0 {
		t.Fatalf("verifier calls = %d; want 0", len(verifier.calls))
	}
}

func TestRunOnceRejectedVerificationMarksQueueFailed(t *testing.T) {
	track := models.Track{ArtistName: "Requested Artist", TrackName: "Requested Title"}
	fetched := models.Track{ArtistName: "Different Artist", TrackName: "Different Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 22,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "requested-title.lrc",
			SourcePath: "/music/requested-title.flac",
		},
	}}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  fetched,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	verifier := &fakeVerifier{results: []verificationResult{{accepted: false}}}
	cache := &fakeCache{}
	w := New(q, cache, fetcher, &fakeWriter{})
	w.EnableVerification(verifier, 0.85)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(verifier.calls) != 1 {
		t.Fatalf("verifier calls = %d; want 1", len(verifier.calls))
	}
	if verifier.calls[0].path != "/music/requested-title.flac" {
		t.Fatalf("verifier path = %q; want source path", verifier.calls[0].path)
	}
	if len(q.failed) != 1 || q.failed[0] != 22 {
		t.Fatalf("failed = %v; want [22]", q.failed)
	}
	if len(cache.stores) != 0 {
		t.Fatalf("cache stores = %d; want none for rejected verification", len(cache.stores))
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
	// The provider fetch succeeded; only STT verification rejected the result. The
	// circuit must still record the successful round-trip, so a later bare 401 is
	// classified as throttling rather than a token that never worked.
	if !w.circuit.EverSucceeded() {
		t.Fatal("circuit EverSucceeded = false; a successful fetch must record success even when verification fails")
	}
}

func TestRunOnceStoresCacheWithRequestedTrackKeys(t *testing.T) {
	track := models.Track{ArtistName: "Requested Artist", TrackName: "Requested Title", AlbumName: "Requested Album"}
	fetched := models.Track{ArtistName: "Canonical Artist", TrackName: "Canonical Title", AlbumName: "Canonical Album"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 6,
		Inputs: models.Inputs{
			Track:    track,
			Outdir:   "out",
			Filename: "requested-title.lrc",
		},
	}}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  fetched,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}

	w := New(q, cache, fetcher, &fakeWriter{})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(cache.stores) != 1 {
		t.Fatalf("cache stores = %d; want 1", len(cache.stores))
	}
	store := cache.stores[0]
	if store.artist != track.ArtistName || store.title != track.TrackName {
		t.Fatalf("cache store key = %+v; want requested track %+v", store, track)
	}
}

func TestRunOnceFailureMarksQueueFailed(t *testing.T) {
	wantErr := errors.New("fetch failed")
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     3,
		Inputs: models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: wantErr}, &fakeWriter{})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(q.failed) != 1 || q.failed[0] != 3 {
		t.Fatalf("failed = %v; want [3]", q.failed)
	}
	if !errors.Is(q.failCauses[0], wantErr) {
		t.Fatalf("fail cause = %v; want %v", q.failCauses[0], wantErr)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
}

func TestRunOnceBenignMissRequeuesDeferredWithoutCounter(t *testing.T) {
	for name, sentinel := range map[string]error{
		"not found": musixmatch.ErrNotFound,
		"no lyrics": musixmatch.ErrNoLyrics,
	} {
		t.Run(name, func(t *testing.T) {
			track := models.Track{ArtistName: "Artist", TrackName: "Title"}
			q := &fakeQueue{items: []queue.WorkItem{{
				ID:     42,
				Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
			}}}
			fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", sentinel)}
			w := New(q, &fakeCache{}, fetcher, &fakeWriter{})

			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			// A no-result requeues via Defer (fixed cooldown) so it is
			// re-attempted later -- it is NOT terminal -- but it must NOT bump
			// the consecutive-failure counter and must NOT use Fail's geometric
			// backoff.
			if len(q.deferred) != 1 || q.deferred[0] != 42 {
				t.Fatalf("deferred (requeued) = %v; want [42]", q.deferred)
			}
			if len(q.failed) != 0 {
				t.Fatalf("failed = %v; want none (a benign miss defers, not fails)", q.failed)
			}
			if len(q.completed) != 0 {
				t.Fatalf("completed = %v; want none (no lyrics written)", q.completed)
			}
			if w.consecutiveFailures != 0 {
				t.Fatalf("consecutiveFailures = %d; want 0 (a no-result must not trip backoff)", w.consecutiveFailures)
			}
			if len(q.deferCauses) != 1 || !errors.Is(q.deferCauses[0], sentinel) {
				t.Fatalf("requeue cause = %v; want errors.Is(_, %v)", q.deferCauses, sentinel)
			}
			// The first miss (miss_count=0+1=1) uses backoff.DefaultMissBase (168h / 7d).
			if len(q.deferDurations) != 1 || q.deferDurations[0] != backoff.DefaultMissBase {
				t.Fatalf("defer cooldown = %v; want first-miss base %v", q.deferDurations, backoff.DefaultMissBase)
			}
		})
	}
}

func TestRunOnceBenignMissSurfacesRequeueError(t *testing.T) {
	deferErr := errors.New("requeue write failed")
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:     55,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}},
		}},
		deferErr: deferErr,
	}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})

	err := w.RunOnce(context.Background())
	if !errors.Is(err, deferErr) {
		t.Fatalf("RunOnce error = %v; want wrapped requeue failure", err)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a no-result never trips backoff, even when the requeue errors)", w.consecutiveFailures)
	}
}

// TestRunOnceBenignMissDeferNoRowsIsBenign covers requeueDeferred treating a
// sql.ErrNoRows from queue.Defer as a benign "item moved on" (the row is no
// longer 'processing' because it was canceled or re-dequeued out from under us).
// RunOnce must NOT propagate an error and must NOT trip the failure counter.
func TestRunOnceBenignMissDeferNoRowsIsBenign(t *testing.T) {
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:     77,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}},
		}},
		deferErr: sql.ErrNoRows,
	}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v; want nil (a Defer no-rows is benign, the item moved on)", err)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (a Defer no-rows must not be recorded as a failure)", q.failed)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a benign Defer no-rows must not trip backoff)", w.consecutiveFailures)
	}
}

func TestRunBenignMissesDoNotBackOff(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 500, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		{ID: 501, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "b.lrc"}},
		{ID: 502, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "c.lrc"}},
	}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.baseBackoff = time.Second
	w.maxBackoff = time.Hour
	var sleeps []time.Duration
	w.sleep = func(_ context.Context, d time.Duration) {
		sleeps = append(sleeps, d)
	}

	if err := w.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleeps = %v; want none (no-results must not back off the worker)", sleeps)
	}
	// All three are requeued via Defer (fixed cooldown), not failed/terminal.
	if len(q.deferred) != 3 {
		t.Fatalf("deferred (requeued) = %v; want all 3 items", q.deferred)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (benign misses defer, not fail)", q.failed)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
}

func TestRunOnceCompleteFailureMarksQueueFailed(t *testing.T) {
	completeErr := errors.New("complete failed")
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID: 7,
			Inputs: models.Inputs{
				Track:    track,
				Outdir:   "out",
				Filename: "artist-title.lrc",
			},
		}},
		completeErr: completeErr,
	}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})

	err := w.RunOnce(context.Background())
	if !errors.Is(err, completeErr) {
		t.Fatalf("RunOnce error = %v; want complete failure", err)
	}
	if len(q.failed) != 1 || q.failed[0] != 7 {
		t.Fatalf("failed = %v; want [7]", q.failed)
	}
	if len(q.failCauses) != 1 || !errors.Is(q.failCauses[0], completeErr) {
		t.Fatalf("fail causes = %v; want complete failure", q.failCauses)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
}

func TestRunReturnsNilWhenQueueEmpty(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v; want nil", err)
	}
}

func TestRunReturnsCompleteErrNoRows(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID: 8,
			Inputs: models.Inputs{
				Track:    track,
				Outdir:   "out",
				Filename: "artist-title.lrc",
			},
		}},
		completeErr: sql.ErrNoRows,
	}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})

	err := w.Run(context.Background())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Run error = %v; want sql.ErrNoRows", err)
	}
}

func TestRunProcessesReadyItemsUntilQueueEmpty(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{
			ID:     4,
			Inputs: models.Inputs{Track: track, Outdir: "out-a", Filename: "a.lrc"},
		},
		{
			ID:     5,
			Inputs: models.Inputs{Track: track, Outdir: "out-b", Filename: "b.lrc"},
		},
	}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	writer := &fakeWriter{}

	w := New(q, cache, fetcher, writer)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(q.completed) != 2 || q.completed[0] != 4 || q.completed[1] != 5 {
		t.Fatalf("completed = %v; want [4 5]", q.completed)
	}
	if len(writer.writes) != 2 {
		t.Fatalf("writes = %d; want 2", len(writer.writes))
	}
	if writer.writes[0].Outdir != "out-a" || writer.writes[0].Filename != "a.lrc" {
		t.Fatalf("writes[0] = %+v; want out-a/a.lrc", writer.writes[0])
	}
	if writer.writes[1].Outdir != "out-b" || writer.writes[1].Filename != "b.lrc" {
		t.Fatalf("writes[1] = %+v; want out-b/b.lrc", writer.writes[1])
	}
	if len(q.items) != 0 {
		t.Fatalf("remaining items = %+v; want none", q.items)
	}
}

func TestRunPacedPausesAfterEachProcessedItem(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{
			ID:     4,
			Inputs: models.Inputs{Track: track, Outdir: "out-a", Filename: "a.lrc"},
		},
		{
			ID:     5,
			Inputs: models.Inputs{Track: track, Outdir: "out-b", Filename: "b.lrc"},
		},
	}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	var completedAtPause []int

	err := w.run(context.Background(), func(context.Context) error {
		completedAtPause = append(completedAtPause, len(q.completed))
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(q.completed) != 2 {
		t.Fatalf("completed = %v; want two completed items", q.completed)
	}
	if len(completedAtPause) < 2 || completedAtPause[0] != 1 || completedAtPause[1] != 2 {
		t.Fatalf("completed at pause = %v; want pauses after each processed item", completedAtPause)
	}
}

func TestRunPacedSkipsPauseWhenNoProviderWasContacted(t *testing.T) {
	// The pause exists to protect providers from being called too often. An item
	// settled entirely by local lanes (the detector, and later any other local
	// lane) issues no provider request, so it must not consume that budget --
	// otherwise a library of instrumentals drains at the provider rate despite
	// never touching a provider (#534).
	q := &fakeQueue{items: []queue.WorkItem{
		detectItem(310, boolPtr(true)),
		detectItem(311, boolPtr(true)),
	}}
	// The detector settles both items from local audio analysis. The fetcher would
	// error if reached, so a pause here would mean the loop paced work no provider
	// ever saw.
	det := &fakeDetector{instrumental: true, version: "1.0.0"}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNoLyrics}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetDetectorOrdering("front")

	pauses := 0
	if err := w.run(context.Background(), func(context.Context) error {
		pauses++
		return nil
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(q.completed) != 2 {
		t.Fatalf("completed = %v; want both items settled by the detector", q.completed)
	}
	if pauses != 0 {
		t.Fatalf("pauses = %d; want 0 -- no provider was contacted, so the provider pacing budget must not be spent", pauses)
	}
}

func TestRunPacedStillPausesWhenAProviderWasContacted(t *testing.T) {
	// The guard must be narrow. A mixed attempt set (a local lane plus a provider
	// lane, as ModeParallel produces) DID issue a provider request, so the pause
	// still applies. Keying off the winning lane instead of the attempted set
	// would wrongly skip it here.
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 12, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
	}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "lyrics"},
		LaneAttempts: []models.LaneAttempt{
			{Lane: "detector", Hit: true, Local: true},
			{Lane: "musixmatch", Hit: false},
		},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})

	pauses := 0
	if err := w.run(context.Background(), func(context.Context) error {
		pauses++
		return nil
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if pauses != 1 {
		t.Fatalf("pauses = %d; want 1 -- a provider lane was attempted, so the pause must still apply", pauses)
	}
}

func TestRunPacedPausesWhenLaneAttributionIsAbsent(t *testing.T) {
	// Fail safe: an empty attempt set means "unknown", not "provider-free". A
	// fetcher that reports no lane attribution at all must keep the existing
	// pacing behavior rather than silently losing the throttle.
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 13, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
	}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "lyrics"},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})

	pauses := 0
	if err := w.run(context.Background(), func(context.Context) error {
		pauses++
		return nil
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if pauses != 1 {
		t.Fatalf("pauses = %d; want 1 -- absent lane attribution must fail safe to pausing", pauses)
	}
}

func TestRunBacksOffGeometricallyAfterConsecutiveFailures(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 100, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		{ID: 101, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "b.lrc"}},
		{ID: 102, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "c.lrc"}},
	}}
	fetcher := &fakeFetcher{err: errors.New("rate limited")}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.baseBackoff = time.Second
	w.maxBackoff = time.Hour
	var sleeps []time.Duration
	w.sleep = func(_ context.Context, d time.Duration) {
		sleeps = append(sleeps, d)
	}

	if err := w.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(q.failed) != 3 {
		t.Fatalf("failed = %v; want all 3 items marked failed", q.failed)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleep count = %d, want %d: %v", len(sleeps), len(want), sleeps)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleep[%d] = %s, want %s", i, sleeps[i], want[i])
		}
	}
}

func TestRunResetsBackoffAfterSuccess(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	cached, err := encodeSong(models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "cached"}})
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 200, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		{ID: 201, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "b.lrc"}},
		{ID: 202, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "c.lrc"}},
	}}
	cache := &fakeCacheToggle{hits: []bool{false, true, false}, payload: cached}
	fetcher := &fakeFetcher{err: errors.New("rate limited")}
	w := New(q, cache, fetcher, &fakeWriter{})
	w.baseBackoff = time.Second
	w.maxBackoff = time.Hour
	var sleeps []time.Duration
	var pauses int
	w.sleep = func(_ context.Context, d time.Duration) {
		sleeps = append(sleeps, d)
	}

	if err := w.run(context.Background(), func(context.Context) error {
		pauses++
		return nil
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if pauses != 1 {
		t.Fatalf("pauses = %d; want 1 (after the cache-hit success)", pauses)
	}
	want := []time.Duration{time.Second, time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleep count = %d, want %d: %v", len(sleeps), len(want), sleeps)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleep[%d] = %s, want %s (counter must reset on cache-hit success)", i, sleeps[i], want[i])
		}
	}
}

func TestRunBackoffFiresBeforeRunOnceAfterErrorReturn(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{
		items: []queue.WorkItem{
			{ID: 300, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		},
	}
	w := New(q, &fakeCache{}, &fakeFetcher{err: errors.New("rate limited")}, &fakeWriter{})
	w.baseBackoff = time.Second
	w.maxBackoff = time.Hour
	w.consecutiveFailures = 3

	var sleeps []time.Duration
	w.sleep = func(_ context.Context, d time.Duration) {
		sleeps = append(sleeps, d)
	}

	if err := w.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sleeps) == 0 || sleeps[0] != 4*time.Second {
		t.Fatalf("first sleep = %v; want 4s before any dequeue (carry-over backoff)", sleeps)
	}
}

func TestRunCounterIncrementsOnWriteFailure(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 400, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		{ID: 401, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "b.lrc"}},
	}}
	fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	writer := &fakeWriter{err: errors.New("disk full")}
	w := New(q, &fakeCache{}, fetcher, writer)
	w.baseBackoff = time.Second
	w.maxBackoff = time.Hour

	var sleeps []time.Duration
	w.sleep = func(_ context.Context, d time.Duration) {
		sleeps = append(sleeps, d)
	}

	if err := w.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v; want %v (write failures must also trip backoff)", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps[%d] = %s; want %s", i, sleeps[i], want[i])
		}
	}
}

func TestRunOnceOpensCircuitOnRateLimitedAndDoesNotMarkFailed(t *testing.T) {
	for name, sentinel := range map[string]error{
		"rate limited": musixmatch.ErrRateLimited,
		"unauthorized": musixmatch.ErrUnauthorized,
	} {
		t.Run(name, func(t *testing.T) {
			track := models.Track{ArtistName: "Artist", TrackName: "Title"}
			q := &fakeQueue{items: []queue.WorkItem{
				{ID: 900, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
				{ID: 901, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "b.lrc"}},
			}}
			fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", sentinel)}
			w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
			fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			w.setClock(func() time.Time { return fixed })
			w.SetCircuitBackoff(60*time.Second, 30*time.Minute)

			// First call dequeues, hits sentinel, opens circuit.
			if err := w.RunOnce(context.Background()); err != nil {
				if !errors.Is(err, errIdle) {
					t.Fatalf("RunOnce: %v; want nil or an idle sentinel", err)
				}
			}
			if len(q.failed) != 0 {
				t.Fatalf("failed = %v; want none on circuit-open trip", q.failed)
			}
			if got := q.released; len(got) != 1 || got[0] != 900 {
				t.Fatalf("released = %v; want [900] (dequeued item must return to pending pool, not stay in processing)", got)
			}
			if len(q.processing) != 0 {
				t.Fatalf("processing = %v; want empty after release", q.processing)
			}
			if w.circuit.OpenUntil().IsZero() {
				t.Fatal("circuitOpenUntil = zero; want circuit opened")
			}
			if got, want := w.circuit.OpenUntil(), fixed.Add(60*time.Second); !got.Equal(want) {
				t.Fatalf("circuitOpenUntil = %v; want %v (trip 1 uses the geometric base, not the flat cap)", got, want)
			}

			// Subsequent call must skip dequeue entirely while circuit open.
			callsBefore := fetcher.calls
			itemsBefore := len(q.items)
			err := w.RunOnce(context.Background())
			if !errors.Is(err, errLanesUnavailable) {
				t.Fatalf("RunOnce while open = %v; want errLanesUnavailable", err)
			}
			if fetcher.calls != callsBefore {
				t.Fatalf("fetcher.calls = %d; want unchanged %d (no dequeue while open)", fetcher.calls, callsBefore)
			}
			if len(q.items) != itemsBefore {
				t.Fatalf("queue items = %d; want unchanged %d (no dequeue while open)", len(q.items), itemsBefore)
			}
			if len(q.failed) != 0 {
				t.Fatalf("failed = %v; want none while circuit open", q.failed)
			}

			// Advance the clock past the window; next RunOnce closes the circuit
			// and resumes processing (and trips again on the same fetcher).
			w.setClock(func() time.Time { return fixed.Add(31 * time.Minute) })
			err = w.RunOnce(context.Background())
			if err != nil && !errors.Is(err, errIdle) {
				t.Fatalf("RunOnce after window = %v; want nil or an idle sentinel", err)
			}
			if fetcher.calls == callsBefore {
				t.Fatalf("fetcher.calls = %d; want >%d after circuit closed", fetcher.calls, callsBefore)
			}
		})
	}
}

// newCircuitWorker builds a worker with a frozen clock and base=60s / cap=30m
// for directly exercising the lane's throttle classification (formerly the
// worker's tripCircuitIfRateLimited) via tripViaLane, without driving full
// RunOnce. The returned fakeFetcher is the lane's provider.
func newCircuitWorker() (*Worker, *fakeQueue, *fakeFetcher, time.Time) {
	q := &fakeQueue{}
	f := &fakeFetcher{}
	w := New(q, &fakeCache{}, f, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(60*time.Second, 30*time.Minute)
	return w, q, f, fixed
}

func tripViaLane(w *Worker, f *fakeFetcher, item queue.WorkItem, err error) (bool, error) {
	f.err = err
	if ou := w.circuit.OpenUntil(); !ou.IsZero() {
		w.setClock(func() time.Time { return ou.Add(time.Nanosecond) })
	}
	_, ferr := w.lane.FindLyrics(context.Background(), item.Inputs.Track, "")
	if orchestrator.ClassifyOutcome(ferr) == orchestrator.OutcomeAuthRateLimit {
		return true, w.releaseAfterThrottle(context.Background(), item)
	}
	return false, nil
}

func TestCircuitRampIncrementsAndCaps(t *testing.T) {
	w, _, f, _ := newCircuitWorker()
	item := queue.WorkItem{ID: 1}
	throttle := fmt.Errorf("upstream: %w", musixmatch.ErrRateLimited)

	// trip 6 reaches the cap: 60 -> 120 -> 240 -> 480 -> 960 -> 1800 (capped).
	// Ported from the inline tripCircuitIfRateLimited (no open gate) to drive the
	// lane (which enforces the open gate): tripViaLane advances the clock past
	// each window before the next probe, so the geometric ramp is asserted as the
	// WINDOW size measured from the clock at the moment of the trip, not relative
	// to a single frozen base. Equivalent ramp/cap assertion.
	wantWindows := []time.Duration{
		60 * time.Second, 120 * time.Second, 240 * time.Second,
		480 * time.Second, 960 * time.Second, 30 * time.Minute,
	}
	for i, want := range wantWindows {
		tripped, releaseErr := tripViaLane(w, f, item, throttle)
		if !tripped || releaseErr != nil {
			t.Fatalf("trip %d: tripped=%v releaseErr=%v; want tripped, no error", i+1, tripped, releaseErr)
		}
		if got := w.circuit.OpenUntil().Sub(w.now()); got != want {
			t.Fatalf("trip %d: window = %v; want %v", i+1, got, want)
		}
		if w.circuit.Trips() != i+1 {
			t.Fatalf("trip %d: consecutiveCircuitTrips = %d; want %d", i+1, w.circuit.Trips(), i+1)
		}
	}
}

func TestThrottleAfterSuccessLogsWarn(t *testing.T) {
	recs := captureLogs(t)
	w, _, f, fixed := newCircuitWorker()
	w.circuit.RecordSuccess()

	tripViaLane(w, f, queue.WorkItem{ID: 1}, fmt.Errorf("x: %w", musixmatch.ErrUnauthorized))

	if !hasLog(*recs, slog.LevelWarn, "provider throttling") {
		t.Fatalf("logs = %+v; want Warn 'provider throttling' after a validated session", *recs)
	}
	if hasLog(*recs, slog.LevelWarn, "verify your token") {
		t.Fatal("logged the no-success Warn even though everProviderSuccess was set")
	}
	// The circuit-open log must carry backoff and next_retry on this branch.
	rec := findLog(*recs, slog.LevelWarn, "provider throttling")
	if rec == nil {
		t.Fatal("no captured record for the throttling Warn")
	}
	if got := attrDuration(rec, "backoff"); got != 60*time.Second {
		t.Fatalf("backoff attr = %v; want 60s", got)
	}
	if got := attrTime(rec, "next_retry"); !got.Equal(fixed.Add(60 * time.Second)) {
		t.Fatalf("next_retry attr = %v; want %v", got, fixed.Add(60*time.Second))
	}
}

func TestThrottleBeforeAnySuccessLogsWarn(t *testing.T) {
	recs := captureLogs(t)
	w, _, f, _ := newCircuitWorker()

	tripViaLane(w, f, queue.WorkItem{ID: 1}, fmt.Errorf("x: %w", musixmatch.ErrUnauthorized))

	if !hasLog(*recs, slog.LevelWarn, "no successful fetch yet") {
		t.Fatalf("logs = %+v; want Warn advising token verification before any success", *recs)
	}
}

func TestEscalationWarnAfterThreshold(t *testing.T) {
	recs := captureLogs(t)
	w, _, f, _ := newCircuitWorker()
	w.circuit.RecordSuccess()
	throttle := fmt.Errorf("x: %w", musixmatch.ErrRateLimited)

	for i := 0; i < escalationThreshold; i++ {
		tripViaLane(w, f, queue.WorkItem{ID: 1}, throttle)
	}

	if w.circuit.Trips() != escalationThreshold {
		t.Fatalf("consecutiveCircuitTrips = %d; want %d", w.circuit.Trips(), escalationThreshold)
	}
	if !hasLog(*recs, slog.LevelWarn, "may have expired") {
		t.Fatalf("logs = %+v; want escalation Warn after %d trips", *recs, escalationThreshold)
	}
}

func TestRenewalHoldsFullCapAndDoesNotIncrementTrips(t *testing.T) {
	recs := captureLogs(t)
	w, q, f, fixed := newCircuitWorker()
	// A genuine renewal is loud even after earlier success.
	w.circuit.RecordSuccess()
	renewal := fmt.Errorf("x: %w", musixmatch.ErrTokenRenewalRequired)

	tripped, releaseErr := tripViaLane(w, f, queue.WorkItem{ID: 7}, renewal)
	if !tripped || releaseErr != nil {
		t.Fatalf("tripped=%v releaseErr=%v; want tripped, no error", tripped, releaseErr)
	}
	if got := w.circuit.OpenUntil().Sub(fixed); got != 30*time.Minute {
		t.Fatalf("window = %v; want the full cap (30m), not the geometric base", got)
	}
	if w.circuit.Trips() != 0 {
		t.Fatalf("consecutiveCircuitTrips = %d; want 0 (renewal must not advance the throttle ramp)", w.circuit.Trips())
	}
	if got := q.released; len(got) != 1 || got[0] != 7 {
		t.Fatalf("released = %v; want [7]", got)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (circuit open is not a failure)", q.failed)
	}
	if !hasLog(*recs, slog.LevelWarn, "token renewal required") {
		t.Fatalf("logs = %+v; want a loud Warn for a genuine renewal", *recs)
	}

	// A subsequent bare-401 throttle starts the ramp at the base, proving the
	// renewal left the ramp position untouched. tripViaLane advances the clock
	// past the renewal window first, so assert the WINDOW size from the trip clock.
	tripViaLane(w, f, queue.WorkItem{ID: 8}, fmt.Errorf("x: %w", musixmatch.ErrUnauthorized))
	if got := w.circuit.OpenUntil().Sub(w.now()); got != 60*time.Second {
		t.Fatalf("post-renewal throttle window = %v; want base 60s (ramp position preserved)", got)
	}
}

func TestRenewalReleaseErrorIsSurfaced(t *testing.T) {
	w, q, f, _ := newCircuitWorker()
	q.releaseErr = errors.New("release boom")
	renewal := fmt.Errorf("x: %w", musixmatch.ErrTokenRenewalRequired)

	tripped, releaseErr := tripViaLane(w, f, queue.WorkItem{ID: 7}, renewal)
	if !tripped {
		t.Fatal("tripped = false; want true (renewal still opens the circuit)")
	}
	if releaseErr == nil {
		t.Fatal("releaseErr = nil; want the Release failure surfaced so the item is not silently orphaned")
	}
}

func TestRunOnceResetsCircuitTripsOnNonCacheSuccess(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{ID: 5, Inputs: models.Inputs{Track: track, OutputPaths: []models.OutputPath{{Outdir: "out", Filename: "a.lrc"}}}}}}
	fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "fresh"}}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	// Prime the ramp to 3 trips, then advance past the window so the gate is
	// half-open (not open) and RunOnce can proceed to a real provider fetch.
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(60*time.Second, 30*time.Minute)
	w.circuit.Trip()
	w.circuit.Trip()
	w.circuit.Trip()
	w.setClock(func() time.Time { return fixed.Add(time.Hour) })

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if w.circuit.Trips() != 0 {
		t.Fatalf("consecutiveCircuitTrips = %d; want 0 after a non-cache success", w.circuit.Trips())
	}
	if !w.circuit.EverSucceeded() {
		t.Fatal("everProviderSuccess = false; want true after a non-cache provider fetch")
	}
}

func TestRunOnceResetsCircuitTripsOnBenignMiss(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{ID: 6, Inputs: models.Inputs{Track: track}}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	// Prime the ramp to 3 trips, then advance past the window so the gate is
	// half-open (not open) and RunOnce can proceed to a real provider fetch.
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(60*time.Second, 30*time.Minute)
	w.circuit.Trip()
	w.circuit.Trip()
	w.circuit.Trip()
	w.setClock(func() time.Time { return fixed.Add(time.Hour) })

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if w.circuit.Trips() != 0 {
		t.Fatalf("consecutiveCircuitTrips = %d; want 0 after a benign miss (a clean 404 proves we are not throttled)", w.circuit.Trips())
	}
	if w.circuit.EverSucceeded() {
		t.Fatal("everProviderSuccess = true; a benign miss is not a provider match")
	}
}

func TestRunOnceCacheHitDoesNotMarkProviderSuccess(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	cached, err := encodeSong(models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "cached"}})
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	q := &fakeQueue{items: []queue.WorkItem{{ID: 9, Inputs: models.Inputs{Track: track, OutputPaths: []models.OutputPath{{Outdir: "out", Filename: "a.lrc"}}}}}}
	w := New(q, &fakeCache{hit: cached}, &fakeFetcher{}, &fakeWriter{})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if w.circuit.EverSucceeded() {
		t.Fatal("everProviderSuccess = true after a cache hit; a cache hit never touches the provider")
	}
}

func TestRunOnceWithOpenCircuitDoesNotIncrementBackoff(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(10*time.Minute, 30*time.Minute)
	w.circuit.Trip() // opens until fixed+10m

	if err := w.RunOnce(context.Background()); !errors.Is(err, errLanesUnavailable) {
		t.Fatalf("RunOnce = %v; want errLanesUnavailable", err)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (open-circuit must not trip backoff)", w.consecutiveFailures)
	}
}

func TestRunOnceSurfacesReleaseFailureAfterCircuitTrip(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{
		items: []queue.WorkItem{
			{ID: 950, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
		},
		releaseErr: errors.New("db down"),
	}
	fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrRateLimited)}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.SetCircuitOpenDuration(30 * time.Minute)

	err := w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce returned nil; want release-failure error to be surfaced")
	}
	if errors.Is(err, errIdle) {
		t.Fatalf("RunOnce returned an idle sentinel; want a real error so the outer loop can react. got %v", err)
	}
	if !errors.Is(err, q.releaseErr) {
		t.Fatalf("RunOnce error %v; want errors.Is(_, releaseErr) so the cause is preserved", err)
	}
	// Circuit must still be opened even though release failed; we want the
	// quiet window applied to upstream while operators investigate the
	// orphaned row.
	if w.circuit.OpenUntil().IsZero() {
		t.Fatal("circuitOpenUntil = zero; want circuit opened despite release failure")
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none on circuit-open trip even when release fails", q.failed)
	}
}

func TestThrottleResetsStaleFailureState(t *testing.T) {
	w, _, f, _ := newCircuitWorker()
	// Simulate a prior hard failure having pinned the consecutive-failure WARN
	// onto a now-stale victim.
	w.consecutiveFailures = 4
	w.lastFailID = 42
	w.lastFailArtist = "Stale Artist"
	w.lastFailTrack = "Stale Track"

	tripViaLane(w, f, queue.WorkItem{ID: 1}, fmt.Errorf("x: %w", musixmatch.ErrUnauthorized))

	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a throttle is not the song's fault)", w.consecutiveFailures)
	}
	if w.lastFailID != 0 || w.lastFailArtist != "" || w.lastFailTrack != "" {
		t.Fatalf("lastFail* = (%d, %q, %q); want all cleared so the WARN stops naming a stale victim",
			w.lastFailID, w.lastFailArtist, w.lastFailTrack)
	}
}

// TestTruncatedResponseRequeuesDeferredWithoutTrippingCircuit covers #496: a
// truncated/empty-body response (HasSubtitles=1, empty subtitle_body) is a
// deterministic per-request condition, not a transient rate limit, so it must
// NOT route through the no-cost throttle Release (which left attempts/
// miss_count/next_attempt_at untouched and returned the row to pending at
// priority=0, spinning forever ahead of the miss-cadence-parked rows). It must
// take the same state-advancing benign-miss Defer path as ErrNotFound /
// ErrNoLyrics, and must never trip the circuit breaker or log as throttling.
func TestTruncatedResponseRequeuesDeferredWithoutTrippingCircuit(t *testing.T) {
	recs := captureLogs(t)
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 77, Attempts: 0, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
	}}
	fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrTruncatedResponse)}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(60*time.Second, 30*time.Minute)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v; want nil (a truncated response defers, it does not throttle-release)", err)
	}
	if !w.circuit.OpenUntil().IsZero() {
		t.Fatal("circuitOpenUntil != zero; a truncated response must NOT open the circuit")
	}
	if len(q.deferred) != 1 || q.deferred[0] != 77 {
		t.Fatalf("deferred = %v; want [77] (Defer, not Release)", q.deferred)
	}
	if len(q.released) != 0 {
		t.Fatalf("released = %v; want none (no-cost Release is the bug this guards against)", q.released)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (a truncated response is not the song's failure)", q.failed)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a truncated response must not trip backoff)", w.consecutiveFailures)
	}
	if hasLog(*recs, slog.LevelWarn, "throttling") || hasLog(*recs, slog.LevelWarn, "rate limit") {
		t.Fatalf("logs = %+v; want no throttle/rate-limit wording for a truncated response", *recs)
	}
}

func TestCircuitHalfOpenThenRecovers(t *testing.T) {
	recs := captureLogs(t)
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 1, Inputs: models.Inputs{Track: track, OutputPaths: []models.OutputPath{{Outdir: "out", Filename: "a.lrc"}}}},
	}}
	fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "fresh"}}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	// Open the circuit two minutes in the past (base window 60s elapsed by now),
	// so now() is past openUntil and the next RunOnce probes (half-open).
	w.setClock(func() time.Time { return fixed.Add(-2 * time.Minute) })
	w.SetCircuitBackoff(60*time.Second, 30*time.Minute)
	w.circuit.Trip()
	w.setClock(func() time.Time { return fixed })

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !hasLog(*recs, slog.LevelDebug, "half-open") {
		t.Fatalf("logs = %+v; want a Debug 'half-open' on the probe", *recs)
	}
	if !hasLog(*recs, slog.LevelInfo, "circuit closed; recovered") {
		t.Fatalf("logs = %+v; want Info recovery after a successful probe round-trip", *recs)
	}
	if got := w.circuit.Allow(); got != circuit.StateClosed {
		t.Fatalf("circuit state = %v; want StateClosed after recovery", got)
	}
}

type fakeCacheToggle struct {
	hits    []bool
	payload string
	idx     int
}

func (c *fakeCacheToggle) Lookup(_ context.Context, _ string, _ string, _ int) (string, error) {
	hit := false
	if c.idx < len(c.hits) {
		hit = c.hits[c.idx]
	}
	c.idx++
	if hit {
		return c.payload, nil
	}
	return "", sql.ErrNoRows
}

func (c *fakeCacheToggle) Store(context.Context, string, string, int, string) error {
	return nil
}

// scan_results writeback for successful completions is now atomic inside
// queue.DBQueue.Complete and is covered by queue tests against real SQLite,
// so worker tests no longer need a fake ScanResults dependency.

func TestConfidence(t *testing.T) {
	want := models.Track{ArtistName: "  Héllo ", TrackName: "World"}
	got := models.Track{ArtistName: "hello", TrackName: " world "}

	if score := Confidence(want, got); score != 1 {
		t.Fatalf("Confidence() = %v; want 1", score)
	}
}

// TestDecodeSong_InstrumentalPreserved verifies that a cached instrumental Song
// (Track.Instrumental=1) retains its recording attributes after decodeSong pairs
// it with a live fallback track that carries only file-identity fields
// (ArtistName/TrackName/AlbumName) but has Instrumental=0.
//
// This is a regression guard: song.Track = fallback would wipe Instrumental=1
// and break the writer's `song.Track.Instrumental == 1` branch on cache hits.
func TestDecodeSong_InstrumentalPreserved(t *testing.T) {
	cached := models.Song{
		Track: models.Track{
			ArtistName:   "Cached Artist",
			TrackName:    "Cached Title",
			AlbumName:    "Cached Album",
			Instrumental: 1,
			HasLyrics:    0,
			HasSubtitles: 0,
			TrackLength:  240,
		},
	}
	b, err := json.Marshal(cached)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	fallback := models.Track{
		ArtistName:   "Live Artist",
		TrackName:    "Live Title",
		AlbumName:    "Live Album",
		Instrumental: 0, // file tag does not carry this; must NOT overwrite cached value
	}

	got := decodeSong(string(b), fallback)

	// Identity fields must come from the live file.
	if got.Track.ArtistName != "Live Artist" {
		t.Errorf("ArtistName = %q; want %q", got.Track.ArtistName, "Live Artist")
	}
	if got.Track.TrackName != "Live Title" {
		t.Errorf("TrackName = %q; want %q", got.Track.TrackName, "Live Title")
	}
	if got.Track.AlbumName != "Live Album" {
		t.Errorf("AlbumName = %q; want %q", got.Track.AlbumName, "Live Album")
	}

	// Recording attributes must come from the cached blob, not fallback.
	if got.Track.Instrumental != 1 {
		t.Errorf("Instrumental = %d; want 1 (must not be overwritten by fallback)", got.Track.Instrumental)
	}
	if got.Track.HasSubtitles != 0 {
		t.Errorf("HasSubtitles = %d; want 0", got.Track.HasSubtitles)
	}
	if got.Track.TrackLength != 240 {
		t.Errorf("TrackLength = %d; want 240", got.Track.TrackLength)
	}
}

// TestMissCadenceEscalates verifies that requeueDeferred uses escalating
// geometric cooldowns driven by item.MissCount rather than a fixed window.
func TestMissCadenceEscalates(t *testing.T) {
	tests := []struct {
		name      string
		missCount int // current miss_count BEFORE this Defer (i.e. item.MissCount)
		want      time.Duration
	}{
		{"miss1", 0, backoff.DefaultMissBase},     // 168h (7d)
		{"miss2", 1, 2 * backoff.DefaultMissBase}, // 336h (14d)
		{"miss3", 2, backoff.DefaultMissCap},      // 4*168h=672h = cap (28d)
		{"miss4", 3, backoff.DefaultMissCap},      // cap; already at ceiling
		// cap at DefaultMissCap (28 days = 672h)
		{"miss7", 6, backoff.DefaultMissCap},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			track := models.Track{ArtistName: "Artist", TrackName: "Title"}
			q := &fakeQueue{items: []queue.WorkItem{{
				ID:        1,
				MissCount: tc.missCount,
				Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
			}}}
			w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})

			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(q.deferDurations) != 1 || q.deferDurations[0] != tc.want {
				t.Fatalf("defer cooldown = %v; want %v (miss_count=%d)", q.deferDurations, tc.want, tc.missCount)
			}
			if len(q.deferred) != 1 {
				t.Fatalf("deferred = %v; want one item", q.deferred)
			}
			if len(q.failed) != 0 {
				t.Fatalf("failed = %v; want none", q.failed)
			}
		})
	}
}

// TestSetMissBackoffOverridesDefaults confirms that SetMissBackoff customizes
// the cadence used by requeueDeferred.
func TestSetMissBackoffOverridesDefaults(t *testing.T) {
	customBase := 12 * time.Hour
	customCap := 48 * time.Hour
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:        1,
		MissCount: 0,
		Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.SetMissBackoff(customBase, customCap)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.deferDurations) != 1 || q.deferDurations[0] != customBase {
		t.Fatalf("defer cooldown = %v; want %v (custom base)", q.deferDurations, customBase)
	}
}

// TestSetMissBackoffCapClamps verifies that a miss at a high count is bounded
// by the custom cap.
func TestSetMissBackoffCapClamps(t *testing.T) {
	customBase := 10 * time.Hour
	customCap := 20 * time.Hour
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:        1,
		MissCount: 5, // would be 10*2^5 = 320h without cap
		Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.SetMissBackoff(customBase, customCap)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.deferDurations) != 1 || q.deferDurations[0] != customCap {
		t.Fatalf("defer cooldown = %v; want cap %v", q.deferDurations, customCap)
	}
}

// TestMaxMissAttemptsRetires verifies that when miss_count+1 >= maxMissAttempts
// the worker calls RetireMiss instead of Defer. With cap=3, the 3rd miss
// (MissCount=2, nextMissCount=3) is the retirement boundary -- exactly N
// fetches occur before the row is retired.
func TestMaxMissAttemptsRetires(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:        99,
		MissCount: 2, // next miss_count=3 == cap; retires on the 3rd miss
		Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.SetMaxMissAttempts(3)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.retired) != 1 || q.retired[0] != 99 {
		t.Fatalf("retired = %v; want [99]", q.retired)
	}
	if len(q.deferred) != 0 {
		t.Fatalf("deferred = %v; want none (should have retired)", q.deferred)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none (retirement is not a failure)", q.failed)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none (RetireMiss does not go through Complete)", q.completed)
	}
	// Consecutive failures must not be bumped on a retirement.
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (retirement is not a failure)", w.consecutiveFailures)
	}
}

// TestMaxMissAttemptsRetiresBoundary verifies that max_miss_attempts=1 retires
// on the very first miss (nextMissCount=1 >= cap=1).
func TestMaxMissAttemptsRetiresBoundary(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:        101,
		MissCount: 0, // next miss_count=1 == cap=1; retires on the 1st miss
		Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.SetMaxMissAttempts(1)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.retired) != 1 || q.retired[0] != 101 {
		t.Fatalf("retired = %v; want [101] (max_miss_attempts=1 retires on first miss)", q.retired)
	}
	if len(q.deferred) != 0 {
		t.Fatalf("deferred = %v; want none (should have retired on first miss)", q.deferred)
	}
}

// TestMaxMissAttemptsZeroNeverRetires verifies that the default (0 = no cap)
// defers indefinitely.
func TestMaxMissAttemptsZeroNeverRetires(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:        1,
		MissCount: 1000, // very high miss_count; cap is 0
		Inputs:    models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"},
	}}}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	// Default maxMissAttempts = 0 (no cap)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.retired) != 0 {
		t.Fatalf("retired = %v; want none (max=0 means no cap)", q.retired)
	}
	if len(q.deferred) != 1 {
		t.Fatalf("deferred = %v; want one item", q.deferred)
	}
}

// TestRetireMissNoRowsIsBenign covers the lost-race path in requeueDeferred's
// retire branch: if RetireMiss returns sql.ErrNoRows the worker must not error.
func TestRetireMissNoRowsIsBenign(t *testing.T) {
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:        88,
			MissCount: 5,
			Inputs:    models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}},
		}},
		retireErr: sql.ErrNoRows,
	}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.SetMaxMissAttempts(3)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v; want nil (RetireMiss no-rows is benign)", err)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0", w.consecutiveFailures)
	}
}

// TestSetMissBackoffIgnoresZero confirms that zero values are silently ignored.
func TestSetMissBackoffIgnoresZero(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	origBase := w.missBackoffBase
	origCap := w.missBackoffCap
	w.SetMissBackoff(0, 0)
	if w.missBackoffBase != origBase {
		t.Fatalf("missBackoffBase changed after SetMissBackoff(0,0)")
	}
	if w.missBackoffCap != origCap {
		t.Fatalf("missBackoffCap changed after SetMissBackoff(0,0)")
	}
}

// TestSetMaxMissAttemptsClampNegative verifies negative values clamp to 0.
func TestSetMaxMissAttemptsClampNegative(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetMaxMissAttempts(-5)
	if w.maxMissAttempts != 0 {
		t.Fatalf("maxMissAttempts = %d; want 0 after clamping -5", w.maxMissAttempts)
	}
}

// fakeDetector is a test detector.Detector that returns a fixed result or error.
type fakeDetector struct {
	instrumental bool
	version      string
	err          error
	calls        []string
	// result, when its Version is non-empty, overrides the instrumental/version
	// fields above and is returned verbatim - lets tests set the full telemetry
	// (Confidence/VocalConfidence/SpeechConfidence/WinningVocalClass) without
	// breaking existing tests that only set instrumental/version.
	result detector.Result
}

func (d *fakeDetector) Detect(_ context.Context, audioPath string) (detector.Result, error) {
	d.calls = append(d.calls, audioPath)
	if d.err != nil {
		return detector.Result{}, d.err
	}
	if d.result.Version != "" {
		return d.result, nil
	}
	return detector.Result{Instrumental: d.instrumental, Version: d.version}, nil
}

// TestRunOnceDetectorInstrumentalWritesMarkerAndCompletes verifies that when
// the audio detector returns instrumental=true on a benign miss, the worker
// writes an instrumental marker, stores it in the cache, and completes the item.
func TestRunOnceDetectorInstrumentalWritesMarkerAndCompletes(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Interlude"}
	audioPath := "/music/interlude.flac"
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 200,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: audioPath,
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNotFound}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Detector must have been called with the source path.
	if len(det.calls) != 1 || det.calls[0] != audioPath {
		t.Fatalf("detector calls = %v; want [%s]", det.calls, audioPath)
	}
	// Item must be completed, not deferred or failed.
	if len(q.completed) != 1 || q.completed[0] != 200 {
		t.Fatalf("completed = %v; want [200]", q.completed)
	}
	if len(q.deferred) != 0 {
		t.Fatalf("deferred = %v; want none (instrumental: completed not deferred)", q.deferred)
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none", q.failed)
	}
	// Writer must have produced exactly one write (the instrumental .txt).
	if len(writer.writes) != 1 {
		t.Fatalf("writes = %v; want one instrumental marker write", writer.writes)
	}
	// The instrumental song handed to the writer must carry the detector version
	// so the marker is stamped with detector provenance (#502).
	if len(writer.songs) != 1 || writer.songs[0].DetectorVersion != "9.9.9" {
		t.Fatalf("written song DetectorVersion = %q; want \"9.9.9\"", func() string {
			if len(writer.songs) == 1 {
				return writer.songs[0].DetectorVersion
			}
			return "<no song captured>"
		}())
	}
	// A detector-sourced instrumental must NOT be cached. WinningLane and the
	// whole Detector* telemetry block carry `json:"-"`, so a stored entry would
	// decode as a bare Instrumental=1 song: a later cache hit would write the
	// marker but stamp no instrumental_result and no provenance, permanently,
	// for every track sharing this artist/track/duration key.
	if len(c.stores) != 0 {
		t.Fatalf("cache stores = %d; want 0 (a detector verdict is not replayable through the cache, so it must not be stored)", len(c.stores))
	}
	// Consecutive failure counter must remain 0.
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0", w.consecutiveFailures)
	}
}

// TestRunOnceDetectorReturnsFalseDefersNormally verifies that when the detector
// returns instrumental=false on a benign miss, the worker falls through to the
// normal deferred miss path (no write, item deferred).
func TestRunOnceDetectorReturnsFalseDefersNormally(t *testing.T) {
	track := models.Track{ArtistName: "Singer", TrackName: "Song"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 201,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "song.lrc",
			SourcePath: "/music/song.flac",
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNoLyrics}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: false}

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(det.calls) != 1 {
		t.Fatalf("detector calls = %d; want 1", len(det.calls))
	}
	// Not instrumental: no write, no cache store, item deferred.
	if len(writer.writes) != 0 {
		t.Fatalf("writes = %v; want none (not instrumental)", writer.writes)
	}
	if len(c.stores) != 0 {
		t.Fatalf("cache stores = %d; want 0 (not instrumental)", len(c.stores))
	}
	if len(q.deferred) != 1 || q.deferred[0] != 201 {
		t.Fatalf("deferred = %v; want [201]", q.deferred)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
}

// TestRunOnceDetectorErrorTreatsAsMiss verifies that a detector error is
// non-fatal: it is logged and the normal miss path (deferred) proceeds unchanged.
func TestRunOnceDetectorErrorTreatsAsMiss(t *testing.T) {
	recs := captureLogs(t)
	track := models.Track{ArtistName: "Artist", TrackName: "Opus"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 202,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "opus.lrc",
			SourcePath: "/music/opus.flac",
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNotFound}
	writer := &fakeWriter{}
	det := &fakeDetector{err: errors.New("sidecar timeout")}

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// A detector error now surfaces as ErrLaneOutage and is logged by the
	// orchestrator's detectorClassifier (not the worker) as a circuit-open
	// warning, since the detector runs as a lane inside FindLyrics rather than
	// via an inline worker call (#502). The item must still end up deferred, as
	// asserted below.
	if !hasLog(*recs, slog.LevelWarn, "lane circuit opened: detector outage; degrading to providers") {
		t.Fatalf("logs = %+v; want a warning about the detector lane circuit opening", *recs)
	}
	// Item must be deferred (normal miss path).
	if len(q.deferred) != 1 || q.deferred[0] != 202 {
		t.Fatalf("deferred = %v; want [202] (error treated as miss)", q.deferred)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none", q.completed)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("writes = %v; want none (error treated as miss)", writer.writes)
	}
}

// TestRunOnceDetectorDisabledWhenNilSourcePath verifies that the detector is
// skipped when the source path is empty (no audio file available for detection).
func TestRunOnceDetectorDisabledWhenNilSourcePath(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Opus"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 203,
		Inputs: models.Inputs{
			Track:    track,
			Outdir:   "out",
			Filename: "opus.lrc",
			// SourcePath intentionally empty: no audio file.
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNotFound}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true} // would produce instrumental if called

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Detector must NOT be called when source path is absent.
	if len(det.calls) != 0 {
		t.Fatalf("detector calls = %v; want none (no source path)", det.calls)
	}
	// Normal miss path: deferred.
	if len(q.deferred) != 1 || q.deferred[0] != 203 {
		t.Fatalf("deferred = %v; want [203]", q.deferred)
	}
}

// TestRunOnceDetectorInstrumentalWriteErrorDefersAsMiss verifies that when the
// writer fails while writing an instrumental marker, the item is deferred (not
// completed) and no consecutive failure counter is incremented.
func TestRunOnceDetectorInstrumentalWriteErrorDefersAsMiss(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Silent Piece"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 205,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "silent.lrc",
			SourcePath: "/music/silent.flac",
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNotFound}
	writer := &fakeWriter{err: errors.New("write failed")}
	det := &fakeDetector{instrumental: true}

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Write failed: item must be deferred (miss path), not completed.
	if len(q.deferred) != 1 || q.deferred[0] != 205 {
		t.Fatalf("deferred = %v; want [205] (write error treated as miss)", q.deferred)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none (write failed)", q.completed)
	}
	// consecutiveFailures must be 0 (write error in instrumental path is non-fatal).
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0", w.consecutiveFailures)
	}
	// A write failure must NOT leave a cached instrumental song behind: the cache
	// store must happen only after the output marker writes succeed, else a
	// deferred retry would hit the cache and complete without ever restoring
	// instrumental_result or the detector telemetry (those are not serialized
	// into the cached song).
	if len(c.stores) != 0 {
		t.Fatalf("cache stores = %d; want none (write failed; must not cache)", len(c.stores))
	}
}

// TestRunOnceGuardInstalledWithOneProviderPlusDetector verifies the guard is
// wired into the orchestrator's suitability check in the common
// one-provider-plus-detector configuration, not just when there are multiple
// provider lanes. w.lanes (the provider-only slice) has length 1 here; the
// EFFECTIVE dispatch list (provider + detector) has length 2. Before the fix,
// rebuildOrchestrator tested len(w.lanes) > 1 and never installed the guard in
// this shape, so a guard-rejected provider result was accepted outright
// instead of falling through to the demoted detector lane.
func TestRunOnceGuardInstalledWithOneProviderPlusDetector(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Aria"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 206,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "aria.lrc",
			SourcePath: "/music/aria.flac",
		},
	}}}
	c := &fakeCache{}
	// The provider returns quality-OK (unsynced) lyrics that the guard rejects.
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "wrong-language lyrics"},
	}}
	writer := &fakeWriter{}
	// A real script guard only rejects lyric bodies dominated by a disallowed
	// script; it has nothing to reject on the detector's textless instrumental
	// result. Model that here: reject only the provider's wrong-language body,
	// accept everything else (including the detector's empty-body song), so the
	// test isolates the fall-through wiring rather than an unconditional guard.
	guard := &selectiveGuard{rejectBody: "wrong-language lyrics", reason: "foreign-script share exceeds threshold"}
	det := &fakeDetector{instrumental: true, version: "1.2.3"}

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det) // ordering defaults to demoted: provider first, detector fallback.
	w.EnableGuard(guard)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The guard must have been consulted on both the provider's result (rejected)
	// and the detector's result (accepted): if it were never installed (the bug),
	// it would be called zero times and the provider's rejected result would
	// settle outright.
	if len(guard.calls) != 2 {
		t.Fatalf("guard calls = %d; want 2 (guard must be installed with one provider + detector)", len(guard.calls))
	}
	// A guard rejection must fall through to the demoted detector lane, not
	// settle on the rejected provider result.
	if len(det.calls) != 1 {
		t.Fatalf("detector calls = %v; want 1 (guard rejection must fall through to the detector lane)", det.calls)
	}
	// The completed write must be the detector's instrumental settle, not the
	// guard-rejected provider lyrics.
	if len(writer.songs) != 1 || writer.songs[0].DetectorVersion != "1.2.3" {
		t.Fatalf("written song DetectorVersion = %+v; want version 1.2.3 (detector fallback settle)", writer.songs)
	}
	if len(q.completed) != 1 || q.completed[0] != 206 {
		t.Fatalf("completed = %v; want [206]", q.completed)
	}
}

// TestRunOnceDetectorNotCalledOnSuccess verifies that the detector is NOT
// invoked when the provider returns lyrics (only invoked on benign misses).
func TestRunOnceDetectorNotCalledOnSuccess(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Hit"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 204,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "hit.lrc",
			SourcePath: "/music/hit.flac",
		},
	}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "found lyrics"},
	}}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true} // must never be called

	w := New(q, c, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(det.calls) != 0 {
		t.Fatalf("detector calls = %v; want none (provider succeeded, detector must not run)", det.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 204 {
		t.Fatalf("completed = %v; want [204]", q.completed)
	}
}

// TestRunOnceDetectorFrontSettlesWithoutProviders verifies that with "front"
// ordering, a gate-positive detector verdict settles the item without
// consulting any provider lane, and that the full detector telemetry
// round-trips through the stamp path.
func TestRunOnceDetectorFrontSettlesWithoutProviders(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 400,
		Inputs: models.Inputs{
			Track: track, Outdir: "out", Filename: "artist-title.lrc",
			SourcePath: "/music/x.flac",
		},
	}}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{}
	writer := &fakeWriter{}
	det := &fakeDetector{result: detector.Result{
		Instrumental: true, Confidence: 0.9, VocalConfidence: 0.01,
		SpeechConfidence: 0.02, WinningVocalClass: "Singing", Version: "1.5.0",
	}}

	w := New(q, cache, fetcher, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetDetectorOrdering("front")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("fetcher.calls = %d; want 0 (detector settled in front)", fetcher.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 400 {
		t.Fatalf("completed = %v; want [400]", q.completed)
	}
	if len(q.instrumentalStamps) != 1 {
		t.Fatalf("instrumentalStamps = %+v; want exactly one", q.instrumentalStamps)
	}
	got := q.instrumentalStamps[0]
	if got.ID != 400 || got.Result != 1 {
		t.Fatalf("stamp = %+v; want ID 400 result 1", got)
	}
	if got.Tel.DetectorVersion != "1.5.0" || got.Tel.MusicSum != 0.9 ||
		got.Tel.VocalPeak != 0.01 || got.Tel.SpeechMean != 0.02 || got.Tel.VocalClass != "Singing" {
		t.Fatalf("telemetry did not round-trip: %+v", got.Tel)
	}
	if q.outcomeTypes[400] != "instrumental" {
		t.Fatalf("outcomeTypes[400] = %q; want instrumental", q.outcomeTypes[400])
	}
}

func TestRunOnceRecordsProviderHitOnSuccess(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{ID: 301, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:       models.Track{ArtistName: "A", TrackName: "T"},
		WinningLane: "musixmatch",
		Lyrics:      models.Lyrics{LyricsBody: "lyrics"},
	}}
	writer := &fakeWriter{}
	rec := &fakeProviderRecorder{}

	w := New(q, c, fetcher, writer)
	w.SetProviderRecorder(rec)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(rec.hits) != 1 || rec.hits[0] != "musixmatch" {
		t.Errorf("provider hits = %v; want [musixmatch]", rec.hits)
	}
	if len(rec.misses) != 0 {
		t.Errorf("provider misses = %v; want none", rec.misses)
	}
}

func TestRunOnceRecordsProviderMissOnBenignMiss(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{ID: 302, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNoLyrics}
	writer := &fakeWriter{}
	rec := &fakeProviderRecorder{}

	w := New(q, c, fetcher, writer)
	w.SetProviderRecorder(rec)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The primary Musixmatch lane must be counted as a miss.
	if len(rec.misses) != 1 || rec.misses[0] != "musixmatch" {
		t.Errorf("provider misses = %v; want [musixmatch]", rec.misses)
	}
	if len(rec.hits) != 0 {
		t.Errorf("provider hits = %v; want none on benign miss", rec.hits)
	}
	if len(q.deferred) != 1 {
		t.Errorf("deferred = %v; want [302]", q.deferred)
	}
}

// TestRunOnceRecordsLaneAttemptsOnSuccess verifies the worker persists the
// per-track attribution carried out of the orchestrator on a successful fetch:
// the winning lane recorded as a hit for this queue row (#282).
func TestRunOnceRecordsLaneAttemptsOnSuccess(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{ID: 301, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  models.Track{ArtistName: "A", TrackName: "T"},
		Lyrics: models.Lyrics{LyricsBody: "lyrics"},
	}}
	rec := &fakeProviderRecorder{}

	w := New(q, c, fetcher, &fakeWriter{})
	w.SetProviderRecorder(rec)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got := rec.attempts[301]
	if len(got) != 1 || got[0].Lane != "musixmatch" || !got[0].Hit {
		t.Errorf("lane attempts for 301 = %+v; want [{musixmatch hit=true}]", got)
	}
}

// TestRunOnceRecordsLaneAttemptsOnBenignMiss verifies the worker persists an
// all-miss per-track attribution when every lane misses (#282).
func TestRunOnceRecordsLaneAttemptsOnBenignMiss(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{ID: 302, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{err: musixmatch.ErrNoLyrics}
	rec := &fakeProviderRecorder{}

	w := New(q, c, fetcher, &fakeWriter{})
	w.SetProviderRecorder(rec)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got := rec.attempts[302]
	if len(got) != 1 || got[0].Lane != "musixmatch" || got[0].Hit {
		t.Errorf("lane attempts for 302 = %+v; want [{musixmatch hit=false}]", got)
	}
}

func TestRunOnceNoRecorderIsNoop(t *testing.T) {
	// No SetProviderRecorder: RunOnce must not panic and must complete normally.
	q := &fakeQueue{items: []queue.WorkItem{{ID: 303, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}}}
	c := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  models.Track{ArtistName: "A", TrackName: "T"},
		Lyrics: models.Lyrics{LyricsBody: "lyrics"},
	}}
	writer := &fakeWriter{}

	w := New(q, c, fetcher, writer)
	// No SetProviderRecorder call.
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce without recorder: %v", err)
	}
	if len(q.completed) != 1 {
		t.Errorf("completed = %v; want [303]", q.completed)
	}
}

// TestRecordHitEmptyLaneIsNoop verifies that calling recordHit with an empty
// lane is a no-op: no recorder call, no queue stamp, no error.
func TestRecordHitEmptyLaneIsNoop(t *testing.T) {
	q := &fakeQueue{}
	rec := &fakeProviderRecorder{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetProviderRecorder(rec)

	// Call directly; empty lane must short-circuit before any recorder/queue call.
	w.recordHit(context.Background(), 999, "")

	if len(rec.hits) != 0 {
		t.Errorf("hits = %v; want none on empty lane", rec.hits)
	}
}

// TestRecordHitRecorderErrorIsNonFatal verifies that a RecordProviderHit error
// is logged but does not propagate (recordHit has no return value and the caller
// must not crash).
func TestRecordHitRecorderErrorIsNonFatal(t *testing.T) {
	q := &fakeQueue{}
	rec := &fakeProviderRecorder{hitErr: errors.New("recorder down")}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetProviderRecorder(rec)

	// Must not panic. The error is slog.Warn only.
	w.recordHit(context.Background(), 1, "musixmatch")

	if len(rec.hits) != 1 || rec.hits[0] != "musixmatch" {
		t.Errorf("hits = %v; want [musixmatch] (recorder called even if it errors)", rec.hits)
	}
}

// TestRecordHitQueueErrorIsNonFatal verifies that a SetProviderLane DB error is
// logged (slog.Warn) but does not propagate.
func TestRecordHitQueueErrorIsNonFatal(t *testing.T) {
	q := &fakeQueue{setProviderLaneErr: errors.New("db down")}
	rec := &fakeProviderRecorder{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetProviderRecorder(rec)

	// Must not panic.
	w.recordHit(context.Background(), 1, "musixmatch")
}

// TestRecordMissesRecorderErrorIsNonFatal verifies that a RecordProviderMiss
// error is logged (slog.Warn) but does not stop iteration or propagate.
func TestRecordMissesRecorderErrorIsNonFatal(t *testing.T) {
	q := &fakeQueue{}
	rec := &fakeProviderRecorder{missErr: errors.New("recorder down")}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.SetProviderRecorder(rec)

	// Must not panic; the worker has one lane ("musixmatch") via New.
	w.recordMisses(context.Background())

	// The recorder WAS called (iteration continued despite the error).
	if len(rec.misses) != 1 || rec.misses[0] != "musixmatch" {
		t.Errorf("misses = %v; want [musixmatch] (recorder called even if it errors)", rec.misses)
	}
}

// TestOutcomeTypeFromSong verifies the helper mirrors WriteLRC's branching
// exactly, including that an instrumental flag wins over a present synced
// subtitle line (Musixmatch delivers a synced line alongside the instrumental
// flag, so instrumental must be checked first). The empty case (nothing
// writable) returns "" so the caller leaves outcome_type NULL.
func TestOutcomeTypeFromSong(t *testing.T) {
	tests := []struct {
		name string
		song models.Song
		want string
	}{
		{
			name: "instrumental flag wins even with a synced line present",
			song: models.Song{
				Track:     models.Track{Instrumental: 1},
				Subtitles: models.Synced{Lines: []models.Lines{{Text: "la la"}}},
			},
			want: "instrumental",
		},
		{
			name: "synced when subtitle lines present",
			song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "la la"}}}},
			want: "synced",
		},
		{
			name: "unsynced when only a lyrics body is present",
			song: models.Song{Lyrics: models.Lyrics{LyricsBody: "plain words"}},
			want: "unsynced",
		},
		{
			name: "empty when nothing writable",
			song: models.Song{},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeTypeFromSong(tc.song); got != tc.want {
				t.Fatalf("outcomeTypeFromSong = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestRunOnceStampsOutcomeTypeOnSuccess verifies the normal success path stamps
// the true outcome type (here "unsynced", because the fetched song carries only
// a lyrics body) before completing -- the fix for issue #379, where the
// dashboard read every completed row as synced.
func TestRunOnceStampsOutcomeTypeOnSuccess(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     7,
		Inputs: models.Inputs{Track: track},
	}}}
	cache := &fakeCache{}
	fetcher := &fakeFetcher{song: models.Song{
		Track:  track,
		Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"},
	}}
	writer := &fakeWriter{}

	w := New(q, cache, fetcher, writer)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.completed) != 1 || q.completed[0] != 7 {
		t.Fatalf("completed = %v; want [7]", q.completed)
	}
	if got := q.outcomeTypes[7]; got != "unsynced" {
		t.Fatalf("outcome_type for id 7 = %q; want \"unsynced\"", got)
	}
}

// TestRunOnceStampsSyncedOutcomeType verifies a synced fetch (subtitle lines)
// stamps "synced".
func TestRunOnceStampsSyncedOutcomeType(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID:     8,
		Inputs: models.Inputs{Track: track},
	}}}
	fetcher := &fakeFetcher{song: models.Song{
		Track:     track,
		Subtitles: models.Synced{Lines: []models.Lines{{Text: "timed line"}}},
	}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := q.outcomeTypes[8]; got != "synced" {
		t.Fatalf("outcome_type for id 8 = %q; want \"synced\"", got)
	}
}

// TestRunOnceDetectorInstrumentalStampsOutcomeType verifies the audio-detector
// instrumental path stamps outcome_type "instrumental" before completing, so
// every instrumental source (not just instrumental_result) is tabulated.
func TestRunOnceDetectorInstrumentalStampsOutcomeType(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Silent Piece"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 9,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "silent.lrc",
			SourcePath: "/music/silent.flac",
		},
	}}}
	fetcher := &fakeFetcher{err: musixmatch.ErrNotFound}
	det := &fakeDetector{instrumental: true}

	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.completed) != 1 || q.completed[0] != 9 {
		t.Fatalf("completed = %v; want [9]", q.completed)
	}
	if got := q.outcomeTypes[9]; got != "instrumental" {
		t.Fatalf("outcome_type for id 9 = %q; want \"instrumental\"", got)
	}
}

func TestOpenCircuitReturnsLanesUnavailableNotQueueEmpty(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 1, Inputs: models.Inputs{
			Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
			Outdir:   "out",
			Filename: "a.lrc",
		}},
	}}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(10*time.Minute, 30*time.Minute)
	w.circuit.Trip() // every lane open until fixed+10m

	err := w.RunOnce(context.Background())

	if !errors.Is(err, errLanesUnavailable) {
		t.Fatalf("RunOnce with an open circuit = %v; want errLanesUnavailable", err)
	}
	if errors.Is(err, errQueueEmpty) {
		t.Fatal("RunOnce reported errQueueEmpty while a ready item sits in the queue and every lane is open-circuited: the queue is not empty, it is blocked")
	}
	if !errors.Is(err, errIdle) {
		t.Fatalf("RunOnce = %v; want it to still satisfy errIdle so the run loop unwinds without recording a failure", err)
	}
}

func TestThrottleReleaseReturnsThrottledNotQueueEmpty(t *testing.T) {
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 42, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}},
	}}
	fetcher := &fakeFetcher{err: fmt.Errorf("upstream: %w", musixmatch.ErrRateLimited)}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })

	err := w.RunOnce(context.Background())

	if !errors.Is(err, errThrottled) {
		t.Fatalf("RunOnce after a throttle = %v; want errThrottled", err)
	}
	if errors.Is(err, errQueueEmpty) {
		t.Fatal("RunOnce reported errQueueEmpty after releasing a throttled item back to the queue: the item is still there")
	}
	if !errors.Is(err, errIdle) {
		t.Fatalf("RunOnce = %v; want it to still satisfy errIdle", err)
	}
}

func TestRunLoopDoesNotClaimQueueEmptyWhenLanesAreBlocked(t *testing.T) {
	recs := captureLogs(t)
	q := &fakeQueue{items: []queue.WorkItem{
		{ID: 7, Inputs: models.Inputs{
			Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
			Outdir:   "out",
			Filename: "a.lrc",
		}},
	}}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fixed })
	w.SetCircuitBackoff(10*time.Minute, 30*time.Minute)
	w.circuit.Trip()

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v; want nil (idle unwinds cleanly)", err)
	}

	if hasLog(*recs, slog.LevelDebug, "queue empty") {
		t.Error("run loop logged \"queue empty\" while every lane was open-circuited and a ready item sat in the queue: this is the line that makes a livelocked worker look idle")
	}
	if !hasLog(*recs, slog.LevelDebug, "all lanes unavailable") {
		t.Error("run loop did not report that all lanes were unavailable; the operator has no way to tell a blocked queue from an empty one")
	}
}

// TestLogIdleNamesTheCauseNotJustEmptiness pins the whole point of issue #500:
// each idle cause must report itself honestly, and an unrecognized one must NOT
// claim the queue is empty. The default branch is the regression guard -- a future
// sentinel wrapping errIdle with no case of its own would otherwise silently
// re-arm exactly the lie this function exists to remove.
func TestLogIdleNamesTheCauseNotJustEmptiness(t *testing.T) {
	novelIdle := fmt.Errorf("%w: some future cause", errIdle)

	for _, tc := range []struct {
		name    string
		err     error
		wantSub string
	}{
		{"empty queue", errQueueEmpty, "queue empty"},
		{"lanes unavailable", errLanesUnavailable, "all lanes unavailable"},
		{"throttled", errThrottled, "provider throttled"},
		{"unrecognized idle cause", novelIdle, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := captureLogs(t)
			logIdle(tc.err)
			if !hasLog(*recs, slog.LevelDebug, tc.wantSub) {
				t.Fatalf("logIdle(%v) did not log %q; got %v", tc.err, tc.wantSub, *recs)
			}
		})
	}

	// The guard, stated separately: only the genuinely-empty cause may ever say so.
	for _, err := range []error{errLanesUnavailable, errThrottled, novelIdle} {
		recs := captureLogs(t)
		logIdle(err)
		if hasLog(*recs, slog.LevelDebug, "queue empty") {
			t.Errorf("logIdle(%v) claimed \"queue empty\"; only errQueueEmpty may say that (#500)", err)
		}
	}
}

// TestRebuildOrchestrator_DetectorOrdering verifies SetDetectorOrdering places
// the detector lane first ("front") or last (any other value, e.g. "demoted")
// among the dispatch lanes.
func TestRebuildOrchestrator_DetectorOrdering(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(&fakeDetector{})
	w.SetDetectorOrdering("front")
	if got := w.orch.LaneNames(); len(got) == 0 || got[0] != "detector" {
		t.Fatalf("front ordering: lanes = %v, want detector first", got)
	}
	w.SetDetectorOrdering("demoted")
	if last := w.orch.LaneNames(); len(last) == 0 || last[len(last)-1] != "detector" {
		t.Fatalf("demoted ordering: lanes = %v, want detector last", last)
	}
}

// TestRebuildOrchestratorSkipsDetectorLaneUnderParallelMode verifies that the
// detector lane is NOT installed when providers.mode=parallel. Under parallel
// dispatch every lane races, so a fast gate-positive detector verdict can be
// held as the winning result before a slower provider answers - which would let
// a lyrical track settle as instrumental. Detection is therefore left inactive
// under parallel (staged dispatch is tracked in #528). Asserted through
// observable behavior: the detector must never be invoked, and the item must
// take the ordinary deferred-miss path instead of being completed as
// instrumental.
func TestRebuildOrchestratorSkipsDetectorLaneUnderParallelMode(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Interlude"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 210,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: "/music/interlude.flac",
		},
	}}}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}

	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetProvidersMode(orchestrator.ModeParallel)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(det.calls) != 0 {
		t.Fatalf("detector calls = %v; want none (the detector lane must not be installed under parallel mode)", det.calls)
	}
	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none (with detection inactive the item is an ordinary miss)", q.completed)
	}
	if len(q.deferred) != 1 {
		t.Fatalf("deferred = %v; want the item deferred as a normal miss", q.deferred)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("writes = %v; want none (no instrumental marker without a detector verdict)", writer.writes)
	}
}

// TestRebuildOrchestratorInstallsDetectorLaneUnderOrderedMode is the positive
// counterpart to the parallel case above: the same configuration under ordered
// mode MUST install the lane and settle the track as instrumental. Without this
// pair, the parallel test would also pass if the detector lane were broken
// outright.
func TestRebuildOrchestratorInstallsDetectorLaneUnderOrderedMode(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Interlude"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 211,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: "/music/interlude.flac",
		},
	}}}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}

	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetProvidersMode(orchestrator.ModeOrdered)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(det.calls) != 1 {
		t.Fatalf("detector calls = %v; want exactly one under ordered mode", det.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 211 {
		t.Fatalf("completed = %v; want [211] settled as instrumental", q.completed)
	}
}

// TestDetectorInstrumentalStampFailureDefersInsteadOfCompleting verifies that a
// failure to stamp instrumental_result is FATAL to the settle. Completing the
// row without the stamp would retire the work item while leaving no record that
// the detector settled it - unauditable, unreproducible, and never revisited by
// a later pass. The item must be deferred for retry instead.
func TestDetectorInstrumentalStampFailureDefersInsteadOfCompleting(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Interlude"}
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID: 220,
			Inputs: models.Inputs{
				Track:      track,
				Outdir:     "out",
				Filename:   "interlude.lrc",
				SourcePath: "/music/interlude.flac",
			},
		}},
		instrumentalErr: errors.New("db: disk I/O error"),
	}
	writer := &fakeWriter{}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}

	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, writer)
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(q.completed) != 0 {
		t.Fatalf("completed = %v; want none (a failed stamp must not complete the row)", q.completed)
	}
	if len(q.deferred) != 1 || q.deferred[0] != 220 {
		t.Fatalf("deferred = %v; want [220] deferred for retry after the stamp failure", q.deferred)
	}
	// The marker write already happened and WriteLRC is idempotent, so the retry
	// re-runs it harmlessly; the write is not rolled back.
	if len(writer.writes) != 1 {
		t.Fatalf("writes = %v; want the marker write to have happened before the stamp", writer.writes)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0 (a stamp failure is a deferral, not a provider failure)", w.consecutiveFailures)
	}
}

// TestDetectorInstrumentalRecordsHitAndLaneAttempts verifies that a detector
// settle records the same fetch bookkeeping an ordinary provider settle does.
// The detector completion path returns early, so without an explicit record the
// provider_outcomes counter would undercount and lane_attempts would have no row
// for this track at all - skewing the per-track hit-rate reports (#282) by
// exactly the set of tracks the detector resolves.
func TestDetectorInstrumentalRecordsHitAndLaneAttempts(t *testing.T) {
	track := models.Track{ArtistName: "Composer", TrackName: "Interlude"}
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 230,
		Inputs: models.Inputs{
			Track:      track,
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: "/music/interlude.flac",
		},
	}}}
	rec := &fakeProviderRecorder{}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}

	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetProviderRecorder(rec)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(q.completed) != 1 {
		t.Fatalf("completed = %v; want the item settled as instrumental", q.completed)
	}
	// The hit must be attributed to the detector lane, matching song.WinningLane.
	if len(rec.hits) != 1 || rec.hits[0] != "detector" {
		t.Fatalf("provider hits = %v; want exactly [detector]", rec.hits)
	}
	if len(rec.attempts[230]) == 0 {
		t.Fatalf("lane attempts for item 230 = %v; want the dispatch's per-lane attribution recorded", rec.attempts[230])
	}
}

// TestAllLanesUnavailableConsultsDetectorLane verifies the pre-dequeue
// availability gate tests the EFFECTIVE lane set, not w.lanes alone. w.lanes
// tracks only the provider lanes, so once every provider breaker is open the
// worker would idle even with a healthy detector lane that could still settle
// items - worst under ordering=front, whose whole purpose is settling a
// high-confidence instrumental with zero provider requests.
func TestAllLanesUnavailableConsultsDetectorLane(t *testing.T) {
	det := &fakeDetector{instrumental: true, version: "9.9.9"}
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetDetectorOrdering("front")

	// Open every PROVIDER breaker. The detector lane's own breaker stays closed.
	for _, l := range w.lanes {
		l.Breaker().Trip()
	}

	if w.allLanesUnavailable() {
		t.Fatal("worker must not idle while a healthy detector lane can still settle items")
	}

	// With the detector lane ALSO open, there is genuinely nothing to run.
	w.detectorLane.Breaker().Trip()
	if !w.allLanesUnavailable() {
		t.Fatal("with every lane open, including the detector, the worker must idle")
	}
}

// TestAllLanesUnavailableIgnoresDetectorUnderParallelMode confirms the gate does
// not resurrect the detector under parallel dispatch, where rebuildOrchestrator
// intentionally declines to install the lane. Without this, the availability fix
// would quietly re-enable the racing behavior the parallel exclusion prevents.
func TestAllLanesUnavailableIgnoresDetectorUnderParallelMode(t *testing.T) {
	det := &fakeDetector{instrumental: true, version: "9.9.9"}
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true)
	w.SetProvidersMode(orchestrator.ModeParallel)

	if w.detectorLane != nil {
		t.Fatal("no detector lane may be installed under parallel mode")
	}
	for _, l := range w.lanes {
		l.Breaker().Trip()
	}
	if !w.allLanesUnavailable() {
		t.Fatal("under parallel mode the gate must ignore the detector and idle on open provider breakers")
	}
}
