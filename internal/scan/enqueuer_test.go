package scan_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"
)

func TestResultInputs(t *testing.T) {
	in, err := scan.ResultInputs(models.ScanResult{
		ID:       4,
		FilePath: "/music/Artist/song.flac",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
	})
	if err != nil {
		t.Fatalf("ResultInputs: %v", err)
	}
	if in.SourcePath != "/music/Artist/song.flac" {
		t.Errorf("SourcePath = %q; want the file path", in.SourcePath)
	}
	if in.Outdir != "/music/Artist" {
		t.Errorf("Outdir = %q; want the file's directory", in.Outdir)
	}
	if in.Filename != "song.lrc" {
		t.Errorf("Filename = %q; want song.lrc", in.Filename)
	}
	if in.ScanResultID != 4 {
		t.Errorf("ScanResultID = %d; want 4", in.ScanResultID)
	}
	if _, err := scan.ResultInputs(models.ScanResult{}); err == nil {
		t.Error("ResultInputs with empty result = nil error; want failure")
	}
}

type fakePendingStore struct {
	results   []models.ScanResult
	status    []statusCall
	libraryID int64
	err       error
}

type statusCall struct {
	ids    []int64
	status string
}

func (f *fakePendingStore) ListPendingByLibrary(_ context.Context, libraryID int64) ([]models.ScanResult, error) {
	f.libraryID = libraryID
	if f.err != nil {
		return nil, f.err
	}
	return append([]models.ScanResult(nil), f.results...), nil
}

func (f *fakePendingStore) SetStatus(_ context.Context, ids []int64, status string) error {
	cp := append([]int64(nil), ids...)
	f.status = append(f.status, statusCall{ids: cp, status: status})
	return nil
}

type fakeLyricsCache struct {
	hits map[string]bool
	err  error
}

func (f fakeLyricsCache) LookupFallback(_ context.Context, artist string, title string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.hits[artist+"\x00"+title] {
		return "[00:00.00]cached", nil
	}
	return "", sql.ErrNoRows
}

type fakeWorkQueue struct {
	inputs     []models.Inputs
	priorities []int
	before     func()
	err        error
}

func (f *fakeWorkQueue) Enqueue(_ context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error) {
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return queue.WorkItem{}, f.err
	}
	f.inputs = append(f.inputs, inputs)
	f.priorities = append(f.priorities, priority)
	return queue.WorkItem{ID: int64(len(f.inputs)), Inputs: inputs, Priority: priority}, nil
}

func TestEnqueuer_EnqueuePendingSkipsCacheHitsAndEnqueuesMisses(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{
		{
			ID: 1,
			Track: models.Track{
				ArtistName: "Cached",
				TrackName:  "Song",
			},
			Outdir:   "/music",
			Filename: "cached.lrc",
		},
		{
			ID:       2,
			FilePath: "/music/missing.mp3",
			Track: models.Track{
				ArtistName: "Missing",
				TrackName:  "Song",
			},
		},
	}}
	cache := fakeLyricsCache{hits: map[string]bool{"Cached\x00Song": true}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: cache, Queue: work, Priority: 5}

	if err := e.EnqueuePending(ctx, 7); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if len(work.inputs) != 1 {
		t.Fatalf("enqueued inputs = %+v; want 1 miss", work.inputs)
	}
	got := work.inputs[0]
	if got.Track.ArtistName != "Missing" || got.OutputPaths[0].Filename != "missing.lrc" {
		t.Fatalf("enqueued input = %+v; want Missing/missing.lrc", got)
	}
	if got.SourcePath != "/music/missing.mp3" {
		t.Fatalf("source path = %q; want scan result file path", got.SourcePath)
	}
	if len(work.priorities) != 1 || work.priorities[0] != 5 {
		t.Fatalf("enqueued priorities = %+v; want [5]", work.priorities)
	}
	if len(store.status) != 2 {
		t.Fatalf("status calls = %+v; want done and processing calls", store.status)
	}
	if store.status[0].status != scan.StatusDone || len(store.status[0].ids) != 1 || store.status[0].ids[0] != 1 {
		t.Fatalf("first status call = %+v; want cache hit done", store.status[0])
	}
	if store.status[1].status != scan.StatusProcessing || len(store.status[1].ids) != 1 || store.status[1].ids[0] != 2 {
		t.Fatalf("second status call = %+v; want miss processing", store.status[1])
	}
}

