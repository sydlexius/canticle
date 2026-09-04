package petitlyrics

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the Petit Lyrics client. Callers should use
// errors.Is to test for these classes rather than string-matching the message.
// These mirror the classes exposed by internal/musixmatch so the two providers
// can be handled uniformly by the worker and circuit breaker.
var (
	// ErrUnauthorized indicates HTTP 401 from the Petit Lyrics API. Treat as a
	// circuit-breaker signal.
	ErrUnauthorized = errors.New("petitlyrics: unauthorized")
	// ErrRateLimited indicates HTTP 429 from the Petit Lyrics API.
	//
	// HTTP 403 is deliberately NOT mapped here. The previous web-scrape client
	// mapped 403 -> rate limited, which made a User-Agent denylist rejection
	// (issue #495: 7/7 requests refused at the door) read as throttling in every
	// log line and sent the investigation after a phantom rate limit. 403 now
	// maps to ErrForbidden so the two stay distinguishable.
	ErrRateLimited = errors.New("petitlyrics: rate limited")
	// ErrForbidden indicates HTTP 403: the request was refused rather than
	// throttled. Kept separate from ErrRateLimited because the remedies differ
	// (a refused request shape is a client bug; throttling is a pacing problem).
	ErrForbidden = errors.New("petitlyrics: forbidden")
	// ErrNotFound indicates the API returned no matching song, meaning no usable
	// lyrics were found. This is a clean miss, not a failure.
	ErrNotFound = errors.New("petitlyrics: no results found")
)

// ErrProviderUnavailable indicates a sustained run of zero-result responses:
// the request shape is valid and the API keeps answering HTTP 200 with
// well-formed XML, but every response carries no songs at all (#607).
//
// This exists because a revoked clientAppId does NOT produce a 401. The service
// keeps replying normally with an empty song list, which is byte-identical to a
// genuine miss -- so a total lane outage would otherwise present as "the
// provider suddenly has none of my tracks", indefinitely, with the lane looking
// healthy the whole time.
//
// It WRAPS ErrNotFound deliberately. Every existing caller that buckets a miss
// as benign keeps working unchanged; only code that explicitly tests for this
// sentinel sees the escalation. That mirrors how musixmatch.ErrTokenRenewalRequired
// also satisfies errors.Is(_, ErrUnauthorized).
var ErrProviderUnavailable = fmt.Errorf(
	"petitlyrics: provider returned no results for %d consecutive lookups (application id revoked?): %w",
	ZeroResultThreshold, ErrNotFound)

// ZeroResultThreshold is how many CONSECUTIVE zero-result lookups escalate to
// ErrProviderUnavailable.
//
// Sizing: this lane runs as a fallback, so it only ever sees tracks the primary
// provider already missed, and its measured coverage on that population is low
// (roughly 1 in 4). A run of 8 consecutive misses is therefore entirely ordinary
// -- at a 25% hit rate any given 8-lookup window is all-misses about 10% of the
// time -- while a given 20-lookup window is under 0.4%. At the client's 30s
// pacing floor, 20 lookups is about 10 minutes to detection.
//
// Read that per-WINDOW figure correctly: it is not the false-positive rate over
// a long run. Some run of 20 is expected roughly every 1,250 lookups at that hit
// rate, or about half a day of sustained fallback traffic, so a large library
// scan should expect to trip this occasionally without any provider fault.
//
// That is tolerable because recovery is HIT-DRIVEN, not count-driven: the first
// non-zero response clears the counter and the latch outright. But note the
// counter keeps climbing past the threshold, so every further zero-result
// re-trips the breaker and ramps its geometric backoff toward the 30-minute cap.
// A long genuine dry spell therefore escalates to a real lane pause, which is
// the correct behavior for an actual outage and merely conservative for a dry
// spell -- the lane is returning nothing either way.
//
// The counter is CONSECUTIVE, never cumulative: any single non-zero response
// clears it. A cumulative count would eventually escalate on any long-lived
// client no matter how healthy the provider is.
const ZeroResultThreshold = 20
