package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/petitlyrics"
)

// The innertube lane had NO error policy at all before #856/#870, exactly as
// the petitlyrics lane had none before #607. Neither innertube sentinel was
// named in ClassifyOutcome or in providerClassifier, so every one of them fell
// to the default arms: OutcomeTransport for the outcome class, and the
// "transport / unexpected error" branch that leaves the breaker untouched.
//
// The costly half is the MISS. This provider's most frequent healthy outcome is
// a correspondence-gate rejection -- innertube search never signals "no match",
// so it answers even a nonsense query with a confident unrelated candidate that
// the gate then rejects (internal/innertube/doc.go). Unclassified, that routine
// miss was recorded as a queue FAILURE: attempts++, geometric backoff, and the
// row ramping toward permanent retirement. A provider working exactly as
// designed read as a provider failing.
//
// These tests drive the REAL classifier per sentinel through a lane, rather
// than asserting errors.Is in isolation, because the defect was never in the
// sentinels -- it was in which arm of the classifier they reached.

// newInnertubeLane builds a provider lane whose stub always fails with err.
func newInnertubeLane(err error) (*Lane, *circuit.Breaker) {
	return newTestLane(&stubProvider{name: "innertube", err: err})
}

// --- AC: a clean miss classifies as a benign miss, not a transport failure ---

// TestInnerTubeMissIsBenignNotTransport is the #870 regression itself. Against
// an unclassified innertube it fails on the first assertion: the miss lands in
// ClassifyOutcome's default arm as OutcomeTransport, which OUTRANKS
// OutcomeBenignMiss in precedence(), so the worker records a failure for a
// track the provider merely does not have.
func TestInnerTubeMissIsBenignNotTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		// The "reached the API, nothing usable" family. All of these route
		// through the SINGLE errors.Is(err, innertube.ErrNotFound) arm -- no arm
		// exists that could catch one and miss another -- so they are listed to
		// document the family rather than because each needs separate wiring.
		//
		// The correspondence-gate rejection is the one that matters most and was
		// missing from this table while the comments called it the important
		// member: innertube search never signals "no match", so a rejected
		// candidate is this lane's most frequent healthy outcome by
		// construction. It reaches here by wrapping (selection.go returns it
		// wrapped around ErrNotFound), which is exactly what the row below
		// pins.
		{"no search candidates", innertube.ErrNotFound},
		{"no lyrics tab for the video", innertube.ErrNoLyricsTab},
		{"correspondence gate rejects the candidate", fmt.Errorf(
			"innertube: no search candidate corresponds to the requested track: %w",
			innertube.ErrNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, the way a lane returns it in production.
			got := ClassifyOutcome(fmt.Errorf("lane innertube: find lyrics: %w", tc.err))
			if got != OutcomeBenignMiss {
				t.Errorf("ClassifyOutcome = %v, want OutcomeBenignMiss.\n"+
					"An innertube miss classifying as anything else -- transport above all --\n"+
					"makes the worker record a queue FAILURE for a routine miss and ramp the\n"+
					"row toward retirement (#748/#607/#870).", got)
			}
		})
	}
}

// TestInnerTubeMissDoesNotTripTheBreaker is the breaker half of the same claim:
// a miss must RESET the ramp, never open the lane. A provider that legitimately
// has nothing for a run of obscure tracks must not be taken out of service for
// it -- the #767 shape, where a threshold tuned on mainstream material fired on
// exactly the obscure material a lyrics provider is most needed for.
func TestInnerTubeMissDoesNotTripTheBreaker(t *testing.T) {
	l, cb := newInnertubeLane(innertube.ErrNotFound)

	// A long run of misses, which is this lane's NORMAL operating condition.
	for range 25 {
		_, err := l.FindLyrics(context.Background(), models.Track{}, "")
		if !errors.Is(err, innertube.ErrNotFound) {
			t.Fatalf("err = %v; want ErrNotFound preserved for ranking", err)
		}
	}
	if cb.Allow() != circuit.StateClosed {
		t.Error("a run of clean misses OPENED the breaker. A miss is a healthy round " +
			"trip: it says the provider has nothing for this track, not that the " +
			"provider is unwell.")
	}
	if cb.Trips() != 0 {
		t.Errorf("trips = %d, want 0; a benign miss must not advance the ramp", cb.Trips())
	}
}

