package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/respdrift"
)

// TestLaneReportsNonDiscriminatingProvider is the wiring assertion for #839.
// stubProvider returns its fixed song regardless of the requested track, which
// is exactly the fault musixmatch exhibited on 2026-09-04 (#838): every query
// answered with ONE unrelated track, HTTP 200, valid envelope, real lyrics.
//
// The detector must see it. An unwired detector passes every test it has, so
// this test exists to prove the seam is REACHED, not just that the package works.
func TestLaneReportsNonDiscriminatingProvider(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{
		Track: models.Track{ArtistName: "Fixed Performer", TrackName: "Fixed Song"},
	}}
	l, _ := newTestLane(p)

	var reported int
	l.WithResponseDrift(respdrift.New(3), func(lane, run string) { reported++ })

	for _, q := range []struct{ artist, title string }{
		{"Aurora Kestrel", "Marigold Drift"},
		{"Bramblewood Quintet", "Ninefold Ascent"},
		{"Zzzq Nonexistent", "Wwwv Not A Real Song"},
	} {
		if _, err := l.FindLyrics(context.Background(), models.Track{
			ArtistName: q.artist, TrackName: q.title,
		}, ""); err != nil {
			t.Fatalf("FindLyrics: %v", err)
		}
	}

	if reported != 1 {
		t.Errorf("reported %d times; want exactly 1 (three distinct queries, one identity)", reported)
	}
}

// TestLaneDoesNotReportOnDistinctIdentities is the counterweight: a healthy
// provider must never trip this. Without it the detector could fire on ordinary
// traffic and turn a diagnostic into an alert storm.
func TestLaneDoesNotReportOnDistinctIdentities(t *testing.T) {
	p := &varyingProvider{name: "petitlyrics"}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewProviderLane(p, cb)

	var reported int
	l.WithResponseDrift(respdrift.New(3), func(lane, run string) { reported++ })

	for i := 0; i < 6; i++ {
		if _, err := l.FindLyrics(context.Background(), models.Track{
			ArtistName: "Artist" + string(rune('A'+i)), TrackName: "Title" + string(rune('A'+i)),
		}, ""); err != nil {
			t.Fatalf("FindLyrics: %v", err)
		}
	}

	if reported != 0 {
		t.Errorf("reported %d times on a provider returning DISTINCT identities; want 0", reported)
	}
}

// TestLaneWithoutDetectorIsUnchanged pins that the wiring is OPT-IN. Every
// existing lane constructs without a detector, and must behave exactly as before
// -- no nil dereference, no added work.
func TestLaneWithoutDetectorIsUnchanged(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{
		Track: models.Track{ArtistName: "Fixed", TrackName: "Fixed"},
	}}
	l, _ := newTestLane(p)

	for i := 0; i < 5; i++ {
		if _, err := l.FindLyrics(context.Background(), models.Track{
			ArtistName: "a", TrackName: "b",
		}, ""); err != nil {
			t.Fatalf("FindLyrics without a detector: %v", err)
		}
	}
	if p.calls != 5 {
		t.Errorf("provider calls = %d; want 5", p.calls)
	}
}

// TestLaneDriftReportCarriesNoMetadata asserts the report names the LANE and the
// run length only. The identity that repeated is a track title from the user's
// library; it must never reach a log line or a metric label.
func TestLaneDriftReportCarriesNoMetadata(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{
		Track: models.Track{ArtistName: "Private Performer", TrackName: "Private Song"},
	}}
	l, _ := newTestLane(p)

	var gotLane, gotRun string
	l.WithResponseDrift(respdrift.New(2), func(lane, run string) { gotLane, gotRun = lane, run })

	l.FindLyrics(context.Background(), models.Track{ArtistName: "q1", TrackName: "t1"}, "")
	l.FindLyrics(context.Background(), models.Track{ArtistName: "q2", TrackName: "t2"}, "")

	if gotLane != "musixmatch" {
		t.Errorf("lane = %q; want musixmatch", gotLane)
	}
	for _, leak := range []string{"Private Performer", "Private Song"} {
		if gotLane == leak || gotRun == leak {
			t.Errorf("report leaked library metadata: lane=%q run=%q", gotLane, gotRun)
		}
	}
}

// varyingProvider returns a DIFFERENT identity per call, modeling a healthy
// provider that discriminates between requests.
type varyingProvider struct {
	name string
	n    int
}

func (p *varyingProvider) Name() string { return p.name }
func (p *varyingProvider) FindLyrics(_ context.Context, tr models.Track) (models.Song, error) {
	p.n++
	return models.Song{Track: models.Track{
		ArtistName: tr.ArtistName, TrackName: tr.TrackName,
	}}, nil
}
