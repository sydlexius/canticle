package petitlyrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
)

// These drive the REAL client through the REAL lane and breaker (#1195 review
// F1/F4), because the defect lives in the seam between them: the client rearms
// its miss counter, and what the breaker does with the misses that follow is
// decided by the orchestrator's classifier, not by the client. A client-only
// test passes on either side of that seam.
//
// Production defaults: 60s base, 30m cap, one lookup per 30s.

const (
	simBase = 60 * time.Second
	simCap  = 30 * time.Minute
	simStep = 30 * time.Second
)

// hitTrack is the fixture's own identity, so a hit both returns songs and
// passes candidate selection.
var hitTrack = models.Track{TrackName: "Lorem Ipsum", ArtistName: "Dolor Sit"}

// simServer answers a hit for hitTrack when live() is true and an empty
// envelope for everything else, counting requests.
func simServer(t *testing.T, live func() bool) (string, *atomic.Int64) {
	t.Helper()
	hit, err := os.ReadFile(filepath.Join("testdata", "type1_unsynced.xml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	const empty = `<?xml version="1.0" encoding="UTF-8"?><response><songs></songs></response>`
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		if err := r.ParseForm(); err == nil && live() && r.FormValue("key_title") == hitTrack.TrackName {
			_, _ = w.Write(hit)
			return
		}
		_, _ = w.Write([]byte(empty))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &requests
}

type simResult struct {
	maxTrips    int
	resetsAfter int // ramp resets to 0 after the first trip, without a hit
	served      int // hitTrack lookups that returned lyrics
	scheduled   int // hitTrack lookups attempted while the lane was not open
	finalTrips  int
	finalState  circuit.BreakerState
}

// runSim performs steps lookups, one per simStep of fake time. isHit decides
// which lookups ask for hitTrack; every other lookup asks for an obscure track.
func runSim(t *testing.T, baseURL string, seed *models.Track, steps int, isHit func(i int) bool) simResult {
	t.Helper()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cb := circuit.New(simBase, simCap)
	cb.SetClock(func() time.Time { return now })
	client := petitlyrics.NewClientForTest(baseURL)
	if seed != nil {
		client.SeedKnownGood(*seed)
	}
	lane := orchestrator.NewProviderLane(providers.New(providers.PetitLyrics, client), cb)

	var r simResult
	tripped := false
	for i := 0; i < steps; i++ {
		track := models.Track{TrackName: "Obscure", ArtistName: "Artist"}
		hitStep := isHit(i)
		if hitStep {
			track = hitTrack
		}
		song, err := lane.FindLyrics(context.Background(), track, "")
		if !errors.Is(err, orchestrator.ErrLaneUnavailable) && hitStep {
			r.scheduled++
			if err == nil && song.Lyrics.LyricsBody != "" {
				r.served++
			}
		}
		trips := cb.Trips()
		if trips > r.maxTrips {
			r.maxTrips = trips
		}
		if trips > 0 {
			tripped = true
		} else if tripped && err != nil {
			r.resetsAfter++
			tripped = false
		}
		now = now.Add(simStep)
	}
	r.finalTrips = cb.Trips()
	r.finalState = cb.Snapshot().State
	return r
}

// TestLaneUnseededRevokedCredentialRampsBackoff: a fresh install whose
// application id is already revoked never earns a control. After the first
// count-confirmed outage the client rearms; the misses that follow must NOT be
// read as benign, or each one resets the breaker's ramp and the lane sits at
// trips=1 forever, hammering a dead provider with a 60s backoff (F1). The trip
// count must climb, and the request volume must stay far below one per lookup.
func TestLaneUnseededRevokedCredentialRampsBackoff(t *testing.T) {
	url, requests := simServer(t, func() bool { return false })
	const steps = 6 * 60 * 2 // six hours
	r := runSim(t, url, nil, steps, func(int) bool { return false })

	if r.maxTrips < 5 {
		t.Errorf("max trips = %d over six hours; want the ramp to climb (>= 5). Stuck at 1 means the "+
			"latched misses are resetting the breaker as benign misses (#1195 F1)", r.maxTrips)
	}
	if r.resetsAfter != 0 {
		t.Errorf("the ramp reset to 0 %d time(s) with no hit; a latched miss must not read as recovery", r.resetsAfter)
	}
	if got := requests.Load(); got > steps/2 {
		t.Errorf("%d requests over %d lookups; a revoked lane must spend most of its time open", got, steps)
	}
}

// TestLaneUnseededHealthyLowHitRateRecovers: a healthy lane with no control and
// a long initial dry spell takes a count-confirmed (false) outage. The rearm
// must still let it through to its next hit, which closes the breaker and earns
// a control -- the half-open ratchet must stay fixed with F1's change in place.
func TestLaneUnseededHealthyLowHitRateRecovers(t *testing.T) {
	url, _ := simServer(t, func() bool { return true })
	const steps = 6 * 60 * 2
	// No hit at all for the first 60 lookups (three thresholds), then one
	// lookup in ten is a track the provider has.
	isHit := func(i int) bool { return i >= 60 && i%10 == 0 }
	r := runSim(t, url, nil, steps, isHit)

	if r.maxTrips < 1 {
		t.Fatalf("max trips = %d; the scenario did not produce the false count-confirmed outage it tests", r.maxTrips)
	}
	if r.finalTrips != 0 || r.finalState != circuit.StateClosed {
		t.Errorf("final trips=%d state=%v; want a recovered, closed lane", r.finalTrips, r.finalState)
	}
	want := (steps - 60) / 10
	if r.served < want*9/10 {
		t.Errorf("served %d of %d scheduled hits (%d reached the provider); a healthy lane must not stay "+
			"latched open on a count-only verdict (#1195 ratchet)", r.served, want, r.scheduled)
	}
}