// TestInnerTubeMissResetsTheRamp is the half the test above CANNOT reach, and
// the distinction is the whole point of the benign-miss arm.
//
// Above, the breaker starts fresh: trips are already 0 and the state is already
// closed, so "the miss did not make things worse" is satisfied by any arm that
// merely leaves the breaker alone -- including the default transport branch,
// which touches it by design. Measured: DELETING the entire benign-miss case
// from providerClassifier leaves that test, and the whole suite, GREEN.
//
// Reset is only observable from a ramp that is already advanced. So this
// pre-trips the breaker twice and advances the clock past the open window
// (half-open, where the provider is consulted again), then asserts the miss
// drove trips back to zero. Mirrors TestLaneBenignMissRecordsBenignMiss, which
// gets this right for musixmatch.
//
// The production consequence the fresh-breaker version cannot see: after a real
// fault has tripped this lane, a healthy miss would never close it back down.
// Misses are this lane's most frequent outcome by construction, so the lane
// would ratchet open and stay there.
func TestInnerTubeMissResetsTheRamp(t *testing.T) {
	l, cb := newInnertubeLane(innertube.ErrNotFound)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	cb.Trip()
	if cb.Trips() != 2 {
		t.Fatalf("setup: trips = %d, want 2 before the miss", cb.Trips())
	}
	// Past the open window, so the lane is half-open and the provider is
	// actually consulted -- an open breaker would short-circuit to
	// ErrLaneUnavailable and never reach the classifier at all.
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, innertube.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if cb.Trips() != 0 {
		t.Errorf("trips = %d, want 0; a benign miss must RESET the ramp, not merely "+
			"leave it alone", cb.Trips())
	}
	// EverSucceeded is deliberately NOT set by a miss, matching every other
	// provider: a miss is a successful round trip but not a genuine lyric match.
	// Without this, adding RecordSuccess() to the miss arm goes undetected.
	if cb.EverSucceeded() {
		t.Error("a benign miss set EverSucceeded; that flag means the lane genuinely " +
			"fetched a lyric, and a miss is not that")
	}
}

// TestInnerTubeNoLyricsTabResetsTheRamp drives ErrNoLyricsTab through the
// real classifier rather than only through ClassifyOutcome. It wraps
// ErrNotFound, so it shares the miss arm -- a property asserted in a comment
// and, before this, nowhere else.
func TestInnerTubeNoLyricsTabResetsTheRamp(t *testing.T) {
	l, cb := newInnertubeLane(innertube.ErrNoLyricsTab)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, innertube.ErrNoLyricsTab) {
		t.Fatalf("err = %v; want ErrNoLyricsTab", err)
	}
	if cb.Trips() != 0 {
		t.Errorf("trips = %d, want 0; a video with no lyrics rendition is a clean "+
			"miss and must reset the ramp like any other", cb.Trips())
	}
}

// TestInnerTubeOutcomeClasses pins the OUTCOME CLASS of every innertube
// sentinel, which is a different question from which breaker arm each one takes
// and was measured to be unpinned for two of them.
//
// Measured: moving ErrUnauthorized and ErrRateLimited out of the auth bucket
// and into the BENIGN-MISS bucket left the entire suite green. The lane tests
// below assert breaker state and pacer counts -- a different function -- and
// the enumeration guard only asserts "not OutcomeTransport", which
// OutcomeBenignMiss satisfies. So nothing was checking the class itself.
//
// The failure that permits is #748/#607 running BACKWARDS. OutcomeAuthRateLimit
// has precedence 4 and OutcomeBenignMiss has 1, so a 429 read as benign loses
// the cross-lane ranking, and when it IS the surfaced outcome the worker records
// a STABLE MISS against a track the provider never answered for. The catalog
// answer was unknown and it gets written down as "absent" -- durable, and worse
// than the failure this change fixes.
//
// This mirrors TestPetitLyricsControlGroupUnchanged, which had the subject
// under test less thoroughly pinned than its own control group.
func TestInnerTubeOutcomeClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want OutcomeClass
	}{
		{"401 is auth/ratelimit", innertube.ErrUnauthorized, OutcomeAuthRateLimit},
		{"429 is auth/ratelimit", innertube.ErrRateLimited, OutcomeAuthRateLimit},
		// The two deliberate exemptions, positively asserted rather than merely
		// absent from the enumeration guard's complaints.
		{"403 stays transport by design", innertube.ErrForbidden, OutcomeTransport},
		{"stale client version stays transport", innertube.ErrClientVersion, OutcomeTransport},
		{"miss is benign", innertube.ErrNotFound, OutcomeBenignMiss},
		{"no lyrics tab is benign", innertube.ErrNoLyricsTab, OutcomeBenignMiss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyOutcome(fmt.Errorf("lane innertube: find lyrics: %w", tc.err))
			if got != tc.want {
				t.Errorf("ClassifyOutcome = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- AC: a transport failure still classifies as transport ---

// TestInnerTubeTransportFailureStillClassifiesAsTransport guards the direction
// nobody thinks to check. It would be easy to "fix" #870 by making the whole
// provider benign, which would silently suppress the backoff a genuine failure
// is supposed to produce.
func TestInnerTubeTransportFailureStillClassifiesAsTransport(t *testing.T) {
	// An unexpected HTTP status, a dial failure, a malformed payload: none of
	// these wrap any sentinel, and all must keep their transport class.
	err := fmt.Errorf("lane innertube: find lyrics: %w",
		errors.New("innertube: unexpected HTTP status 503"))
	if got := ClassifyOutcome(err); got != OutcomeTransport {
		t.Errorf("ClassifyOutcome = %v, want OutcomeTransport for a genuine failure", got)
	}
}

func TestInnerTubeTransportFailureLeavesTheBreakerClosed(t *testing.T) {
	l, cb := newInnertubeLane(errors.New("innertube: unexpected HTTP status 503"))

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if err == nil {
		t.Fatal("want an error")
	}
	// Matching every other provider: an unexpected error is transport noise and
	// leaves the breaker untouched, rather than opening the lane on one blip.
	if cb.Allow() != circuit.StateClosed {
		t.Error("a single transport error opened the breaker")
	}
}

// --- AC: the lane-circuit-breaker path is given an innertube arm ---

func TestInnerTubeUnauthorizedTripsBreaker(t *testing.T) {
	l, cb := newInnertubeLane(innertube.ErrUnauthorized)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, innertube.ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized preserved for ranking", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 401 left the breaker CLOSED. This API is unauthenticated, so a " +
			"401 means the request shape or the public key changed -- a client " +
			"defect that retrying unchanged cannot fix.")
	}
}

func TestInnerTubeRateLimitedTripsBreakerAndRatchetsPacer(t *testing.T) {
	p := &stubProvider{name: "innertube", err: innertube.ErrRateLimited}
	pacer := &stubPacer{}
	cb := circuit.New(60e9, 30*60e9)
	l := NewProviderLane(p, cb)
	l.pacer = pacer

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, innertube.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited preserved", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 429 left the breaker CLOSED")
	}
	// A 429 is the one unambiguous throttle signal this provider produces, so it
	// is the one condition that may slow the lane.
	if pacer.throttles != 1 {
		t.Errorf("pacer throttles = %d, want 1; an explicit 429 must ratchet the pacer",
			pacer.throttles)
	}
}

