package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

func boolPtr(b bool) *bool { return &b }

func detectItem(id int64, detect *bool) queue.WorkItem {
	return queue.WorkItem{
		ID: id,
		Inputs: models.Inputs{
			Track:      models.Track{ArtistName: "Composer", TrackName: "Interlude"},
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: "/music/interlude.flac",
		},
		DetectInstrumental: detect,
	}
}

// TestRunOnce_DetectItemFlagOffSkipsDetection verifies a per-item decision of
// "off" suppresses detection even when the global default is on.
func TestRunOnce_DetectItemFlagOffSkipsDetection(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(300, boolPtr(false))}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNoLyrics}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true) // global on, but item says off

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 0 {
		t.Errorf("detector calls = %v; want none (item opted out)", det.calls)
	}
	if len(q.deferred) != 1 {
		t.Errorf("deferred = %v; want the item deferred as a normal miss", q.deferred)
	}
}

// TestRunOnce_DetectItemFlagOnOverridesDefaultOff verifies a per-item decision of
// "on" runs detection even when the global default is off.
func TestRunOnce_DetectItemFlagOnOverridesDefaultOff(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(301, boolPtr(true))}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.EnableAudioDetector(det)
	// global default left false on purpose

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 1 {
		t.Errorf("detector calls = %v; want 1 (item opted in)", det.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 301 {
		t.Errorf("completed = %v; want [301] (instrumental marker)", q.completed)
	}
}

// TestRunOnce_DetectNilFallsBackToDefaultOff verifies a NULL (nil) per-item
// decision falls back to the global default (here off), preserving the behavior
// of pre-existing rows.
func TestRunOnce_DetectNilFallsBackToDefaultOff(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(302, nil)}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNoLyrics}, &fakeWriter{})
	w.EnableAudioDetector(det)
	// global default false: nil item must not detect

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 0 {
		t.Errorf("detector calls = %v; want none (nil falls back to default off)", det.calls)
	}
}

// TestDetectInstrumental_WantedButNoClassifierLoudSkips verifies that when an
// item requests detection but no classifier is configured, the worker logs an
// error (loud-skip, no silent no-op) and resolves an empty detector path.
//
// This test previously called the now-removed w.detectInstrumental directly.
// detectInstrumental was replaced by detectionEnabledFor (the enable decision,
// unchanged logic) and detectorPathFor (the path-gating + loud-skip logging,
// consulted at the FindLyrics call site instead of via an inline detector
// call) as part of wiring the detector into the orchestrator as a lane
// (#502). This ports the same assertions to the new split.
func TestDetectInstrumental_WantedButNoClassifierLoudSkips(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	// no EnableAudioDetector: classifier unconfigured
	item := detectItem(303, boolPtr(true))
	if !w.detectionEnabledFor(item) {
		t.Fatalf("detectionEnabledFor = false; want true (item opted in)")
	}
	path := w.detectorPathFor(item)
	if path != "" {
		t.Errorf("detectorPathFor = %q; want empty when no classifier configured (loud-skip path)", path)
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "no classifier") {
		t.Errorf("expected an ERROR loud-skip log mentioning the missing classifier; got: %q", logged)
	}
}

// TestDetectorBreakerStateSurvivesRebuild verifies that a rebuild of the
// orchestrator preserves the detector breaker's accumulated state (#531).
//
// Before the fix, rebuildOrchestrator constructed a brand-new circuit.Breaker
// for the detector lane on every call, so any trip count and open-until deadline
// were silently discarded. That was latent while every rebuild-triggering setter
// ran only at startup, but it would let a runtime rebuild (e.g. a settings save
// that re-wires the worker) reset a tripped detector breaker and immediately
// hammer a sidecar that is known to be unreachable.
//
// The assertion is on the breaker's OBSERVABLE state rather than on pointer
// identity: reusing the instance and transferring its state are both valid
// fixes, and the invariant that matters is that a tripped detector stays
// tripped across a rebuild.
func TestDetectorBreakerStateSurvivesRebuild(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(&fakeDetector{})
	w.SetDetectorOrdering("front")

	lane := w.detectorLane
	if lane == nil {
		t.Fatal("detector lane not installed; cannot exercise the rebuild invariant")
	}

	// Trip the detector breaker so there is state worth preserving.
	lane.Breaker().Trip()
	trips := lane.Breaker().Trips()
	if trips == 0 {
		t.Fatal("breaker reports 0 trips after Trip(); test cannot discriminate")
	}
	if got := lane.Breaker().Allow(); got != circuit.StateOpen {
		t.Fatalf("breaker state after Trip() = %v; want StateOpen", got)
	}

	// Any rebuild-triggering setter will do; ordering is the cheapest.
	w.SetDetectorOrdering("demoted")

	rebuilt := w.detectorLane
	if rebuilt == nil {
		t.Fatal("detector lane missing after rebuild")
	}
	if got := rebuilt.Breaker().Trips(); got != trips {
		t.Errorf("trips after rebuild = %d; want %d (breaker state was discarded)", got, trips)
	}
	if got := rebuilt.Breaker().Allow(); got != circuit.StateOpen {
		t.Errorf("breaker state after rebuild = %v; want StateOpen (a tripped detector must stay tripped)", got)
	}
}

// TestCircuitSettersReachDetectorBreaker verifies the circuit-config setters fan
// out to the detector breaker (#531).
//
// This became load-bearing only once the breaker started surviving rebuilds.
// Previously each rebuild constructed a fresh breaker from the worker's current
// config, so a setter that skipped it was invisible. A long-lived breaker would
// instead keep whatever window it was born with, forever.
func TestCircuitSettersReachDetectorBreaker(t *testing.T) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	w.EnableAudioDetector(&fakeDetector{})
	w.SetDetectorOrdering("front")
	if w.detectorBreaker == nil {
		t.Fatal("detector breaker not created; cannot exercise the fan-out")
	}

	// A frozen clock proves the setter reached the breaker: the breaker's
	// open-until deadline is computed from ITS clock, so if setClock skipped it
	// the deadline would be built from the real wall clock instead.
	frozen := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return frozen })
	// Order matters: SetCircuitBackoff runs FIRST with a distinct (smaller) cap,
	// so SetCircuitOpenDuration is the only call that can raise the window to
	// 90m. Setting them the other way round -- or with the same cap -- lets
	// SetCircuitBackoff reapply the expected value and masks a missing
	// SetCircuitOpenDuration fan-out entirely.
	w.SetCircuitBackoff(90*time.Minute, 30*time.Minute)
	w.SetCircuitOpenDuration(90 * time.Minute)

	w.detectorBreaker.Trip()
	got := w.detectorBreaker.OpenUntil()
	want := frozen.Add(90 * time.Minute)
	if !got.Equal(want) {
		t.Errorf("detector breaker OpenUntil = %v; want %v (a circuit setter did not reach it)", got, want)
	}
}
