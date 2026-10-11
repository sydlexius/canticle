package worker

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/detector"
)

// fakeStoredDecider is a test detector that implements storedDecider: it counts
// Detect calls, reports a fixed model version, and re-decides stored scores via a
// swappable decide func so a test can simulate a threshold recalibration without
// a new inference.
type fakeStoredDecider struct {
	version   string
	detectRes detector.Result
	detectErr error
	detectN   int
	decideFn  func(musicSum, vocalPeak, speechMean float64) bool
}

func (f *fakeStoredDecider) Detect(_ context.Context, _ string) (detector.Result, error) {
	f.detectN++
	if f.detectErr != nil {
		return detector.Result{}, f.detectErr
	}
	return f.detectRes, nil
}

func (f *fakeStoredDecider) ModelVersion() string { return f.version }

func (f *fakeStoredDecider) DecideStored(musicSum, vocalPeak, speechMean float64) bool {
	if f.decideFn != nil {
		return f.decideFn(musicSum, vocalPeak, speechMean)
	}
	return false
}

// plainDetector implements only detector.Detector (not storedDecider), so the
// memo can never reuse and must always infer.
type plainDetector struct{ detectN int }

func (p *plainDetector) Detect(_ context.Context, _ string) (detector.Result, error) {
	p.detectN++
	return detector.Result{Instrumental: true, Version: "v1"}, nil
}

func telemetryV1() *storedTelemetry {
	return &storedTelemetry{Version: "v1", MusicSum: 0.9, VocalPeak: 0.02, SpeechMean: 0.01, VocalClass: "Singing", GateComplete: true}
}

func TestMemoDetectorReusesStoredScoresOnVersionMatch(t *testing.T) {
	inner := &fakeStoredDecider{version: "v1", decideFn: func(_, _, _ float64) bool { return true }}
	m := newMemoDetector(inner)
	m.prime(telemetryV1())

	res, err := m.Detect(context.Background(), "/music/a.flac")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 0 {
		t.Fatalf("inner Detect called %d times; want 0 (reuse)", inner.detectN)
	}
	if !res.Instrumental {
		t.Errorf("Instrumental = false; want true (DecideStored)")
	}
	if res.Confidence != 0.9 || res.VocalConfidence != 0.02 || res.SpeechConfidence != 0.01 {
		t.Errorf("reconstructed scores = %+v; want stored telemetry", res)
	}
	if res.Version != "v1" {
		t.Errorf("Version = %q; want v1", res.Version)
	}
	if !res.Reusable {
		t.Errorf("Reusable = false; a reused gate-complete row must carry the bit forward so a later stamp keeps it")
	}
	last, ran := m.lastInference()
	if ran {
		t.Errorf("lastInference ran = true; want false (reuse)")
	}
	if !last.Instrumental {
		t.Errorf("lastInference result not carried")
	}
}

func TestMemoDetectorInfersOnVersionMismatch(t *testing.T) {
	inner := &fakeStoredDecider{version: "v2", detectRes: detector.Result{Instrumental: false, Version: "v2"}}
	m := newMemoDetector(inner)
	m.prime(telemetryV1()) // stored under old version

	if _, err := m.Detect(context.Background(), "/music/a.flac"); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 1 {
		t.Fatalf("inner Detect called %d times; want 1 (model changed)", inner.detectN)
	}
	_, ran := m.lastInference()
	if !ran {
		t.Errorf("lastInference ran = false; want true (fresh inference)")
	}
}

func TestMemoDetectorInfersWhenNotPrimed(t *testing.T) {
	inner := &fakeStoredDecider{version: "v1", detectRes: detector.Result{Instrumental: true, Version: "v1"}}
	m := newMemoDetector(inner)
	m.prime(nil)

	if _, err := m.Detect(context.Background(), "/music/a.flac"); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 1 {
		t.Fatalf("inner Detect called %d times; want 1 (no stored telemetry)", inner.detectN)
	}
}