// TestInnerTubeUnauthorizedDoesNotRatchetPacer pins the asymmetry against the
// musixmatch policy it would be natural to copy. Musixmatch treats a
// post-success 401 as an egress throttle and ratchets; innertube must not,
// because it holds no credential that could expire -- its API key is a public,
// non-authenticating constant every unofficial client ships. Slowing the lane
// would persist long past the real fix.
func TestInnerTubeUnauthorizedDoesNotRatchetPacer(t *testing.T) {
	p := &stubProvider{name: "innertube", err: innertube.ErrUnauthorized}
	pacer := &stubPacer{}
	cb := circuit.New(60e9, 30*60e9)
	l := NewProviderLane(p, cb)
	l.pacer = pacer

	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if pacer.throttles != 0 {
		t.Errorf("pacer throttles = %d, want 0; there is no credential here to have "+
			"expired, so a 401 is a client-shape defect and not a throttle",
			pacer.throttles)
	}
}

func TestInnerTubeForbiddenTripsBreaker(t *testing.T) {
	l, cb := newInnertubeLane(innertube.ErrForbidden)

	_, err := l.FindLyrics(context.Background(), models.Track{}, "")
	if !errors.Is(err, innertube.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden preserved", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a 403 left the breaker CLOSED. Per internal/innertube/errors.go a " +
			"403 is a REFUSED request shape, not throttling: retrying an unchanged " +
			"request cannot succeed.")
	}
}

// --- AC: ErrClientVersion, which wraps ErrForbidden, is checked ---

