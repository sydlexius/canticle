package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/models"
)

type stubDetector struct {
	res detector.Result
	err error
	got string
	// calls counts Detect invocations. It exists because got alone cannot prove
	// Detect was NOT called: got starts empty, so a skipped call and a call made
	// with an empty path are indistinguishable by got - both leave it "". Only a
	// counter separates them.
	calls int
}

func (s *stubDetector) Detect(_ context.Context, audioPath string) (detector.Result, error) {
	s.calls++
	s.got = audioPath
	return s.res, s.err
}

func TestDetectorLane_GatePositiveIsSuitableWithTelemetry(t *testing.T) {
	d := &stubDetector{res: detector.Result{
		Instrumental: true, Confidence: 0.9, VocalConfidence: 0.01,
		SpeechConfidence: 0.02, WinningVocalClass: "Singing", Version: "1.5.0",
	}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)
	song, err := lane.FindLyrics(context.Background(), models.Track{TrackName: "x"}, "/music/x.flac")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if d.got != "/music/x.flac" {
		t.Fatalf("detector got path %q", d.got)
	}
	if song.Track.Instrumental != 1 || song.DetectorVersion != "1.5.0" {
		t.Fatalf("song = %+v", song)
	}
	if song.DetectorMusicSum != 0.9 || song.DetectorVocalPeak != 0.01 ||
		song.DetectorSpeechMean != 0.02 || song.DetectorVocalClass != "Singing" {
		t.Fatalf("telemetry not carried: %+v", song)
	}
	if !IsSuitable(song, nil) {
		t.Fatal("gate-positive detector song must be terminal-suitable")
	}
}

func TestDetectorLane_GateNegativeIsBenignMiss(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: false, Version: "1.5.0"}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if ClassifyOutcome(err) != OutcomeBenignMiss {
		t.Fatalf("gate-negative outcome = %v, want OutcomeBenignMiss", ClassifyOutcome(err))
	}
}

func TestDetectorLane_EmptyPathIsBenignMiss(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: true, Version: "1.5.0"}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "")
	if ClassifyOutcome(err) != OutcomeBenignMiss {
		t.Fatalf("empty-path outcome = %v, want OutcomeBenignMiss", ClassifyOutcome(err))
	}
	if d.calls != 0 {
		t.Fatalf("detector must not be invoked at all for an empty path, got %d calls", d.calls)
	}
}

func TestDetectorClassifier_OtherErrorWrapsAndLeavesBreakerUntouched(t *testing.T) {
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(&stubDetector{}, br, nil)
	cause := errors.New("unexpected decode failure")

	wrapped := detectorClassifier(lane, cause)

	if !errors.Is(wrapped, cause) {
		t.Fatalf("wrapped error must wrap the cause: %v", wrapped)
	}
	if errors.Is(wrapped, ErrLaneBenignMiss) || errors.Is(wrapped, ErrLaneOutage) {
		t.Fatalf("an unrelated error must not be reclassified as benign-miss or outage: %v", wrapped)
	}
	if br.Trips() != 0 {
		t.Fatalf("an unrelated error must not trip the breaker, got %d trips", br.Trips())
	}
	if br.Allow() != circuit.StateClosed {
		t.Fatalf("breaker must remain closed for an unrelated error, got %v", br.Allow())
	}
}

// stubPacer is a minimal providers.AdaptivePacer recording notification
// counts, used to prove which lane notifications actually reach a shared
// pacer instance (as opposed to a lane's own mock ratchet).
type stubPacer struct {
	throttles int
	successes int
}

func (p *stubPacer) OnThrottle() { p.throttles++ }
func (p *stubPacer) OnSuccess()  { p.successes++ }

