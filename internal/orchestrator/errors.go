package orchestrator

import (
	"errors"

	"github.com/sydlexius/canticle/internal/musixmatch"
)

// ErrLaneUnavailable is the sentinel a lane returns when its breaker is open and
// the provider was therefore not called. The orchestrator surfaces it as the
// dispatch outcome only when EVERY available lane is unavailable, in which case
// the worker releases the item back to pending with no failure increment (no
// lane was actually consulted, so the catalog answer is unknown).
var ErrLaneUnavailable = errors.New("orchestrator: lane unavailable (circuit open)")

// ErrLaneBenignMiss is the provider-agnostic sentinel a non-provider lane
// returns when it reached its backend and found no usable result (e.g. the
// detector gate is negative). It classifies as a benign miss: the ramp resets
// and the remaining lanes run.
var ErrLaneBenignMiss = errors.New("orchestrator: lane benign miss (no result)")

// ErrLaneOutage is the provider-agnostic sentinel a non-provider lane returns
// when its backend call genuinely failed (e.g. the detector sidecar is
// unreachable). It trips the lane's breaker so repeated outages open the lane
// and it degrades to OutcomeUnavailable.
var ErrLaneOutage = errors.New("orchestrator: lane outage")

// OutcomeClass classifies a lane's outcome for cross-lane precedence (design
// doc Gap 4). The precedence rule is "least-certain-negative wins": any signal
// that we did not truly learn the track is absent (auth, rate-limit, transport,
// open circuit) outranks the only signal that says the track is absent (a
// benign miss).
type OutcomeClass int

const (
	// OutcomeSuccess means the lane returned lyrics (suitable or not). It carries
	// no error precedence.
	OutcomeSuccess OutcomeClass = iota
	// OutcomeBenignMiss means the lane reached the provider and the track was
	// absent or had no usable lyrics (ErrNotFound, ErrNoLyrics), or the
	// provider returned a structurally valid but hollow body
	// (ErrTruncatedResponse). ErrTruncatedResponse is deterministic per
	// request rather than a genuine catalog answer, but it is bucketed here
	// (not with the throttle signals below) because it must take the same
	// bounded-retry path as a clean miss -- see #496.
	OutcomeBenignMiss
	// OutcomeLaneOutage means a NON-PROVIDER lane (today: the detector) reached
	// for its backend and the call genuinely failed (ErrLaneOutage). It ranks
	// above a benign miss - we did not cleanly learn anything from that lane -
	// but deliberately BELOW OutcomeTransport, because such a lane answers a
	// different question than the providers do. A detector outage says nothing
	// about whether the track has lyrics, so it must never outrank, or tie and
	// then mask, a provider's own transport failure: at equal precedence rankErr
	// keeps whichever lane reported FIRST, so a front-ordered detector outage
	// would otherwise become the surfaced error and let the worker downgrade a
	// genuine provider failure to a benign miss, suppressing its backoff.
	OutcomeLaneOutage
	// OutcomeTransport means a retriable failure that is not a clean miss
	// (timeout, connection failure, an unexpected error).
	OutcomeTransport
	// OutcomeAuthRateLimit means an auth or rate-limit / throttle signal
	// (ErrUnauthorized, ErrTokenRenewalRequired, ErrRateLimited). The catalog
	// answer is unknown.
	OutcomeAuthRateLimit
	// OutcomeUnavailable means the lane's breaker was open and the provider was
	// not called (ErrLaneUnavailable).
	OutcomeUnavailable
)

// ClassifyOutcome maps a lane error to its OutcomeClass. A nil error is a
// success. The auth/rate-limit check folds in the Musixmatch throttle sentinels
// the worker historically tripped the circuit on; classification is per-provider
// today (only Musixmatch lanes exist) and lives here so the breaker stays
// provider-agnostic. ErrTruncatedResponse is deliberately NOT in the
// auth/rate-limit bucket: it is a deterministic per-request condition (an
// empty body), not a transient throttle, so it must classify as a benign miss
// and take the bounded-retry path rather than the no-cost throttle release
// (#496).
func ClassifyOutcome(err error) OutcomeClass {
	switch {
	case err == nil:
		return OutcomeSuccess
	case errors.Is(err, ErrLaneUnavailable):
		return OutcomeUnavailable
	case errors.Is(err, musixmatch.ErrTokenRenewalRequired),
		errors.Is(err, musixmatch.ErrUnauthorized),
		errors.Is(err, musixmatch.ErrRateLimited):
		return OutcomeAuthRateLimit
	case musixmatch.IsBenignMiss(err), errors.Is(err, musixmatch.ErrTruncatedResponse),
		errors.Is(err, ErrLaneBenignMiss):
		return OutcomeBenignMiss
	case errors.Is(err, ErrLaneOutage):
		return OutcomeLaneOutage
	default:
		return OutcomeTransport
	}
}

// precedence returns the cross-lane ranking weight. Higher wins. Unavailable is
// ranked at the auth/rate-limit tier for queue purposes (both release the item
// without recording a stable miss), but it remains a distinct class so the
// orchestrator can surface ErrLaneUnavailable when every lane was unavailable.
func (c OutcomeClass) precedence() int {
	switch c {
	case OutcomeSuccess:
		return 0
	case OutcomeBenignMiss:
		return 1
	case OutcomeLaneOutage:
		return 2
	case OutcomeTransport:
		return 3
	case OutcomeAuthRateLimit:
		return 4
	case OutcomeUnavailable:
		return 4
	default:
		return 0
	}
}
