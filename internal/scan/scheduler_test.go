package scan_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"
	"github.com/sydlexius/mxlrcgo-svc/internal/scanner"
)

type fakeLibraries struct {
	libs []models.Library
	err  error
}

func (f fakeLibraries) List(context.Context) ([]models.Library, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.libs, nil
}

type fakeResults struct {
	calls []upsertCall
	err   error
}

type upsertCall struct {
	libraryID int64
	results   []models.ScanResult
	opts      scan.UpsertOptions
}

func (f *fakeResults) Upsert(_ context.Context, libraryID int64, results []models.ScanResult, opts scan.UpsertOptions) error {
	cp := append([]models.ScanResult(nil), results...)
	f.calls = append(f.calls, upsertCall{libraryID: libraryID, results: cp, opts: opts})
	if f.err != nil {
		return f.err
	}
	return nil
}

type fakeScanner struct {
	results []models.ScanResult
	err     error
}

func (f fakeScanner) ScanLibrary(context.Context, string, scanner.ScanOptions) ([]models.ScanResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func TestScheduler_RunOncePersistsAndCallsCallback(t *testing.T) {
	ctx := context.Background()
	store := &fakeResults{}
	called := false
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/a.mp3",
			Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		}}},
		OnScanComplete: func(_ context.Context, lib models.Library, results []models.ScanResult) error {
			called = true
			if lib.ID != 7 {
				t.Errorf("callback lib ID = %d; want 7", lib.ID)
			}
			if len(results) != 1 || results[0].LibraryID != 7 {
				t.Errorf("callback results = %+v; want library ID 7", results)
			}
			return nil
		},
	}

	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("Upsert calls = %d; want 1", len(store.calls))
	}
	if store.calls[0].libraryID != 7 {
		t.Errorf("Upsert libraryID = %d; want 7", store.calls[0].libraryID)
	}
	if len(store.calls[0].results) != 1 || store.calls[0].results[0].Status != scan.StatusPending {
		t.Errorf("Upsert results = %+v; want one pending result", store.calls[0].results)
	}
	if !called {
		t.Fatal("OnScanComplete was not called")
	}
}

func TestScheduler_PassesForceStatusOnUpdateOrUpgrade(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		opts scanner.ScanOptions
		want bool
	}{
		{name: "default", opts: scanner.ScanOptions{}, want: false},
		{name: "update", opts: scanner.ScanOptions{Update: true}, want: true},
		{name: "upgrade", opts: scanner.ScanOptions{Upgrade: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeResults{}
			s := scan.Scheduler{
				Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
				Results:   store,
				Scanner:   fakeScanner{results: []models.ScanResult{{FilePath: "/music/a.mp3"}}},
				Options:   tc.opts,
			}
			if err := s.RunOnce(ctx); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(store.calls) != 1 {
				t.Fatalf("Upsert calls = %d; want 1", len(store.calls))
			}
			if got := store.calls[0].opts.ForceStatus; got != tc.want {
				t.Fatalf("ForceStatus = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestScheduler_RunWithNoIntervalRunsOnce(t *testing.T) {
	ctx := context.Background()
	store := &fakeResults{}
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/a.mp3",
		}}},
	}

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("Upsert calls = %d; want 1", len(store.calls))
	}
}

func TestScheduler_RunOnceReturnsContextErrorOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeResults{}
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/a.mp3",
		}}},
	}

	cancel()
	if err := s.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v; want context.Canceled", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("Upsert calls = %d; want 0", len(store.calls))
	}
}

func TestScheduler_RunReturnsContextErrorOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeResults{}
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/a.mp3",
		}}},
		Interval: time.Hour,
	}

	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v; want context.Canceled", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("Upsert calls = %d; want 0", len(store.calls))
	}
}

