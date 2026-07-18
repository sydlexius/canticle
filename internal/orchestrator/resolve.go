package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/doxazo-net/canticle/internal/circuit"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/musixmatch"
	"github.com/doxazo-net/canticle/internal/providers"
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

	if musixmatch.IsBenignMiss(err) || errors.Is(err, musixmatch.ErrTruncatedResponse) {
		// A clean miss proves the provider round-trip succeeded, so we are not
		// being throttled: reset the ramp. EverSucceeded is deliberately NOT set
		// (a miss is a successful round-trip but not a genuine lyric match). A
		// truncated/empty body is bucketed here too (#496): it is a deterministic
		// per-request condition, not a transient throttle, so it must not trip the
		// breaker or ratchet the pacer -- the worker's benign-miss cadence bounds
		// its cost instead.
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
//
//nolint:unused // foundation for the detector lane wired in a later task (#501 series); not yet referenced.
func detectorClassifier(l *Lane, err error) error {
	switch {
	case errors.Is(err, ErrLaneBenignMiss):
		if l.breaker.RecordBenignMiss() {
			slog.Info("lane circuit closed; recovered", "lane", l.Name())
		}
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
