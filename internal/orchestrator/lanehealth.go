package orchestrator

import (
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
)

// Lane circuit states reported by LaneHealth.
const (
	LaneStateClosed   = "closed"
	LaneStateOpen     = "open"
	LaneStateHalfOpen = "half-open"
)

// LaneState is a read-only view of one lane's circuit breaker.
type LaneState struct {
	// Provider is the lane name. In ordered mode the lane set includes the
	// instrumental detector lane, so Provider may name it; use Local to tell
	// it apart from a lyrics provider.
	Provider string
	// Local is true for a lane that resolves without an outbound provider
	// request (the instrumental detector), false for a lyrics provider.
	Local bool
	// State is LaneStateClosed, LaneStateOpen or LaneStateHalfOpen. Half-open
	// (probing) is reported separately from open: the lane will accept the next
	// request even though it recently tripped.
	State string
	// OpenUntil is the end of the open window; zero unless State is open.
	OpenUntil time.Time
	// Trips is the consecutive-trip count (the throttle ramp position).
	Trips int
	// EverSucceeded reports any successful resolve on this lane this session.
	EverSucceeded bool
}

// LaneHealth reports every lane's circuit state in lane (priority) order.
//
// It is read-only and takes only a short per-breaker lock; it never calls out. Each lane is read through
// circuit.Breaker.Snapshot, which takes only that breaker's own short lock,
// never mutates (so observing never consumes a half-open transition), and no
// orchestrator state is locked at all (the lane slice is fixed at New).
// Safe for concurrent use with dispatch.
//
// Production caller: /metrics through worker.LaneHealth (#488 slice 2); the
// dashboard (slice 3) will consume it too.
func (o *Orchestrator) LaneHealth() []LaneState {
	out := make([]LaneState, 0, len(o.lanes))
	for _, l := range o.lanes {
		s := l.breaker.Snapshot()
		out = append(out, LaneState{
			Provider:      l.name,
			Local:         l.Local(),
			State:         laneStateName(s.State),
			OpenUntil:     s.OpenUntil,
			Trips:         s.Trips,
			EverSucceeded: s.EverSucceeded,
		})
	}
	return out
}

func laneStateName(s circuit.BreakerState) string {
	switch s {
	case circuit.StateOpen:
		return LaneStateOpen
	case circuit.StateHalfOpen:
		return LaneStateHalfOpen
	default:
		return LaneStateClosed
	}
}
