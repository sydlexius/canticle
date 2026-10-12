package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
)

// Detector-first under parallel provider dispatch (#528), set through the
// worker setters. Every artist, title and album below is invented.

var parallelFrontTrack = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites"}

// newParallelFrontRig is newTitleRig with a detector, front, parallel dispatch.
func newParallelFrontRig(t *testing.T, track models.Track, instrumental bool) (*titleRig, *fakeDetector) {
	t.Helper()
	r := newTitleRig(t, track)
	det := &fakeDetector{instrumental: instrumental, version: "9.9.9"}
	r.w.EnableAudioDetector(det)
	r.w.SetInstrumentalDetectionDefault(true)
	r.w.SetProvidersMode(orchestrator.ModeParallel)
	r.w.SetDetectorOrdering("front")
	return r, det
}

func wantProviderLyric(t *testing.T, r *titleRig) {
	t.Helper()
	if got, want := r.cols(t, "status", "provider_lane"), "done|musixmatch"; got != want {
		t.Errorf("row = %q; want %q", got, want)
	}
	if !strings.Contains(r.read(t, "track.txt"), titleRuleWords) {
		t.Error("the provider's lyric did not land")
	}
}

func TestParallelFront_GatePositiveMakesNoProviderRequest(t *testing.T) {
	r, det := newParallelFrontRig(t, parallelFrontTrack, true)
	asked := func() { t.Error("a provider was asked although the detector settled the row first") }
	r.primary.onCall, r.secondary.onCall = asked, asked
	r.run(t)
	if len(det.calls) != 1 || r.fetches() != 0 {
		t.Errorf("detector calls = %d, fetcher calls = %d; want 1, 0", len(det.calls), r.fetches())
	}
	if got, want := r.cols(t, "status", "outcome_type", "provider_lane", "instrumental_result"), "done|instrumental|detector|1"; got != want {
		t.Errorf("row = %q; want %q", got, want)
	}
	if strings.Contains(r.read(t, "track.txt"), titleRuleWords) {
		t.Error("the provider's lyric landed instead of the detector's marker")
	}
	if r.w.lastItemContactedProvider {
		t.Error("a pacing wait is owed for a row that contacted no provider")
	}
}

func TestParallelFront_GateNegativeFetchesAndWritesLyric(t *testing.T) {
	r, det := newParallelFrontRig(t, parallelFrontTrack, false)
	r.run(t)
	if len(det.calls) != 1 || r.primary.calls != 1 || r.secondary.calls != 1 {
		t.Errorf("detector calls = %d, primary = %d, fallback = %d; want 1 each", len(det.calls), r.primary.calls, r.secondary.calls)
	}
	wantProviderLyric(t, r)
	if !r.w.lastItemContactedProvider {
		t.Error("no pacing wait owed although both providers were asked")
	}
}

// The verdict is gate-positive, so a detector that ran would settle the row.
func TestParallelFront_DetectorBreakerOpenFallsThrough(t *testing.T) {
	r, det := newParallelFrontRig(t, parallelFrontTrack, true)
	if r.w.detectorLane == nil {
		t.Fatal("no detector lane installed")
	}
	r.w.detectorLane.Breaker().Trip()
	r.run(t)
	if len(det.calls) != 0 || r.primary.calls != 1 {
		t.Errorf("detector calls = %d, primary calls = %d; want 0, 1", len(det.calls), r.primary.calls)
	}
	wantProviderLyric(t, r)
}

// The rows of TestTitleRule_KaraokeSkipsDetectorFirst, dispatched in parallel.
func TestParallelFront_KaraokeTitleSkipsDetector(t *testing.T) {
	for _, tc := range []struct {
		name          string
		track         models.Track
		rule          bool
		detectorCalls int
		lane          string
	}{
		{"karaoke title", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Version)"}, true, 0, "musixmatch"},
		{"karaoke album", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites Karaoke"}, true, 0, "musixmatch"},
		{"rule off", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Version)"}, false, 1, "detector"},
		{"ordinary title", parallelFrontTrack, true, 1, "detector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, det := newParallelFrontRig(t, tc.track, true)
			r.w.SetTitleRule(tc.rule)
			r.run(t)
			if lane := r.cols(t, "provider_lane"); len(det.calls) != tc.detectorCalls || lane != tc.lane {
				t.Errorf("detector calls = %d, lane = %q; want %d, %q", len(det.calls), lane, tc.detectorCalls, tc.lane)
			}
			if body := r.read(t, "track.txt"); (tc.lane == "musixmatch") != strings.Contains(body, titleRuleWords) {
				t.Errorf("track.txt = %q; lane %q", body, tc.lane)
			}
		})
	}
}

func TestParallelFront_CacheHitSkipsDetector(t *testing.T) {
	r, det := newParallelFrontRig(t, parallelFrontTrack, true)
	if err := r.w.store(context.Background(), parallelFrontTrack, r.primary.song); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	r.run(t)
	if len(det.calls) != 0 || r.fetches() != 0 {
		t.Errorf("detector calls = %d, fetcher calls = %d; want 0, 0 (the cache answers before any lane)", len(det.calls), r.fetches())
	}
	if !strings.Contains(r.read(t, "track.txt"), titleRuleWords) {
		t.Error("the cached lyric did not land")
	}
}

// A demoted detector is asked only after a provider (#528), so with every
// provider breaker open the worker idles and touches no row.
func TestParallelDemoted_AllProvidersOpenIdles(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{ID: 7, Inputs: models.Inputs{Track: parallelFrontTrack, SourcePath: "/music/x.flac"}}}}
	det := &fakeDetector{instrumental: true, version: "9.9.9"}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetProvidersMode(orchestrator.ModeParallel)
	for _, l := range w.lanes {
		l.Breaker().Trip()
	}
	if err := w.RunOnce(context.Background()); !errors.Is(err, errLanesUnavailable) {
		t.Fatalf("RunOnce = %v; want errLanesUnavailable", err)
	}
	if len(det.calls) != 0 || len(q.items) != 1 || len(q.released)+len(q.deferred)+len(q.completed) != 0 {
		t.Errorf("detector calls = %d, queued = %d, released = %v, deferred = %v, completed = %v; want the row untouched", len(det.calls), len(q.items), q.released, q.deferred, q.completed)
	}
}
