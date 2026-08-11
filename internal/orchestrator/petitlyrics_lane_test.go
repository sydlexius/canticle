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

func TestPetitLyricsProviderUnavailableTripsBreaker(t *testing.T) {
	// Sustained zero-results almost certainly means a dead application id, so the
	// lane opens rather than continuing to spend requests on a provider that
	// answers nothing. The breaker's backoff re-probes periodically, so a
	// transient cause still recovers without operator action.
	//
	// This ordering is load-bearing: ErrProviderUnavailable WRAPS ErrNotFound, so
	// it must be tested BEFORE the benign-miss branch. Reversed, the wrapper would
	// match the miss case first and the outage would reset the ramp instead of
	// tripping the lane, which is the exact silent-degradation #607 exists to end.
	p := &stubProvider{name: "petitlyrics", err: petitlyrics.ErrProviderUnavailable}
	l, cb := newTestLane(p)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, petitlyrics.ErrProviderUnavailable) {
		t.Fatalf("err = %v; want ErrProviderUnavailable preserved for ranking", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a sustained zero-result outage left the breaker CLOSED. The lane " +
			"would keep firing paced requests at a provider returning nothing, " +
			"indefinitely, while appearing to serve honest misses.")
	}
}

// pacingStub is a stubProvider that also satisfies providers.AdaptivePacer, so a
// test can observe whether the lane ratcheted the pacer. stubProvider itself does
// not implement the optional interface, and NewProviderLane type-asserts for it,
// so a plain stub silently receives no notifications.
type pacingStub struct {
	stubProvider
	throttles int
	successes int
}

func (p *pacingStub) OnThrottle() { p.throttles++ }
func (p *pacingStub) OnSuccess()  { p.successes++ }

func TestPetitLyricsProviderUnavailableDoesNotRatchetPacer(t *testing.T) {
	// A dead credential is not a throttle. Ratcheting the adaptive pacer here
	// would slow a lane that is not being rate limited, and would keep it slow
	// after the credential is restored.
	p := &pacingStub{stubProvider: stubProvider{name: "petitlyrics", err: petitlyrics.ErrProviderUnavailable}}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewProviderLane(p, cb)

	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")

	if cb.Allow() != circuit.StateOpen {
		t.Fatal("precondition: the sentinel should have opened the breaker")
	}
	if p.throttles != 0 {
		t.Errorf("pacer ratcheted %d time(s) on a credential outage; only an "+
			"explicit 429 is a throttle signal, and slowing a lane that is not "+
			"rate limited would persist after the credential is restored", p.throttles)
	}
}

func TestPetitLyricsRateLimitedDoesRatchetPacer(t *testing.T) {
	// The control for the test above: an explicit 429 SHOULD ratchet. Without
	// this, "throttles == 0" above would also pass if the pacer were never wired
	// up at all, which would make it a tautology.
	p := &pacingStub{stubProvider: stubProvider{name: "petitlyrics", err: petitlyrics.ErrRateLimited}}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewProviderLane(p, cb)

	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")

	if p.throttles != 1 {
		t.Errorf("pacer ratcheted %d time(s) on an explicit 429; want exactly 1", p.throttles)
	}
}

// ClassifyOutcome is the telemetry-and-queue twin of providerClassifier: the
// worker branches on it (worker.go:1187) to decide how a work item is RELEASED.
// It enumerated only the musixmatch sentinels, so every petitlyrics error fell to
// OutcomeTransport -- meaning the two provider lanes disagreed about what a miss
// even is, and a petitlyrics outage was indistinguishable from a network blip.
func TestClassifyOutcome_PetitLyricsSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want OutcomeClass
		why  string
	}{
		{
			"clean miss", petitlyrics.ErrNotFound, OutcomeBenignMiss,
			"a miss is a successful round trip; classing it as transport makes the " +
				"queue release the item on a different path than an identical musixmatch miss",
		},
		{
			"undecodable tier", petitlyrics.ErrUnsupportedTier, OutcomeBenignMiss,
			"lyricsType 2 arrived and parsed; only its payload is undecodable, " +
				"which is a per-track gap on a healthy lane",
		},
		{
			"revoked application id", petitlyrics.ErrUnauthorized, OutcomeAuthRateLimit,
			"an auth rejection must release the item without recording a stable miss, " +
				"exactly as the musixmatch 401 does",
		},
		{
			"throttled", petitlyrics.ErrRateLimited, OutcomeAuthRateLimit,
			"an explicit 429 is the same class of outcome for either provider",
		},
		{
			"sustained zero results", petitlyrics.ErrProviderUnavailable, OutcomeAuthRateLimit,
			"a revoked application id reached through the zero-result path is the same " +
				"OUTCOME as one reached through a 401, however differently it was detected",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyOutcome(tc.err); got != tc.want {
				t.Errorf("ClassifyOutcome(%v) = %v; want %v.\n%s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

func TestClassifyOutcome_PetitLyricsForbiddenIsNotAuth(t *testing.T) {
	// 403 is a refused request SHAPE, not a credential or throttle problem, so it
	// must NOT be folded into the auth/rate-limit class. Bucketing it there would
	// reproduce the #495 misdiagnosis, where a User-Agent denylist rejection read
	// as a rate limit and sent the investigation after a phantom throttle.
	if got := ClassifyOutcome(petitlyrics.ErrForbidden); got == OutcomeAuthRateLimit {
		t.Error("403 classified as auth/rate-limit; it is a client-shape defect " +
			"that no amount of waiting or credential rotation will fix")
	}
}
