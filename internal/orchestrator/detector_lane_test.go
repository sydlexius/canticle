package orchestrator

import (
	"context"
	"errors"
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
}

func (s *stubDetector) Detect(_ context.Context, audioPath string) (detector.Result, error) {
	s.got = audioPath
	return s.res, s.err
}

func TestDetectorLane_GatePositiveIsSuitableWithTelemetry(t *testing.T) {
	d := &stubDetector{res: detector.Result{
		Instrumental: true, Confidence: 0.9, VocalConfidence: 0.01,
		SpeechConfidence: 0.02, WinningVocalClass: "Singing", Version: "1.5.0",
	}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour))
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
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour))
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if ClassifyOutcome(err) != OutcomeBenignMiss {
		t.Fatalf("gate-negative outcome = %v, want OutcomeBenignMiss", ClassifyOutcome(err))
	}
}

func TestDetectorLane_EmptyPathIsBenignMiss(t *testing.T) {
	d := &stubDetector{res: detector.Result{Instrumental: true, Version: "1.5.0"}}
	lane := NewDetectorLane(d, circuit.New(time.Minute, time.Hour))
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "")
	if ClassifyOutcome(err) != OutcomeBenignMiss {
		t.Fatalf("empty-path outcome = %v, want OutcomeBenignMiss", ClassifyOutcome(err))
	}
	if d.got != "" {
		t.Fatal("detector must not be called with an empty path")
	}
}

func TestDetectorClassifier_OtherErrorWrapsAndLeavesBreakerUntouched(t *testing.T) {
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(&stubDetector{}, br)
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

func TestDetectorLane_OutageTripsBreaker(t *testing.T) {
	d := &stubDetector{err: errors.New("connection refused")}
	br := circuit.New(time.Minute, time.Hour)
	lane := NewDetectorLane(d, br)
	_, err := lane.FindLyrics(context.Background(), models.Track{}, "/music/x.flac")
	if ClassifyOutcome(err) != OutcomeTransport {
		t.Fatalf("outage outcome = %v, want OutcomeTransport", ClassifyOutcome(err))
	}
	if !errors.Is(err, ErrLaneOutage) {
		t.Fatalf("outage error must wrap ErrLaneOutage: %v", err)
	}
	if br.Trips() == 0 {
		t.Fatal("a detector outage must trip the breaker")
	}
}
