package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sydlexius/mxlrcgo-svc/internal/circuit"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
)

// escalationThreshold is the number of consecutive circuit trips (with zero
// intervening provider successes, after at least one earlier success) after
// which the throttle log escalates from a steady throttling note back to a Warn
// that the token, valid earlier this session, may now have expired. It mirrors
// the worker's prior constant of the same name (the classification moved here).
const escalationThreshold = 5

// Lane wraps a single lyrics provider with its own circuit breaker. It owns the
// breaker interaction that previously lived inline in the worker: the open-gate
// short-circuit, the half-open probe note, the throttle classification and
// trip, the benign-miss reset, and the success/recovery recording. The breaker
// is provider-agnostic; the rate-limit / auth / renewal classification is
// per-provider and lives here.
//
// A Lane satisfies providers.Fetcher, so the orchestrator (and, with a single
// lane, the worker) can treat it as a drop-in fetcher.
type Lane struct {
	provider providers.LyricsProvider
	breaker  *circuit.Breaker
}

// NewLane builds a lane over a named provider and its dedicated breaker.
func NewLane(provider providers.LyricsProvider, breaker *circuit.Breaker) *Lane {
	return &Lane{provider: provider, breaker: breaker}
}

// Name reports the underlying provider's name.
func (l *Lane) Name() string { return l.provider.Name() }

// Breaker exposes the lane's breaker. It is used by construction (to share the
// worker's configured breaker) and by tests that assert ramp/recovery state.
func (l *Lane) Breaker() *circuit.Breaker { return l.breaker }

// FindLyrics drives the lane's breaker around a provider fetch:
//
//   - If the breaker is open, it returns ErrLaneUnavailable WITHOUT calling the
//     provider, so an open lane spends no calls.
//   - On a benign miss it resets the breaker ramp (a clean miss is a successful
//     round-trip, not a throttle) and returns the classified miss error.
//   - On a rate-limit / auth / renewal signal it trips the breaker (or holds the
//     full renewal window) and returns the classified error after emitting the
//     honest four-way throttle log.
//   - On any other (transport) error it returns the error without touching the
//     breaker (a transport failure is not a throttle signal).
//   - On success it records the success (recovering the breaker if it was
//     probing) and returns the song.
func (l *Lane) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	switch l.breaker.Allow() {
	case circuit.StateOpen:
		return models.Song{}, ErrLaneUnavailable
	case circuit.StateHalfOpen:
		// Half-open probe: the first real provider call after the window elapsed.
		slog.Debug("lane circuit half-open; probing provider", "provider", l.Name())
	case circuit.StateClosed:
	}

	song, err := l.provider.FindLyrics(ctx, track)
	if err != nil {
		return models.Song{}, l.classify(err)
	}

	if l.breaker.RecordSuccess() {
		slog.Info("lane circuit closed; provider recovered", "provider", l.Name())
	}
	return song, nil
}

// classify drives the breaker for an error outcome and returns the error
// unchanged so the orchestrator can rank it. It preserves the worker's prior
// classification order and honest-401 logging.
func (l *Lane) classify(err error) error {
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
		errors.Is(err, musixmatch.ErrUnauthorized) ||
		errors.Is(err, musixmatch.ErrTruncatedResponse) {
		res := l.breaker.Trip()
		switch {
		case errors.Is(err, musixmatch.ErrTruncatedResponse):
			slog.Warn("lane circuit opened: provider returned truncated response; likely throttling",
				"provider", l.Name(), "trips", res.Trips, "cause", err, "backoff", res.Window, "next_retry", res.OpenUntil)
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
		return err
	}

	if musixmatch.IsBenignMiss(err) {
		// A clean miss proves the provider round-trip succeeded, so we are not
		// being throttled: reset the ramp. EverSucceeded is deliberately NOT set
		// (a miss is a successful round-trip but not a genuine lyric match).
		if l.breaker.RecordBenignMiss() {
			slog.Info("lane circuit closed; provider recovered", "provider", l.Name())
		}
		return err
	}

	// Transport / unexpected error: not a throttle signal, leave the breaker
	// untouched. Wrap for context parity with the prior worker path.
	return fmt.Errorf("lane %s: find lyrics: %w", l.Name(), err)
}