// TestDetectorLane_GatePositiveCreditsSharedPacer is the #550 discriminator: a
// detector settle must credit the SAME pacer instance the primary provider
// lane uses, so the measured production gap (8 of 52 settles were detector
// instrumentals crediting nothing) is closed. Against the pre-#550
// constructor (no pacer parameter, Lane.pacer always nil for a detector lane)
// this fails because OnSuccess is never reachable -- there was no pacer to
// call it on.
func TestDetectorLane_GatePositiveCreditsSharedPacer(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: true, Version: "1.5.0"}}
	pacer := &stubPacer{}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), pacer)

	_, err := lane.FindLyrics(context.Background(), models.Track{TrackName: "x"}, "/music/x.flac")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if pacer.successes != 1 {
		t.Fatalf("pacer.successes = %d; want 1 (a gate-positive detector settle must credit decay)", pacer.successes)
	}
	if pacer.throttles != 0 {
		t.Fatalf("pacer.throttles = %d; want 0 (a detector settle is never a throttle signal)", pacer.throttles)
	}
}

// TestDetectorLane_GateNegativeDoesNotCreditPacer asserts the credit is scoped
// to a genuine gate-positive settle: a benign miss must not fake a success
// signal into the shared pacer, mirroring the provider lane's existing
// "no OnSuccess on a benign miss" rule (lane.go's notifySuccess is reached
// only on the FindLyrics success path).
func TestDetectorLane_GateNegativeDoesNotCreditPacer(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: false, Version: "1.5.0"}}
	pacer := &stubPacer{}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), pacer)

	_, _ = lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if pacer.successes != 0 {
		t.Fatalf("pacer.successes = %d; want 0 (a benign miss must not credit decay)", pacer.successes)
	}
}

// TestDetectorLane_NilPacerIsSafe covers every pre-#550 call site (nil pacer):
// a detector settle must not panic or otherwise misbehave when no pacer is
// wired in.
func TestDetectorLane_NilPacerIsSafe(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: true, Version: "1.5.0"}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)

	if _, err := lane.FindLyrics(context.Background(), models.Track{TrackName: "x"}, "/music/x.flac"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// scriptedDetector returns a scripted sequence of (result, error) pairs, one
// per Detect call, so a test can drive a success-then-failure or repeated
// failure sequence through the lane's ever-reached gate (#567).
type scriptedDetector struct {
	steps []scriptStep
	i     int
}

type scriptStep struct {
	res detector.Result
	err error
}

func (s *scriptedDetector) Detect(_ context.Context, _ string) (detector.Result, error) {
	step := s.steps[s.i]
	if s.i < len(s.steps)-1 {
		s.i++
	}
	return step.res, step.err
}

// notReadyErr mimics the detector's dial-failure wrap: the classifier could not
// be reached at all.
func notReadyErr() error {
	return fmt.Errorf("detector classify: %w: dial tcp: connect: connection refused", detector.ErrClassifierNotReady)
}

// TestDetectorLane_FirstDialErrorIsNotReady: before the lane has ever reached
// the sidecar, a dial failure is a startup race (ErrLaneNotReady), not an
// outage, and does not trip the breaker.
func TestDetectorLane_FirstDialErrorIsNotReady(t *testing.T) {
	d := &scriptedDetector{steps: []scriptStep{{err: notReadyErr()}}}
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(d, br, nil)

	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if !errors.Is(err, ErrLaneNotReady) {
		t.Fatalf("first dial error: got %v, want ErrLaneNotReady", err)
	}
	if errors.Is(err, ErrLaneOutage) {
		t.Fatalf("a boot-race must not be an outage: %v", err)
	}
	if br.Trips() != 0 {
		t.Fatalf("not-ready must not trip the breaker, got %d trips", br.Trips())
	}
}

// TestDetectorLane_DialErrorAfterSuccessIsOutage: once the lane has reached the
// sidecar, a later dial failure means the sidecar was up and then died - a
// genuine outage.
func TestDetectorLane_DialErrorAfterSuccessIsOutage(t *testing.T) {
	d := &scriptedDetector{steps: []scriptStep{
		{res: detector.Result{Instrumental: false, Version: "1.5.0"}}, // reaches sidecar
		{err: notReadyErr()}, // now it died
	}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)

	_, _ = lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")    // success
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac") // died
	if !errors.Is(err, ErrLaneOutage) {
		t.Fatalf("dial error after a success: got %v, want ErrLaneOutage", err)
	}
	if errors.Is(err, ErrLaneNotReady) {
		t.Fatalf("a post-success failure must not be not-ready: %v", err)
	}
}

// TestDetectorLane_DeadFromBootEscalatesAfterBound: a sidecar never reachable
// since boot is not a boot race - after notReadyBound not-ready results the lane
// escalates to a genuine outage so the breaker trips and the lane stops
// re-spending a provider call every cycle.
func TestDetectorLane_DeadFromBootEscalatesAfterBound(t *testing.T) {
	steps := make([]scriptStep, notReadyBound+1)
	for i := range steps {
		steps[i] = scriptStep{err: notReadyErr()}
	}
	d := &scriptedDetector{steps: steps}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)

	for i := range notReadyBound {
		_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
		if !errors.Is(err, ErrLaneNotReady) {
			t.Fatalf("call %d: got %v, want ErrLaneNotReady", i+1, err)
		}
	}
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if !errors.Is(err, ErrLaneOutage) {
		t.Fatalf("after %d not-readies: got %v, want ErrLaneOutage", notReadyBound, err)
	}
}