// TestInnerTubeClientVersionIsDistinguishedFromForbidden is the ordering
// assertion #870 asks for by name, and it is subtler than it looks: BOTH
// sentinels trip the breaker and BOTH classify as transport, so no
// state-based assertion can separate them. What the ordering protects is the
// operator-facing DIAGNOSIS.
//
// ErrClientVersion wraps ErrForbidden, so a providerClassifier that tested
// ErrForbidden first would swallow it and log "refused the request shape",
// sending the operator hunting for a malformed body when the actual remedy is
// bumping the pinned ANDROID_MUSIC version constant. The pin today is 7.03.52;
// 5.16.51 is a DIFFERENT, older version, measured to return HTTP 400 on every
// call, and it is the evidence for what a stale pin does rather than the value
// currently in the code (see internal/innertube/doc.go). That pin is the single
// most fragile fact in the provider, and the failure is total: every browse call
// fails until the constant moves.
//
// The wrapping relationship is asserted directly, since it is the premise the
// ordering constraint rests on and a future edit to errors.go could drop it
// silently.
func TestInnerTubeClientVersionIsDistinguishedFromForbidden(t *testing.T) {
	if !errors.Is(innertube.ErrClientVersion, innertube.ErrForbidden) {
		t.Fatal("ErrClientVersion no longer wraps ErrForbidden; the ordering " +
			"constraint in providerClassifier and the exemption in " +
			"sentinel_enumeration_test.go both rest on that relationship")
	}

	l, cb := newInnertubeLane(innertube.ErrClientVersion)
	var err error
	logs := captureLogs(t, func() {
		_, err = l.FindLyrics(context.Background(), models.Track{}, "")
	})

	// THE LOG IS THE ONLY OBSERVABLE THAT SEPARATES THE TWO ARMS, so it is what
	// this test must assert on. Measured: swapping the two cases in
	// providerClassifier left every OTHER assertion in this test passing --
	// errors.Is sees through the wrap either way, both arms trip the breaker,
	// and both classify as transport. A test that omits this check pins the
	// ordering constraint in its NAME and nowhere in its body.
	if !strings.Contains(logs, "pinned client version") {
		t.Errorf("log did not name the client-version cause.\n"+
			"got: %s\n"+
			"ErrClientVersion wraps ErrForbidden, so an arm ordered after it "+
			"swallows it and logs \"refused the request shape\" -- sending the "+
			"operator after a malformed body when the fix is bumping the pinned "+
			"ANDROID_MUSIC version constant.", logs)
	}
	if strings.Contains(logs, "refused the request shape") {
		t.Errorf("log reported the generic ErrForbidden diagnosis for a stale "+
			"client version; the ErrClientVersion arm must be tested FIRST.\ngot: %s", logs)
	}

	// The SPECIFIC sentinel must survive to the caller, not be flattened to the
	// general one it wraps.
	if !errors.Is(err, innertube.ErrClientVersion) {
		t.Fatalf("err = %v; want ErrClientVersion preserved for diagnosis", err)
	}
	if cb.Allow() != circuit.StateOpen {
		t.Error("a rejected client version left the breaker CLOSED. Every browse " +
			"call fails until the version constant is bumped; spending paced " +
			"requests on it is pure waste.")
	}
	// It inherits ErrForbidden's outcome class deliberately: same remedy class
	// (a client change), so the same bucket. Only the diagnosis differs.
	if got := ClassifyOutcome(err); got != OutcomeTransport {
		t.Errorf("ClassifyOutcome = %v, want OutcomeTransport (inherited from "+
			"ErrForbidden, and recorded as a deliberate exemption)", got)
	}
}

// --- AC: the petitlyrics control group still classifies unchanged ---

// TestPetitLyricsControlGroupUnchanged is the control #870's own measurement
// used: petitlyrics was the provider whose miss classified correctly while
// innertube's did not. Adding innertube arms ahead of the petitlyrics arms
// could regress them -- an over-broad innertube case would capture a
// petitlyrics error before it reached its own arm -- so the control is
// re-measured here rather than assumed.
func TestPetitLyricsControlGroupUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want OutcomeClass
	}{
		{"miss stays benign", petitlyrics.ErrNotFound, OutcomeBenignMiss},
		{"provider outage stays auth/ratelimit", petitlyrics.ErrProviderUnavailable, OutcomeAuthRateLimit},
		{"401 stays auth/ratelimit", petitlyrics.ErrUnauthorized, OutcomeAuthRateLimit},
		{"429 stays auth/ratelimit", petitlyrics.ErrRateLimited, OutcomeAuthRateLimit},
		{"403 stays transport by design", petitlyrics.ErrForbidden, OutcomeTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyOutcome(fmt.Errorf("lane petitlyrics: find lyrics: %w", tc.err))
			if got != tc.want {
				t.Errorf("ClassifyOutcome = %v, want %v; the innertube arms must not "+
					"have disturbed the petitlyrics control group", got, tc.want)
			}
		})
	}
}

// TestInnerTubeAndPetitLyricsMissesAgree is the cross-provider invariant the
// whole enumeration guard exists to protect: two provider lanes must not
// disagree about what a miss IS. When they did (#607), the worker released the
// item on a different path depending on which lane produced the miss.
func TestInnerTubeAndPetitLyricsMissesAgree(t *testing.T) {
	it := ClassifyOutcome(fmt.Errorf("lane innertube: %w", innertube.ErrNotFound))
	pl := ClassifyOutcome(fmt.Errorf("lane petitlyrics: %w", petitlyrics.ErrNotFound))
	if it != pl {
		t.Errorf("innertube miss = %v but petitlyrics miss = %v; the two lanes "+
			"disagree about what a miss is, which is the #607 defect", it, pl)
	}
}
