package petitlyrics

import "errors"

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
	// ErrUnsupportedTier indicates the API returned a lyrics tier this client
	// cannot decode yet -- specifically lyricsType 2, whose payload is an
	// encrypted LSY binary blob. Callers should treat it as a miss for this tier
	// rather than failing the track outright.
	ErrUnsupportedTier = errors.New("petitlyrics: unsupported lyrics tier")
)
