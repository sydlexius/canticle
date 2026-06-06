package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/verification"
)

// fakeQueue models DBQueue's status transitions for tests. Dequeue moves an
// item out of the pending pool and into processing; Complete/Fail/Release
// remove it from processing. Release additionally records the ID so tests
// can assert that an item was returned to the pending pool without a failure
// being recorded against it.
type fakeQueue struct {
	items          []queue.WorkItem
	processing     []queue.WorkItem
	completed      []int64
	failed         []int64
	released       []int64
	deferred       []int64
	failCauses     []error
	deferCauses    []error
	deferDurations []time.Duration
	completeErr    error
	failErr        error
	deferErr       error
	releaseErr     error
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
	album  string
	lyrics string
}

type fakeCache struct {
	exact    string
	fallback string
	err      error
	stores   []cacheStore
}

func (c *fakeCache) LookupExact(context.Context, string, string, string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.exact == "" {
		return "", sql.ErrNoRows
	}
	return c.exact, nil
}

func (c *fakeCache) LookupFallback(context.Context, string, string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.fallback == "" {
		return "", sql.ErrNoRows
	}
	return c.fallback, nil
}

func (c *fakeCache) Store(_ context.Context, artist, title, album, lyrics string) error {
	c.stores = append(c.stores, cacheStore{artist: artist, title: title, album: album, lyrics: lyrics})
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

type fakeWriter struct {
	writes []models.OutputPath
	err    error
}

func (w *fakeWriter) WriteLRC(_ models.Song, filename string, outdir string) error {
	w.writes = append(w.writes, models.OutputPath{Outdir: outdir, Filename: filename})
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
	cache := &fakeCache{exact: cached}
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
	if store.artist != track.ArtistName || store.title != track.TrackName || store.album != track.AlbumName {
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
			if len(q.deferDurations) != 1 || q.deferDurations[0] != benignMissCooldown {
				t.Fatalf("defer cooldown = %v; want fixed %v", q.deferDurations, benignMissCooldown)
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
			w.now = func() time.Time { return fixed }
			w.SetCircuitOpenDuration(30 * time.Minute)

			// First call dequeues, hits sentinel, opens circuit.
			if err := w.RunOnce(context.Background()); err != nil {
				if !errors.Is(err, errQueueEmpty) {
					t.Fatalf("RunOnce: %v; want nil or errQueueEmpty", err)
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
			if w.circuitOpenUntil.IsZero() {
				t.Fatal("circuitOpenUntil = zero; want circuit opened")
			}
			if got, want := w.circuitOpenUntil, fixed.Add(30*time.Minute); !got.Equal(want) {
				t.Fatalf("circuitOpenUntil = %v; want %v", got, want)
			}

			// Subsequent call must skip dequeue entirely while circuit open.
			callsBefore := fetcher.calls
			itemsBefore := len(q.items)
			err := w.RunOnce(context.Background())
			if !errors.Is(err, errQueueEmpty) {
				t.Fatalf("RunOnce while open = %v; want errQueueEmpty", err)
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
			w.now = func() time.Time { return fixed.Add(31 * time.Minute) }
			err = w.RunOnce(context.Background())
			if err != nil && !errors.Is(err, errQueueEmpty) {
				t.Fatalf("RunOnce after window = %v; want nil or errQueueEmpty", err)
			}
			if fetcher.calls == callsBefore {
				t.Fatalf("fetcher.calls = %d; want >%d after circuit closed", fetcher.calls, callsBefore)
			}
		})
	}
}

func TestRunOnceWithOpenCircuitDoesNotIncrementBackoff(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return fixed }
	w.circuitOpenUntil = fixed.Add(10 * time.Minute)

	if err := w.RunOnce(context.Background()); !errors.Is(err, errQueueEmpty) {
		t.Fatalf("RunOnce = %v; want errQueueEmpty", err)
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
	if errors.Is(err, errQueueEmpty) {
		t.Fatalf("RunOnce returned errQueueEmpty; want a real error so the outer loop can react. got %v", err)
	}
	if !errors.Is(err, q.releaseErr) {
		t.Fatalf("RunOnce error %v; want errors.Is(_, releaseErr) so the cause is preserved", err)
	}
	// Circuit must still be opened even though release failed; we want the
	// quiet window applied to upstream while operators investigate the
	// orphaned row.
	if w.circuitOpenUntil.IsZero() {
		t.Fatal("circuitOpenUntil = zero; want circuit opened despite release failure")
	}
	if len(q.failed) != 0 {
		t.Fatalf("failed = %v; want none on circuit-open trip even when release fails", q.failed)
	}
}

type fakeCacheToggle struct {
	hits    []bool
	payload string
	idx     int
}

func (c *fakeCacheToggle) LookupExact(context.Context, string, string, string) (string, error) {
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

func (c *fakeCacheToggle) LookupFallback(context.Context, string, string) (string, error) {
	return "", sql.ErrNoRows
}

func (c *fakeCacheToggle) Store(context.Context, string, string, string, string) error {
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
