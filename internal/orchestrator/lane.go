package orchestrator

import (
	"context"
	"log/slog"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/providers"
)

// escalationThreshold is the number of consecutive circuit trips (with zero
// intervening provider successes, after at least one earlier success) after
// which the throttle log escalates from a steady throttling note back to a Warn
// that the token, valid earlier this session, may now have expired. It mirrors
// the worker's prior constant of the same name (the classification moved here).
const escalationThreshold = 5

// Lane wraps a single resolve func with its own circuit breaker. It owns the
// breaker interaction (open-gate short-circuit, half-open probe note, the
// error classification+trip, benign-miss reset, success recording). The
// error->breaker policy is injected as classifyErr so a provider lane and a
// detector lane can bring different semantics over the same breaker machinery.
type Lane struct {
	name        string
	resolve     ResolveFunc
	breaker     *circuit.Breaker
	classifyErr func(l *Lane, err error) error
	pacer       providers.AdaptivePacer // optional; nil when the lane has no pacer
	// local marks a lane that resolves without an outbound provider request (the
	// detector lane today). It is surfaced per-attempt on models.LaneAttempt so
	// the worker can tell whether an item spent the provider-request pacing
	// budget (#534). The zero value is false, so a lane is treated as remote
	// unless it opts in -- a new lane cannot accidentally suppress pacing.
	local bool
}

// Name reports the lane's name.
func (l *Lane) Name() string { return l.name }

// Local reports whether the lane resolves without an outbound provider request.
func (l *Lane) Local() bool { return l.local }

// Breaker exposes the lane's breaker (construction + tests asserting ramp state).
func (l *Lane) Breaker() *circuit.Breaker { return l.breaker }

// FindLyrics drives the lane's breaker around a resolve call. An open breaker
// returns ErrLaneUnavailable without calling resolve. Errors run through the
// lane's injected classifier; success records recovery and pacer stabilization.
func (l *Lane) FindLyrics(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
	switch l.breaker.Allow() {
	case circuit.StateOpen:
		return models.Song{}, ErrLaneUnavailable
	case circuit.StateHalfOpen:
		slog.Debug("lane circuit half-open; probing", "lane", l.Name())
	case circuit.StateClosed:
	}

	song, err := l.resolve(ctx, track, sourcePath)
	if err != nil {
		return models.Song{}, l.classifyErr(l, err)
	}

	if l.breaker.RecordSuccess() {
		slog.Info("lane circuit closed; recovered", "lane", l.Name())
	}
	l.notifySuccess()
	return song, nil
}

// notifyThrottle forwards a throttle notification to the lane's pacer, if any.
func (l *Lane) notifyThrottle() {
	if l.pacer != nil {
		l.pacer.OnThrottle()
	}
}

// notifySuccess forwards a success notification to the lane's pacer, if any.
func (l *Lane) notifySuccess() {
	if l.pacer != nil {
		l.pacer.OnSuccess()
	}
}
