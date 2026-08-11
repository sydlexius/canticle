package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/petitlyrics"
)

// The petitlyrics lane had NO error policy at all before #607: providerClassifier
// referenced only the musixmatch sentinels, so every petitlyrics failure fell
// through to the "transport / unexpected error" branch that deliberately leaves
// the breaker untouched.
//
// The consequence was a lane that could never trip. A revoked credential (401), a
// refused request shape (403), and provider throttling (429) all read as ordinary
// transport noise: no breaker trip, no pacer ratchet, and none of the diagnostic
// warnings the musixmatch lane gets for the same conditions. Meanwhile a genuine
// miss never reset the ramp, because petitlyrics.ErrNotFound is not in
// musixmatch.IsBenignMiss's vocabulary either.
//
// These tests pin the policy per sentinel. They are the regression net for the
// asymmetry: whatever musixmatch does for a condition, petitlyrics must do the
// analogous thing, or the second provider lane is silently unmanaged.

func TestPetitLyricsUnauthorizedTripsBreaker(t *testing.T) {
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrUnauthorized}
	l, cb := newTestLane(p)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized preserved for ranking", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 401 left the breaker CLOSED. The clientAppId is a hardcoded " +
			"constant shared by every install, so a revocation is exactly the " +
			"outage this must surface rather than absorb as transport noise.")
	}
}

func TestPetitLyricsForbiddenTripsBreaker(t *testing.T) {
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrForbidden}
	l, cb := newTestLane(p)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden preserved for ranking", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 403 left the breaker CLOSED. Per internal/petitlyrics/errors.go " +
			"a 403 is a REFUSED request shape (the #495 User-Agent denylist), not " +
			"throttling: retrying an unchanged request cannot succeed.")
	}
}

func TestPetitLyricsRateLimitedTripsBreaker(t *testing.T) {
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrRateLimited}
	l, cb := newTestLane(p)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited preserved for ranking", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 429 left the breaker CLOSED; an explicit throttle signal must " +
			"open the lane, exactly as it does for musixmatch")
	}
}

func TestPetitLyricsNotFoundIsBenignMiss(t *testing.T) {
	// A clean miss proves the round trip worked, so it must RESET the throttle
	// ramp rather than trip anything. Mirrors TestLaneBenignMissRecordsBenignMiss,
	// including the clock advance: RecordBenignMiss clears the ramp but does not
	// reopen an open window, so the miss has to actually reach the provider.
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrNotFound}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	cb.Trip()
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound preserved", err)
	}
	if cb.Trips() != 0 {
		t.Errorf("trips = %d; want 0. A successful round trip that simply found "+
			"nothing must reset the ramp, not leave the lane ratcheting toward "+
			"a longer backoff.", cb.Trips())
	}
	if cb.EverSucceeded() {
		t.Error("a benign miss must NOT set EverSucceeded: the round trip worked " +
			"but no lyric was matched")
	}
}

func TestPetitLyricsUnsupportedTierIsBenignMiss(t *testing.T) {
	// lyricsType 2 (the encrypted LSY blob) is undecodable today. The response
	// ARRIVED and parsed fine; only its payload tier is unsupported. That is a
	// per-track capability gap on a healthy lane, so it resets the ramp exactly
	// like a miss and must never be mistaken for a lane fault.
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrUnsupportedTier}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	cb.Trip()
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrUnsupportedTier) {
		t.Fatalf("err = %v; want ErrUnsupportedTier preserved", err)
	}
	if cb.Trips() != 0 {
		t.Errorf("trips = %d; want 0. An undecodable tier is a healthy round "+
			"trip, so it must reset the ramp rather than ratchet it.", cb.Trips())
	}
	if cb.EverSucceeded() {
		t.Error("an undecodable tier must NOT set EverSucceeded: no lyric was written")
	}
}
