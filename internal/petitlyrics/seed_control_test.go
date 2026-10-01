package petitlyrics

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// This file guards #1195: the #767 liveness control was memory-only, so every
// restart dropped the lane onto the count fallback.

// TestSeededControlAdjudicatesMissRunAfterRestart: a FRESH client (a restart)
// seeded from a prior win must answer a threshold-length miss run with the
// probe, not the count. The server still has the control track, so the probe
// hits and the run is material.
func TestSeededControlAdjudicatesMissRunAfterRestart(t *testing.T) {
	handler, probes := serveKnownGoodOnly(t)
	c, _ := newTestClient(t, handler)
	c.SeedKnownGood(models.Track{TrackName: "Known", ArtistName: "Good"})

	for i := 0; i < ZeroResultThreshold; i++ {
		_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
		if errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("miss %d reported ErrProviderUnavailable on a restarted client seeded with a "+
				"control the provider still serves; the probe must adjudicate, not the count (#1195)", i+1)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("probe count = %d; want 1 (the seeded control must be what adjudicates the run)", got)
	}
}

// TestSeededControlStillConfirmsRealOutage: seeding must not blunt #607, and a
// seeded control is UNVERIFIED (#1195 review F2). With a dead credential the
// seed's probe misses; that miss is not evidence (the row may name a track this
// lane never served), so the seed is dropped and the run is judged by the count,
// which still confirms the outage. Past it the lane has no control, so the next
// miss makes ONE request (no probe) and is judged by the count, as an unseeded
// lane's is.
func TestSeededControlStillConfirmsRealOutage(t *testing.T) {
	handler, calls := serveHitThenEmpty(t, 0)
	c, _ := newTestClient(t, handler)
	c.SeedKnownGood(models.Track{TrackName: "Known", ArtistName: "Good"})

	var err error
	for i := 0; i < ZeroResultThreshold; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v; want ErrProviderUnavailable when the seeded control no longer resolves", err)
	}
	c.mu.Lock()
	have := c.hasKnownGood
	c.mu.Unlock()
	if have {
		t.Error("a seeded control that missed its probe was kept; it is unverified and must be dropped")
	}
	before := calls.Load()
	_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	if got := calls.Load() - before; got != 1 || !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("next miss made %d requests, err = %v; want 1 (no control left to probe) and the count-confirmed outage", got, err)
	}
}

// TestSeededControlMissIsNotProbedEvidence is F2(b): a seed naming a track the
// provider does NOT serve (a purged/retired row that kept its provider_lane) on
// a HEALTHY lane. The probe on it misses; that must drop the seed rather than be
// reported as a probed outage; the run is judged by the count, exactly as an
// unseeded lane's is. The first hit then recovers the lane and earns a real
// control, which IS evidence from then on.
func TestSeededControlMissIsNotProbedEvidence(t *testing.T) {
	handler, _ := serveKnownGoodOnly(t) // serves "Known" only
	c, _ := newTestClient(t, handler)
	c.SeedKnownGood(models.Track{TrackName: "Never Served", ArtistName: "Wrong"})

	for i := 0; i < ZeroResultThreshold; i++ {
		_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	}
	c.mu.Lock()
	have, seeded := c.hasKnownGood, c.knownGoodSeeded
	c.mu.Unlock()
	if have || seeded {
		t.Fatalf("hasKnownGood=%v seeded=%v after the seed missed its probe; want it dropped", have, seeded)
	}

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("hit after the dropped seed: %v", err)
	}
	c.mu.Lock()
	have, seeded, latched := c.hasKnownGood, c.knownGoodSeeded, c.zeroReported
	c.mu.Unlock()
	if !have || seeded || latched {
		t.Errorf("after a hit: hasKnownGood=%v seeded=%v latched=%v; want an EARNED control and the latch cleared", have, seeded, latched)
	}
}

// TestSeedDoesNotTouchTheRun: the seed is not a response, so it must neither
// clear a miss run in progress nor accept an unusable track as a control.
func TestSeedDoesNotTouchTheRun(t *testing.T) {
	c, _ := newTestClient(t, serveEmpty())
	for i := 0; i < 5; i++ {
		c.recordZeroResult()
	}
	c.SeedKnownGood(models.Track{TrackName: "", ArtistName: "Good"})
	c.mu.Lock()
	run, have := c.consecutiveZero, c.hasKnownGood
	c.mu.Unlock()
	if have {
		t.Error("a track with no title was accepted as a control; it cannot be probed")
	}
	c.SeedKnownGood(models.Track{TrackName: "Known", ArtistName: "Good"})
	c.mu.Lock()
	after, haveAfter := c.consecutiveZero, c.hasKnownGood
	c.mu.Unlock()
	if run != 5 || after != 5 {
		t.Errorf("miss run = %d then %d across the seeds; want 5 both times (a seed proves nothing about the credential now)", run, after)
	}
	if !haveAfter {
		t.Error("a usable track was not accepted as a control")
	}
}

// TestEarnedControlAfterSeedIsEvidence: once this process earns a hit, the
// control is VERIFIED, even if it is the very track that was seeded. A later
// probe miss on it is then real evidence: the probed outage is reported and the
// control is kept, so each further miss re-asks it (#1195 review F2).
func TestEarnedControlAfterSeedIsEvidence(t *testing.T) {
	handler, _ := serveHitThenEmpty(t, 1) // the first request hits, then the credential dies
	c, _ := newTestClient(t, handler)
	c.SeedKnownGood(models.Track{TrackName: "Known", ArtistName: "Good"})
	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("earning hit: %v", err)
	}
	var err error
	for i := 0; i < ZeroResultThreshold; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	}
	c.mu.Lock()
	have, seeded, run := c.hasKnownGood, c.knownGoodSeeded, c.consecutiveZero
	c.mu.Unlock()
	if !errors.Is(err, ErrProviderUnavailable) || !have || seeded || run < ZeroResultThreshold {
		t.Errorf("err=%v hasKnownGood=%v seeded=%v run=%d; want a PROBED outage on an earned control "+
			"(control kept, run left at the threshold)", err, have, seeded, run)
	}
}
