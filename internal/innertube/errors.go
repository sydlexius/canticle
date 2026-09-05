package innertube

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the innertube client. Callers should use
// errors.Is to test for these classes rather than string-matching the
// message. These mirror the classes exposed by internal/musixmatch and
// internal/petitlyrics so all three providers can be handled uniformly by the
// worker and circuit breaker.
var (
	// ErrUnauthorized indicates HTTP 401 from the innertube API.
	ErrUnauthorized = errors.New("innertube: unauthorized")

	// ErrRateLimited indicates HTTP 429 from the innertube API.
	//
	// HTTP 403 is deliberately NOT mapped here, mirroring
	// internal/petitlyrics.ErrRateLimited (issue #495): a request refused at
	// the door is a different condition from one that was throttled, and
	// conflating them once sent an investigation after a phantom rate limit.
	// 403 maps to ErrForbidden instead so the two stay distinguishable.
	ErrRateLimited = errors.New("innertube: rate limited")

	// ErrForbidden indicates HTTP 403: the request was refused rather than
	// throttled. Kept separate from ErrRateLimited because the remedies
	// differ (a refused request shape is a client bug; throttling is a
	// pacing problem).
	ErrForbidden = errors.New("innertube: forbidden")

	// ErrClientVersion indicates the measured ANDROID_MUSIC 5.16.51 HTTP 400
	// case for a stale client version -- see doc.go. It wraps ErrForbidden,
	// mirroring how ErrNoLyricsTab wraps ErrNotFound below: once #854 adds an
	// innertube arm to orchestrator/resolve.go's lane-circuit-breaker switch,
	// a caller that buckets broadly on ErrForbidden there will keep working
	// unchanged, while code that cares about the distinction sees the real
	// reason. Today nothing outside this package references innertube's
	// sentinels at all, so that benefit is not yet realized -- this wrapping
	// is prepared for #854's wiring, not a claim that the wiring exists. This
	// is deliberately NOT the same bucket as a generic malformed-request 400
	// (bad body or params) -- the remedy for a stale client version is
	// "bump the client version", not "fix the request shape" -- but this
	// seed does not attempt to distinguish the two at the type level; #854
	// (which owns status-code mapping) decides how a raw 400 maps here.
	ErrClientVersion = fmt.Errorf("innertube: stale client version: %w", ErrForbidden)

	// ErrNotFound indicates the lookup reached the API and got a clean
	// answer, but no usable lyrics resulted. Every "reached it, nothing
	// usable" path in this package wraps ErrNotFound, matching the
	// convention in internal/petitlyrics: a search returning no results, a
	// video with no lyrics tab, and a browse response with no cues are all
	// clean misses, not failures.
	ErrNotFound = errors.New("innertube: no results found")

	// ErrNoLyricsTab indicates the next call for a videoId returned no
	// lyrics-tab browseId at all -- the video exists but innertube does not
	// offer a lyrics rendition for it. It wraps ErrNotFound: existing callers
	// that bucket a miss as benign keep working unchanged, while code that
	// explicitly tests for this sentinel sees the more specific reason.
	ErrNoLyricsTab = fmt.Errorf("innertube: no lyrics tab for video: %w", ErrNotFound)
)