// TestDetectorClassifier_NotReadyDoesNotTrip: a not-ready (startup race) error
// must leave the breaker CLOSED, so the next work cycle re-attempts the lane
// once the sidecar is up, and must stay matchable as ErrLaneNotReady so the
// worker classifies it as OutcomeLaneNotReady (#567).
func TestDetectorClassifier_NotReadyDoesNotTrip(t *testing.T) {
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(&stubDetector{}, br, nil)
	notReady := fmt.Errorf("detector not ready: %w", errors.Join(ErrLaneNotReady, errors.New("connection refused")))

	got := detectorClassifier(lane, notReady)

	if br.Trips() != 0 {
		t.Fatalf("not-ready must not trip the breaker, got %d trips", br.Trips())
	}
	if br.Allow() != circuit.StateClosed {
		t.Fatalf("breaker must stay closed on not-ready, got %v", br.Allow())
	}
	if !errors.Is(got, ErrLaneNotReady) {
		t.Fatalf("returned error must stay matchable as ErrLaneNotReady: %v", got)
	}
	if ClassifyOutcome(got) != OutcomeLaneNotReady {
		t.Fatalf("classifier output must classify as OutcomeLaneNotReady, got %v", ClassifyOutcome(got))
	}
}

// TestDetectorClassifier_OutageStillTrips guards that the not-ready carve-out
// did not weaken the genuine-outage path.
func TestDetectorClassifier_OutageStillTrips(t *testing.T) {
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(&stubDetector{}, br, nil)
	outage := fmt.Errorf("detector request failed: %w", errors.Join(ErrLaneOutage, errors.New("connection refused")))

	_ = detectorClassifier(lane, outage)

	if br.Trips() == 0 {
		t.Fatal("a genuine outage must still trip the breaker")
	}
}

func TestDetectorLane_OutageTripsBreaker(t *testing.T) {
	d := &stubDetector{err: errors.New("connection refused")}
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(d, br, nil)
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if ClassifyOutcome(err) != OutcomeLaneOutage {
		t.Fatalf("outage outcome = %v, want OutcomeLaneOutage", ClassifyOutcome(err))
	}
	if !errors.Is(err, ErrLaneOutage) {
		t.Fatalf("outage error must wrap ErrLaneOutage: %v", err)
	}
	if br.Trips() == 0 {
		t.Fatal("a detector outage must trip the breaker")
	}
}

