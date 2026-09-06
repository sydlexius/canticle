package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
)

// TestProviderLanesWireDriftDetection asserts the PRODUCTION construction path
// opts its provider lanes into response-drift detection (#839).
//
// This test exists because the detector and its lane seam both pass every test
// in their own packages while being unreachable from serve mode. Wiring that is
// never called is the failure mode here, not wiring that is wrong -- so the
// assertion is on the production constructor, not on a hand-built lane.
func TestProviderLanesWireDriftDetection(t *testing.T) {
	w := New(nil, nil, nil, nil)

	if len(w.lanes) == 0 {
		t.Fatal("worker constructed with no lanes")
	}
	for _, l := range w.lanes {
		if !l.DriftWired() {
			t.Errorf("lane %q has no response-drift detector; the feature is unreachable in serve mode", l.Name())
		}
	}
}

// TestFallbackLanesWireDriftDetection covers the SECOND construction site.
// SetFallbackProviders builds its own lanes, so wiring only the primary would
// leave every fallback -- including petitlyrics, currently the only enabled
// lane in production -- undetected.
func TestFallbackLanesWireDriftDetection(t *testing.T) {
	w := New(nil, nil, nil, nil)
	w.SetFallbackProviders(&driftStubProvider{name: "petitlyrics"})

	if len(w.lanes) < 2 {
		t.Fatalf("got %d lanes; want the primary plus one fallback", len(w.lanes))
	}
	for _, l := range w.lanes {
		if !l.DriftWired() {
			t.Errorf("fallback lane %q has no response-drift detector", l.Name())
		}
	}
}

// driftStubProvider is a minimal LyricsProvider for the fallback-wiring test.
type driftStubProvider struct{ name string }

func (p *driftStubProvider) Name() string { return p.name }
func (p *driftStubProvider) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{}, nil
}

// TestInnerTubeLaneWiresDriftDetection is #857's "assert it, do not assume it"
// acceptance criterion.
//
// The issue states that worker and lane code need NO change, because
// NewProviderLane, laneName and withDriftDetection are all generic -- so
// registering a provider gets drift detection for free. That claim is exactly
// the kind that is true until it is not: a provider-specific construction path
// added later would silently opt out, and the detector's whole failure mode is
// a fault nobody notices.
//
// It uses the REAL innertube client rather than a stub named "innertube". A
// stub would pass even if the concrete type failed to flow through
// providers.New and laneName -- proving only that the test's own string
// survives, which is the vacuity this repo keeps rediscovering.
func TestInnerTubeLaneWiresDriftDetection(t *testing.T) {
	w := New(nil, nil, nil, nil)
	w.SetFallbackProviders(providers.New(providers.InnerTube, innertube.NewClient()))

	if len(w.lanes) < 2 {
		t.Fatalf("got %d lanes; want the primary plus the innertube fallback", len(w.lanes))
	}

	var found bool
	for _, l := range w.lanes {
		if l.Name() != providers.InnerTube {
			continue
		}
		found = true
		if !l.DriftWired() {
			t.Error("the innertube lane has no response-drift detector; #844's " +
				"detection does not in fact cover a newly registered provider")
		}
	}
	if !found {
		lanes := make([]string, 0, len(w.lanes))
		for _, l := range w.lanes {
			lanes = append(lanes, l.Name())
		}
		t.Fatalf("no lane named %q among %v -- laneName did not carry the "+
			"provider's own identity, so every result would be misattributed in "+
			"work_queue, lane_attempts and the UI credit",
			providers.InnerTube, lanes)
	}
}

// TestDriftWarningIsEmittedAndCarriesNoMetadata drives real traffic through the
// PRODUCTION helper so the slog callback actually fires.
//
// The wiring tests above assert the detector is attached; they never make it
// report, so the callback body -- the only thing that emits the operator-facing
// line -- went unexecuted. That is where a wrong field name or a leaked title
// would hide, and it is the half of this feature an operator actually reads.
func TestDriftWarningIsEmittedAndCarriesNoMetadata(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// A provider that answers every distinct query with ONE fixed track: the
	// exact fault of 2026-09-04 (#838), reproduced through the real helper.
	lane := withDriftDetection(orchestrator.NewProviderLane(
		&fixedTrackProvider{name: "musixmatch", artist: "Private Performer", title: "Private Song"},
		circuit.New(time.Minute, 30*time.Minute),
	))

	for i := 0; i < driftThreshold; i++ {
		if _, err := lane.FindLyrics(context.Background(), models.Track{
			ArtistName: "Asked Artist " + strconv.Itoa(i),
			TrackName:  "Asked Title " + strconv.Itoa(i),
		}, ""); err != nil {
			t.Fatalf("FindLyrics: %v", err)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "stopped discriminating") {
		t.Fatalf("no drift warning emitted after %d distinct queries returning one track:\n%s",
			driftThreshold, out)
	}
	if !strings.Contains(out, "lane=musixmatch") {
		t.Errorf("warning does not name the lane:\n%s", out)
	}
	if !strings.Contains(out, "consecutive_distinct_queries="+strconv.Itoa(driftThreshold)) {
		t.Errorf("warning does not carry the run length:\n%s", out)
	}
	// The identity that repeated, and the queries that asked for it, are the
	// library's private metadata. Neither may reach an operator log line.
	for _, leak := range []string{"Private Performer", "Private Song", "Asked Artist", "Asked Title"} {
		if strings.Contains(out, leak) {
			t.Errorf("drift warning leaked library metadata %q:\n%s", leak, out)
		}
	}
}

// fixedTrackProvider answers every query with the same track, modeling the
// non-discriminating provider this detector exists to catch.
type fixedTrackProvider struct{ name, artist, title string }

func (p *fixedTrackProvider) Name() string { return p.name }
func (p *fixedTrackProvider) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{Track: models.Track{ArtistName: p.artist, TrackName: p.title}}, nil
}
