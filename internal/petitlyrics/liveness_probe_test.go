package petitlyrics

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// This file is the guard for #767.
//
// ZeroResultThreshold escalates a run of consecutive zero-result lookups to
// ErrProviderUnavailable, on the reasoning that a revoked clientAppId keeps
// answering HTTP 200 with no songs (#607). A fixed COUNT cannot make that
// distinction, and the population it gets wrong is the one this lane exists to
// serve: a fallback lane only sees what the primary already missed, so long miss
// runs are its normal operating condition.
//
// Measured on a real paced sweep: a 91% miss rate, and an exact DP over the run
// puts P(>=1 run of 20 misses in 78 lookups) at 0.742. The escalation was more
// likely than not to fire on healthy material -- 6 lookups had succeeded on the
// same credential earlier in that very run.
//
// The fix is a positive liveness probe: on reaching the threshold, re-fetch a
// track this credential DEMONSTRABLY served before. A hit proves the credential
// works and the misses were about the material; a miss is real evidence.

// serveKnownGoodOnly answers requests for the control track ("Known") with a real
// fixture and everything else with an empty envelope. It counts probe hits.
//
// This is the HEALTHY-CREDENTIAL model, and getting it right is the whole point:
// the provider still HAS the track it served before, and simply lacks the obscure
// ones. A count-based handler ("first N requests hit, then empty") would also
// starve the probe's re-fetch, which models a DEAD credential instead -- the
// opposite scenario.
func serveKnownGoodOnly(t *testing.T) (http.HandlerFunc, *atomic.Int32) {
	t.Helper()
	hit := serveFixture(t, "type1_unsynced.xml")
	var probes atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil && r.FormValue("key_title") == "Known" {
			probes.Add(1)
			hit(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(emptyResponse))
	}, &probes
}

// serveHitThenEmpty answers the first n requests with a real fixture and every
// later request with an empty envelope -- including the liveness probe's
// re-fetch. That models a credential that WORKED and then genuinely died.
func serveHitThenEmpty(t *testing.T, n int32) (http.HandlerFunc, *atomic.Int32) {
	t.Helper()
	hit := serveFixture(t, "type1_unsynced.xml")
	var count atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) <= n {
			hit(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(emptyResponse))
	}, &count
}

// TestMissRunOnHealthyCredentialIsNotAnOutage is the #767 regression test: the
// exact shape that misfired in production.
//
// A credential that has served hits, then hits a threshold-length run of misses
// on obscure material, must NOT be reported as unavailable -- the liveness probe
// re-fetches the known-good track, gets a hit, and the run is correctly read as
// material rather than a credential failure.
func TestMissRunOnHealthyCredentialIsNotAnOutage(t *testing.T) {
	// The provider still has the control track; it simply lacks the obscure ones.
	handler, _ := serveKnownGoodOnly(t)
	c, _ := newTestClient(t, handler)

	// The successful lookup that records the known-good track.
	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}

	// Now drive well past the threshold on obscure material. Under the old
	// count-only rule this escalated at exactly ZeroResultThreshold.
	var err error
	for i := 0; i < ZeroResultThreshold+5; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
		if errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("miss %d reported ErrProviderUnavailable on a credential that is still serving; "+
				"this is the #767 false positive (P=0.742 on the measured population)", i+1)
		}
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v; want an ordinary ErrNotFound", err)
	}
}

// TestColdStartRevokedCredentialStillEscalates guards the case a probe-only
// design would BREAK, and it is the original #607 defect.
//
// A credential revoked before this process ever saw a hit never earns a control
// track, so there is nothing to probe. Falling back to the count is the only
// signal available; returning "not an outage" there would leave a dead
// credential undetected forever -- strictly worse than the behavior #767
// replaces.
func TestColdStartRevokedCredentialStillEscalates(t *testing.T) {
	c, _ := newTestClient(t, serveEmpty())

	var err error
	for i := 0; i < ZeroResultThreshold; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("a credential that never served a single hit did not escalate after %d misses; "+
			"with no control track the count is the ONLY available signal (#607)", ZeroResultThreshold)
	}
	// It must still satisfy ErrNotFound so existing benign-miss callers work.
	if !errors.Is(err, ErrNotFound) {
		t.Error("ErrProviderUnavailable must still wrap ErrNotFound")
	}
}

// TestLivenessProbeConfirmsRealOutage is the other half of the discrimination:
// when the credential genuinely dies, the probe MISSES on the known-good track
// and the outage is reported.
//
// Without this, "never escalates" would pass the test above and be useless.
func TestLivenessProbeConfirmsRealOutage(t *testing.T) {
	handler, _ := serveHitThenEmpty(t, 1)
	c, _ := newTestClient(t, handler)

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	// From here the server answers EVERYTHING with an empty envelope, including
	// the probe's re-fetch of the known-good track. That is a real outage.
	var err error
	for i := 0; i < ZeroResultThreshold; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v; want ErrProviderUnavailable -- the probe re-fetched a track the "+
			"credential had served and got nothing, which IS evidence of an outage", err)
	}
}