func TestMemoDetectorReuseFollowsRecalibratedThreshold(t *testing.T) {
	// Same stored scores, same version, but DecideStored now returns true
	// (simulating a threshold recalibration). The verdict must flip with NO new
	// inference.
	inner := &fakeStoredDecider{version: "v1", decideFn: func(_, _, _ float64) bool { return true }}
	m := newMemoDetector(inner)
	m.prime(telemetryV1())

	res, err := m.Detect(context.Background(), "/music/a.flac")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 0 {
		t.Fatalf("inner Detect called %d times; want 0", inner.detectN)
	}
	if !res.Instrumental {
		t.Errorf("Instrumental = false; want true after recalibration")
	}
}

func TestMemoDetectorAlwaysInfersWithoutStoredDecider(t *testing.T) {
	inner := &plainDetector{}
	m := newMemoDetector(inner)
	m.prime(telemetryV1())

	if _, err := m.Detect(context.Background(), "/music/a.flac"); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 1 {
		t.Fatalf("inner Detect called %d times; want 1 (inner is not a storedDecider)", inner.detectN)
	}
}

func TestMemoDetectorEmptyVersionNeverReuses(t *testing.T) {
	// A detector with no model version cannot key cache validity, so it must
	// always infer even when primed.
	inner := &fakeStoredDecider{version: "", detectRes: detector.Result{Instrumental: true}}
	m := newMemoDetector(inner)
	m.prime(&storedTelemetry{Version: "", MusicSum: 0.9})

	if _, err := m.Detect(context.Background(), "/music/a.flac"); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 1 {
		t.Fatalf("inner Detect called %d times; want 1 (empty version never reuses)", inner.detectN)
	}
}

func TestMemoDetectorPrimeResetsStaleState(t *testing.T) {
	// After a live inference on item A, priming for item B must clear the stale
	// last-inference state: if B never calls Detect (detection skipped), the memo
	// must NOT report A's result as B's. Guards against a cross-item score leak
	// where B's row is stamped with A's telemetry (#582 hostile-review C1).
	inner := &fakeStoredDecider{
		version:   "v1",
		detectRes: detector.Result{Instrumental: false, Version: "v1", Confidence: 0.40, VocalConfidence: 0.72},
	}
	m := newMemoDetector(inner)

	// Item A: live inference.
	m.prime(nil)
	if _, err := m.Detect(context.Background(), "/music/a.flac"); err != nil {
		t.Fatalf("Detect A: %v", err)
	}
	if _, ran := m.lastInference(); !ran {
		t.Fatalf("after A: ran = false; want true")
	}

	// Item B: prime (detection will be skipped, no Detect call).
	m.prime(nil)
	res, ran := m.lastInference()
	if ran {
		t.Errorf("after priming B: ran = true; want false (A's state must be cleared)")
	}
	if res.Version != "" || res.Confidence != 0 {
		t.Errorf("after priming B: stale result leaked: %+v; want zero", res)
	}
}

// A primed row whose gate_complete bit is not true (NULL from before migration
// 070, or 0 from a degraded response) must never be re-decided from its stored
// scores: DecideStored skips the completeness guards, so reuse would settle a
// guard-degraded row instrumental (#511). It must run live inference instead.
func TestMemoDetectorInfersWhenGateNotComplete(t *testing.T) {
	inner := &fakeStoredDecider{
		version:   "v1",
		detectRes: detector.Result{Version: "v1", Reusable: true},
		decideFn:  func(_, _, _ float64) bool { return true },
	}
	m := newMemoDetector(inner)
	tel := telemetryV1()
	tel.GateComplete = false
	m.prime(tel)

	res, err := m.Detect(context.Background(), "/music/a.flac")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inner.detectN != 1 {
		t.Fatalf("inner Detect called %d times; want 1 (gate bit not set forces live inference)", inner.detectN)
	}
	if res.Instrumental {
		t.Errorf("Instrumental = true; the stored scores must not have been re-decided")
	}
	if _, ran := m.lastInference(); !ran {
		t.Errorf("lastInference ran = false; want true")
	}
}