func TestScheduler_RunOnceReturnsContextErrorBetweenLibraries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeResults{}
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{
			{ID: 7, Path: "/music/a", Name: "A"},
			{ID: 8, Path: "/music/b", Name: "B"},
		}},
		Results: store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/a.mp3",
		}}},
		OnScanComplete: func(context.Context, models.Library, []models.ScanResult) error {
			cancel()
			return nil
		},
	}

	if err := s.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v; want context.Canceled", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("Upsert calls = %d; want 1", len(store.calls))
	}
	if store.calls[0].libraryID != 7 {
		t.Fatalf("Upsert libraryID = %d; want 7", store.calls[0].libraryID)
	}
}

func TestScheduler_RunOnceRequiresDependencies(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		s    scan.Scheduler
	}{
		{name: "missing libraries", s: scan.Scheduler{}},
		{name: "missing results", s: scan.Scheduler{Libraries: fakeLibraries{}}},
		{name: "missing scanner", s: scan.Scheduler{Libraries: fakeLibraries{}, Results: &fakeResults{}}},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			if err := v.s.RunOnce(ctx); err == nil {
				t.Fatal("RunOnce returned nil error; want dependency error")
			}
		})
	}
}

func TestScheduler_RunOnceForScansOnlyProvidedLibraries(t *testing.T) {
	ctx := context.Background()
	store := &fakeResults{}
	// fakeLibraries.List would return all three; passing a subset to
	// RunOnceFor must skip the unselected ones entirely.
	all := []models.Library{
		{ID: 7, Path: "/music/a", Name: "A"},
		{ID: 8, Path: "/music/b", Name: "B"},
		{ID: 9, Path: "/music/c", Name: "C"},
	}
	s := scan.Scheduler{
		Libraries: fakeLibraries{libs: all},
		Results:   store,
		Scanner: fakeScanner{results: []models.ScanResult{{
			FilePath: "/music/x.mp3",
			Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		}}},
	}

	if err := s.RunOnceFor(ctx, []models.Library{all[0], all[2]}); err != nil {
		t.Fatalf("RunOnceFor: %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("Upsert calls = %d; want 2 (one per selected library)", len(store.calls))
	}
	gotIDs := []int64{store.calls[0].libraryID, store.calls[1].libraryID}
	wantIDs := []int64{7, 9}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("Upsert call %d libraryID = %d; want %d", i, gotIDs[i], wantIDs[i])
		}
	}
}

func TestScheduler_RunOncePropagatesErrors(t *testing.T) {
	ctx := context.Background()
	listErr := errors.New("list failed")
	scanErr := errors.New("scan failed")
	storeErr := errors.New("store failed")
	callbackErr := errors.New("callback failed")

	tests := []struct {
		name string
		s    scan.Scheduler
		want error
	}{
		{
			name: "list",
			s: scan.Scheduler{
				Libraries: fakeLibraries{err: listErr},
				Results:   &fakeResults{},
				Scanner:   fakeScanner{},
			},
			want: listErr,
		},
		{
			name: "scan",
			s: scan.Scheduler{
				Libraries: fakeLibraries{libs: []models.Library{{ID: 1, Path: "/music"}}},
				Results:   &fakeResults{},
				Scanner:   fakeScanner{err: scanErr},
			},
			want: scanErr,
		},
		{
			name: "store",
			s: scan.Scheduler{
				Libraries: fakeLibraries{libs: []models.Library{{ID: 1, Path: "/music"}}},
				Results:   &fakeResults{err: storeErr},
				Scanner: fakeScanner{results: []models.ScanResult{{
					FilePath: "/music/a.mp3",
				}}},
			},
			want: storeErr,
		},
		{
			name: "callback",
			s: scan.Scheduler{
				Libraries: fakeLibraries{libs: []models.Library{{ID: 1, Path: "/music"}}},
				Results:   &fakeResults{},
				Scanner: fakeScanner{results: []models.ScanResult{{
					FilePath: "/music/a.mp3",
				}}},
				OnScanComplete: func(context.Context, models.Library, []models.ScanResult) error {
					return callbackErr
				},
			},
			want: callbackErr,
		},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			err := v.s.RunOnce(ctx)
			if !errors.Is(err, v.want) {
				t.Fatalf("RunOnce error = %v; want wrapping %v", err, v.want)
			}
		})
	}
}