// TestLivenessProbeResetsTheRunOnSuccess asserts the probe does not merely
// suppress one escalation: a successful probe clears the counter, so the lane
// gets a fresh threshold rather than re-escalating on the very next miss.
//
// Without the reset the probe would fire on every subsequent lookup, costing a
// paced request each time -- turning a false alarm into a throughput problem.
func TestLivenessProbeResetsTheRunOnSuccess(t *testing.T) {
	handler, probes := serveKnownGoodOnly(t)
	c, _ := newTestClient(t, handler)

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	seeded := probes.Load()

	// Two full threshold-length runs.
	for i := 0; i < ZeroResultThreshold*2; i++ {
		if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"}); errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("miss %d escalated despite a live credential", i+1)
		}
	}

	// Exactly two probes: one per completed run. More would mean the counter is
	// not resetting and every miss past the threshold re-probes.
	if got := probes.Load() - seeded; got != 2 {
		t.Errorf("probe count = %d over two threshold runs; want 2 (a successful probe must reset the run)", got)
	}
}

// TestConcurrentMissesProbeOnce asserts the in-flight guard: a burst of
// concurrent misses crossing the threshold together must not each launch a
// probe. Each probe is a real paced request, so N probes for one question is a
// throughput cost with no extra information.
func TestConcurrentMissesProbeOnce(t *testing.T) {
	handler, probes := serveKnownGoodOnly(t)
	c, _ := newTestClient(t, handler)

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	seeded := probes.Load()

	// Walk to one short of the threshold serially, so the burst below crosses it.
	for i := 0; i < ZeroResultThreshold-1; i++ {
		_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
		}()
	}
	wg.Wait()

	// EXACTLY one, and the exactness is provable rather than a hope about
	// scheduling. The run sits at ZeroResultThreshold-1, so whichever goroutine
	// increments first reaches the threshold and wins probeInFlight; every other
	// goroutine either sees busy (returns false, no probe) or arrives after the
	// successful probe reset the counter to 0, where 7 remaining misses cannot
	// reach the threshold again.
	//
	// The earlier `got > 2` was wrong in the direction that matters: it also
	// passed on ZERO probes, which is the guard blocking the probe entirely --
	// precisely the failure this test exists to catch. Caught in review.
	if got := probes.Load() - seeded; got != 1 {
		t.Errorf("probe count = %d for 8 concurrent misses; want exactly 1 "+
			"(0 means the in-flight guard suppressed the probe entirely, >1 means it failed to collapse them)", got)
	}
}

// TestProbeTransportFailureDoesNotStorm covers the retry storm Copilot caught in
// review on PR #771.
//
// When the probe cannot reach the API, the run is genuinely unadjudicated -- but
// leaving the counter at the threshold means recordZeroResult keeps returning
// true, so EVERY subsequent miss launches another probe. That is a burst of real
// paced requests aimed at a provider that is already failing to answer.
//
// Backing the counter off spaces the retries out by roughly another threshold.
func TestProbeTransportFailureDoesNotStorm(t *testing.T) {
	hit := serveFixture(t, "type1_unsynced.xml")
	var probes atomic.Int32
	var breakProbe atomic.Bool

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil && r.FormValue("key_title") == "Known" {
			probes.Add(1)
			if breakProbe.Load() {
				// Not a valid XML envelope: the probe fails in decode, which is
				// the transport-ish failure class that says nothing about the
				// credential.
				w.Header().Set("Content-Type", "text/xml; charset=utf-8")
				_, _ = w.Write([]byte("}{ not xml"))
				return
			}
			hit(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(emptyResponse))
	})

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Known", ArtistName: "Good"}); err != nil {
		t.Fatalf("seed lookup: %v", err)
	}
	breakProbe.Store(true)
	seeded := probes.Load()

	// Drive a full threshold run plus a long tail of further misses. With no
	// backoff every one of those tail misses probes again.
	for i := 0; i < ZeroResultThreshold*2; i++ {
		_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Obscure", ArtistName: "Artist"})
		if errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("miss %d escalated on a FAILED probe; a probe that could not reach "+
				"the API is not evidence of a dead credential", i+1)
		}
	}

	// Without the backoff this is ~21 probes (one per miss past the threshold).
	// With it, the counter halves each time, so attempts are spaced out.
	if got := probes.Load() - seeded; got > 4 {
		t.Errorf("probe count = %d over %d misses after a failing probe; the backoff "+
			"should space retries out rather than probing on every miss", got, ZeroResultThreshold*2)
	}
	if got := probes.Load() - seeded; got == 0 {
		t.Error("no probe was attempted at all; the backoff must delay retries, not disable them")
	}
}