// THE LANE ITSELF MUST PRODUCE THE COLLAPSED FORM (#790).
//
// THIS IS THE TEST THAT ACTUALLY GUARDS THE FIX, and its absence was the
// branch's worst defect. The constructor tests below exercise newLaneOutage
// DIRECTLY; they say nothing about whether the lane calls it. Reverting
// detector_lane.go's single delivery line back to
//
//	fmt.Errorf("detector request failed: %w", errors.Join(ErrLaneOutage, err))
//
// left the ENTIRE REPOSITORY green (verified by mutation) -- because every
// pre-existing lane test asserts only errors.Is(err, ErrLaneOutage), which was
// true under the OLD form too: errors.Join unwrapped to the sentinel exactly as
// the new type does. The sentinel-matching assertions can therefore never
// distinguish the fix from the bug, and nothing asserted on the rendered string,
// which is the entire subject of the issue.
//
// So this test drives the REAL lane (not the constructor) and asserts on what it
// RENDERS. It is the only thing on the branch that fails if the fix is reverted.
func TestDetectorLane_RenderedOutageHasNoWrapperChain(t *testing.T) {
	d := &stubDetector{err: errors.New("detector: audio file is missing")}
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(d, br, nil)

	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if err == nil {
		t.Fatal("lane returned no error; want an outage")
	}

	// The classification contract is unchanged -- asserted here too so this test
	// fails loudly if a future edit trades rendering for matchability.
	if !errors.Is(err, ErrLaneOutage) {
		t.Errorf("lane error must still match ErrLaneOutage: %v", err)
	}
	if got := ClassifyOutcome(err); got != OutcomeLaneOutage {
		t.Errorf("ClassifyOutcome = %v, want OutcomeLaneOutage", got)
	}

	// And the rendering -- the part no other test covers.
	got := err.Error()
	if got != "detector: audio file is missing" {
		t.Errorf("lane rendered %q; want exactly the cause, with no wrapper prose", got)
	}
	if strings.Contains(got, "detector request failed") {
		t.Errorf("lane rendered %q; the wrapper prefix is what #790 removes", got)
	}
	if strings.Contains(got, "orchestrator:") {
		t.Errorf("lane rendered %q; the sentinel's text must never reach an operator-facing string", got)
	}
}

// THE WRAPPER CHAIN MUST NOT PRECEDE THE ACTIONABLE FACT (#790). The stored
// reason used to open with roughly 80 characters of pure wrapper --
// "detector request failed: " then the ErrLaneOutage sentinel's own
// "orchestrator: lane outage" -- before anything an operator can act on
// appeared. Each layer named a subsystem; none named the problem.
//
// The sentinel still has to be MATCHABLE (detectorClassifier and
// ClassifyOutcome both key on it via errors.Is), so it cannot simply be
// dropped. What changes is that its TEXT no longer renders: the lane returns an
// error whose message is the cause's own, while still unwrapping to both the
// sentinel and the cause.
func TestDetectorLane_OutageErrorLeadsWithTheCause(t *testing.T) {
	cause := fmt.Errorf("detector: audio file is missing")
	err := newLaneOutage(cause)

	// Still matchable both ways -- this is the load-bearing half.
	if !errors.Is(err, ErrLaneOutage) {
		t.Errorf("errors.Is(err, ErrLaneOutage) = false; the classifier keys on this sentinel")
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false; callers and logs need the underlying failure")
	}
	// And it classifies as it always did.
	if got := ClassifyOutcome(err); got != OutcomeLaneOutage {
		t.Errorf("ClassifyOutcome = %v, want OutcomeLaneOutage", got)
	}

	got := err.Error()
	if got != "detector: audio file is missing" {
		t.Errorf("rendered %q; want exactly the cause, with no wrapper prose", got)
	}
	if strings.Contains(got, "orchestrator:") {
		t.Errorf("rendered %q; the sentinel's text must not reach an operator-facing string", got)
	}
	if strings.Contains(got, "detector request failed") {
		t.Errorf("rendered %q; the wrapper prefix must be gone", got)
	}
}

// A nil cause must not panic or render an empty reason. Defensive: the lane
// only builds this with a non-nil cause today, but an empty last_error is
// indistinguishable from "no error recorded" downstream.
func TestNewLaneOutageWithNilCause(t *testing.T) {
	err := newLaneOutage(nil)
	if !errors.Is(err, ErrLaneOutage) {
		t.Errorf("a nil-cause outage must still match the sentinel")
	}
	if err.Error() == "" {
		t.Errorf("a nil-cause outage rendered empty; an empty reason reads as 'no error recorded'")
	}
}
