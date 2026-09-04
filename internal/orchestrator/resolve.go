package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
)

// ResolveFunc produces a Song for a track. sourcePath is the on-disk audio path
// from the work item (empty for provider lanes, which do not read the file); a
// file-reading lane such as the detector uses it to locate the audio to classify.
type ResolveFunc func(ctx context.Context, track models.Track, sourcePath string) (models.Song, error)

// NewProviderLane builds a lane over a named lyrics provider and its dedicated
// breaker. The provider is adapted to a ResolveFunc that ignores sourcePath, and
// the lane uses the provider-aware classifier (musixmatch throttle/auth/miss
// semantics) plus the provider's optional adaptive-pacer hooks.
func NewProviderLane(p providers.LyricsProvider, breaker *circuit.Breaker) *Lane {
	pacer, _ := p.(providers.AdaptivePacer)
	return &Lane{
		name:    p.Name(),
		breaker: breaker,
		resolve: func(ctx context.Context, track models.Track, _ string) (models.Song, error) {
			return p.FindLyrics(ctx, track)
		},
		classifyErr: providerClassifier,
		pacer:       pacer,
	}
}

// providerClassifier drives the breaker for a provider lane's error outcome and
// returns the error unchanged so the orchestrator can rank it. It preserves the
// worker's prior classification order and honest-401 logging.
func providerClassifier(l *Lane, err error) error {
	// A genuine token renewal must be tested BEFORE the bare-401 check: a renewal
	// also satisfies errors.Is(_, ErrUnauthorized), so testing ErrUnauthorized
	// first would wrongly fold a renewal into the throttle ramp. A renewal holds
	// the full window, stays loud, and does NOT advance the throttle counter.
	if errors.Is(err, musixmatch.ErrTokenRenewalRequired) {
		res := l.breaker.TripRenewal()
		slog.Warn("lane circuit opened: token renewal required; regenerate the usertoken",
			"provider", l.Name(), "backoff", res.Window, "next_retry", res.OpenUntil, "cause", err)
		return err
	}

	if errors.Is(err, musixmatch.ErrRateLimited) ||
		errors.Is(err, musixmatch.ErrUnauthorized) {
		res := l.breaker.Trip()
		switch {
		case l.breaker.EverSucceeded() && res.Trips >= escalationThreshold:
			slog.Warn("lane circuit opened: token validated earlier this session but has failed repeatedly; it may have expired",
				"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		case l.breaker.EverSucceeded():
			slog.Warn("lane circuit opened: provider throttling; token validated earlier this session",
				"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		default:
			slog.Warn("lane circuit opened: no successful fetch yet this session; verify your token",
				"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		}
		// Ratchet the adaptive pacer only on genuine throttle signals: a rate-limit
		// is ALWAYS throttling; a 401 is throttling only AFTER the token has
		// succeeded this session (before that it's a bad token, not a throttle).
		// Never ratchet on a never-succeeded 401.
		if errors.Is(err, musixmatch.ErrRateLimited) ||
			(errors.Is(err, musixmatch.ErrUnauthorized) && l.breaker.EverSucceeded()) {
			l.notifyThrottle()
		}
		return err
	}

	// Petit Lyrics fault sentinels. These are handled separately from the
	// musixmatch block above rather than folded into it, because the two
	// providers do not share a throttle model: musixmatch distinguishes a
	// never-succeeded 401 (a bad token) from a post-success 401 (an egress
	// throttle) and ratchets the adaptive pacer only in the latter case, while
	// petitlyrics has no token at all -- its 401 means the hardcoded clientAppId
	// was rejected, which is never a throttle and must never ratchet the pacer.
	//
	// Before #607 none of these were classified here, so every petitlyrics
	// failure fell through to the transport branch below and left the breaker
	// untouched. The lane could not trip on any condition.
	switch {
	// ORDER IS LOAD-BEARING: ErrProviderUnavailable WRAPS ErrNotFound (so that
	// existing callers bucketing a miss as benign keep working), which means it
	// also satisfies the benign-miss case further down. Tested after it, a
	// sustained outage would RESET the ramp instead of opening the lane -- the
	// exact silent degradation #607 exists to end.
	case errors.Is(err, petitlyrics.ErrProviderUnavailable):
		// Deliberate decision (#607 asks for this to be chosen, not inherited):
		// the sentinel TRIPS the breaker. A sustained run of zero-result
		// responses almost certainly means the hardcoded application id was
		// revoked, and continuing to fire paced requests at a provider that
		// answers nothing is pure waste against a shared egress. The breaker's
		// backoff re-probes periodically, so a transient cause still recovers
		// with no operator action.
		//
		// It does NOT ratchet the pacer: a dead credential is not a throttle, and
		// slowing a lane that is not being rate limited would persist after the
		// credential is restored.
		res := l.breaker.Trip()
		slog.Warn("lane circuit opened: provider returned no results for a sustained run of lookups; the application id may have been revoked",
			"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		return err

	case errors.Is(err, petitlyrics.ErrUnauthorized):
		res := l.breaker.Trip()
		slog.Warn("lane circuit opened: provider rejected the client application id; the lane is down until it is restored",
			"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		return err

	case errors.Is(err, petitlyrics.ErrForbidden):
		// A refused request SHAPE, not throttling (internal/petitlyrics/errors.go
		// keeps 403 distinct from 429 for exactly this reason -- see #495, where a
		// User-Agent denylist rejection read as a phantom rate limit and sent the
		// investigation after the wrong cause). Retrying an unchanged request
		// cannot succeed, so open the lane rather than spend requests on it.
		res := l.breaker.Trip()
		slog.Warn("lane circuit opened: provider refused the request shape (not throttling); a client change is required",
			"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		return err

	case errors.Is(err, petitlyrics.ErrRateLimited):
		res := l.breaker.Trip()
		slog.Warn("lane circuit opened: provider throttling",
			"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
		// An explicit 429 is an unambiguous throttle signal, so it ratchets the
		// pacer -- unlike the 401 above.
		l.notifyThrottle()
		return err

	case errors.Is(err, petitlyrics.ErrNotFound), errors.Is(err, petitlyrics.ErrUnsupportedTier):
		// Both are healthy round trips. ErrNotFound is a clean miss;
		// ErrUnsupportedTier means the response arrived fine and its payload was
		// an undecodable tier (lyricsType 2, the encrypted LSY blob). Neither says
		// anything about lane health, so both reset the ramp.
		//
		// EverSucceeded is deliberately NOT set, matching the musixmatch branch: a
		// miss is a successful round trip but not a genuine lyric match.
		if l.breaker.RecordBenignMiss() {
			slog.Info("lane circuit closed; provider recovered", "provider", l.Name())
		}
		return err
	}

	if musixmatch.IsBenignMiss(err) || errors.Is(err, musixmatch.ErrTruncatedResponse) ||
		errors.Is(err, musixmatch.ErrUnparsableSubtitleBody) ||
		errors.Is(err, musixmatch.ErrMatchMismatch) {
		// A clean miss proves the provider round-trip succeeded, so we are not
		// being throttled: reset the ramp. EverSucceeded is deliberately NOT set
		// (a miss is a successful round-trip but not a genuine lyric match). A
		// truncated/empty body is bucketed here too (#496): it is a deterministic
		// per-request condition, not a transient throttle, so it must not trip the
		// breaker or ratchet the pacer -- the worker's benign-miss cadence bounds
		// its cost instead. An unrecognized subtitle_body encoding (#838) joins it:
		// the request returned HTTP 200 with a complete body, so the round trip
		// demonstrably succeeded and a throttle response would be a misreading.
		if l.breaker.RecordBenignMiss() {
			slog.Info("lane circuit closed; provider recovered", "provider", l.Name())
		}
		return err
	}

	// Transport / unexpected error: not a throttle signal, leave the breaker
	// untouched. Wrap for context parity with the prior worker path.
	return fmt.Errorf("lane %s: find lyrics: %w", l.Name(), err)
}

// detectorClassifier is the error->breaker policy for a detector lane. A benign
// miss (gate-negative) resets the ramp; an outage trips the breaker; any other
// error is wrapped transport and leaves the breaker untouched.
func detectorClassifier(l *Lane, err error) error {
	switch {
	case errors.Is(err, ErrLaneBenignMiss):
		if l.breaker.RecordBenignMiss() {
			slog.Info("lane circuit closed; recovered", "lane", l.Name())
		}
		return err
	case errors.Is(err, ErrLaneNotReady):
		// Startup race: the sidecar is still booting. Do NOT trip the breaker, so
		// the next work cycle re-attempts the lane once the sidecar is up. The
		// worker releases the item penalty-free (no miss, no cooldown) (#567).
		// (The default branch below would also leave the breaker closed, but this
		// explicit case gives a distinct log and pins the intent against future
		// edits to that branch.)
		slog.Debug("detector lane not ready (sidecar starting up); leaving circuit closed",
			"lane", l.Name(), "cause", err)
		return err
	case errors.Is(err, ErrLaneOutage):
		res := l.breaker.Trip()
		slog.Warn("lane circuit opened: detector outage; degrading to providers",
			"lane", l.Name(), "trips", res.Trips, "backoff", res.Window, "next_retry", res.OpenUntil, "cause", err)
		return err
	default:
		return fmt.Errorf("lane %s: resolve: %w", l.Name(), err)
	}
}