func TestEnqueuer_ReservesMissBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       3,
		FilePath: "/music/reserved.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
	}}}
	cache := fakeLyricsCache{hits: map[string]bool{}}
	var reservedBeforeEnqueue bool
	work := &fakeWorkQueue{before: func() {
		reservedBeforeEnqueue = len(store.status) == 1 &&
			store.status[0].status == scan.StatusProcessing &&
			len(store.status[0].ids) == 1 &&
			store.status[0].ids[0] == 3
	}}
	e := scan.Enqueuer{Results: store, Cache: cache, Queue: work}

	if err := e.EnqueuePending(ctx, 7); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if !reservedBeforeEnqueue {
		t.Fatalf("status calls before enqueue = %+v; want processing reservation", store.status)
	}
}

func TestEnqueuer_RestoresPendingWhenEnqueueFails(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       4,
		FilePath: "/music/failure.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
	}}}
	cache := fakeLyricsCache{hits: map[string]bool{}}
	queueErr := errors.New("queue failed")
	work := &fakeWorkQueue{err: queueErr}
	e := scan.Enqueuer{Results: store, Cache: cache, Queue: work}

	err := e.EnqueuePending(ctx, 7)
	if !errors.Is(err, queueErr) {
		t.Fatalf("EnqueuePending error = %v; want wrapping %v", err, queueErr)
	}
	if len(store.status) != 2 {
		t.Fatalf("status calls = %+v; want reserve and restore", store.status)
	}
	if store.status[0].status != scan.StatusProcessing || store.status[1].status != scan.StatusPending {
		t.Fatalf("status calls = %+v; want processing then pending", store.status)
	}
}

func TestEnqueuer_RestoresPendingWhenScanResultDestinationInvalid(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:    5,
		Track: models.Track{ArtistName: "Artist", TrackName: "Title"},
	}}}
	cache := fakeLyricsCache{hits: map[string]bool{}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: cache, Queue: work}

	err := e.EnqueuePending(ctx, 7)
	if err == nil {
		t.Fatal("EnqueuePending returned nil error; want invalid destination error")
	}
	if len(work.inputs) != 0 {
		t.Fatalf("enqueued inputs = %+v; want none", work.inputs)
	}
	if len(store.status) != 2 {
		t.Fatalf("status calls = %+v; want reserve and restore", store.status)
	}
	if store.status[0].status != scan.StatusProcessing || store.status[1].status != scan.StatusPending {
		t.Fatalf("status calls = %+v; want processing then pending", store.status)
	}
}

func TestEnqueuer_OnScanCompleteUsesLibraryID(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{}
	cache := fakeLyricsCache{hits: map[string]bool{}}
	work := &fakeWorkQueue{}
	e := scan.Enqueuer{Results: store, Cache: cache, Queue: work}

	if err := e.OnScanComplete(ctx, models.Library{ID: 9}, nil); err != nil {
		t.Fatalf("OnScanComplete: %v", err)
	}
	if store.libraryID != 9 {
		t.Fatalf("libraryID = %d; want 9", store.libraryID)
	}
	if len(store.status) != 0 {
		t.Fatalf("status calls = %+v; want no status calls", store.status)
	}
}

func TestEnqueuer_PropagatesCacheAndQueueErrors(t *testing.T) {
	ctx := context.Background()
	results := []models.ScanResult{{
		ID:       1,
		FilePath: "/music/error.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
	}}
	cacheErr := errors.New("cache failed")
	queueErr := errors.New("queue failed")

	tests := []struct {
		name  string
		cache fakeLyricsCache
		queue *fakeWorkQueue
		want  error
	}{
		{
			name:  "cache",
			cache: fakeLyricsCache{err: cacheErr},
			queue: &fakeWorkQueue{},
			want:  cacheErr,
		},
		{
			name:  "queue",
			cache: fakeLyricsCache{hits: map[string]bool{}},
			queue: &fakeWorkQueue{err: queueErr},
			want:  queueErr,
		},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			store := &fakePendingStore{results: results}
			e := scan.Enqueuer{Results: store, Cache: v.cache, Queue: v.queue}
			err := e.EnqueuePending(ctx, 1)
			if !errors.Is(err, v.want) {
				t.Fatalf("EnqueuePending error = %v; want wrapping %v", err, v.want)
			}
		})
	}
}
