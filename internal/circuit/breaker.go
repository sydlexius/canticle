// Package circuit provides a concurrency-safe circuit breaker that models a
// single provider lane's rate-limit / throttle response. It was extracted
// verbatim from the worker's inline breaker (the fields and tripCircuitIfRateLimited
// logic) so per-provider concurrency can compose one breaker per lane without
// the data race the inline state would otherwise carry.
//
// The breaker is closed normally, opens for a geometrically-ramping window on a
// throttle trip, and after the window elapses transitions to half-open (probing)
// until a successful round-trip closes it again. All state lives behind a single
// mutex; every method performs its full read-modify-write under the lock.
package circuit

import (
	"sync"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/backoff"
)

// BreakerState reports whether a caller may proceed through the breaker.
type BreakerState int

const (
	// StateClosed means the breaker is healthy; the caller may proceed normally.
	StateClosed BreakerState = iota
	// StateOpen means the open window has not yet elapsed; the caller must not proceed.
	StateOpen
	// StateHalfOpen means the window has elapsed and the caller is probing the
	// provider; it may proceed, and the next round-trip either closes the breaker
	// (success) or reopens it (a fresh trip).
	StateHalfOpen
)

// TripResult reports the effect of a trip so the caller can log it.
type TripResult struct {
	// Trips is the post-increment consecutive-trip count (0 for a renewal trip,
	// which deliberately does not advance the throttle ramp).
	Trips int
	// Window is the open duration applied by this trip.
	Window time.Duration
	// OpenUntil is the wall-clock instant until which the breaker is open.
	OpenUntil time.Time
}

// Breaker is a concurrency-safe circuit breaker for one provider lane.
type Breaker struct {
	mu sync.Mutex

	backoffBase  time.Duration
	openDuration time.Duration
	now          func() time.Time

	openUntil           time.Time
	consecutiveTrips    int
	probing             bool
	everProviderSuccess bool
}

// New creates a closed breaker. backoffBase is the trip-1 window; openDuration
// is the cap that the geometric ramp climbs to and also the flat window a
// renewal trip applies immediately.
func New(backoffBase, openDuration time.Duration) *Breaker {
	return &Breaker{
		backoffBase:  backoffBase,
		openDuration: openDuration,
		now:          time.Now,
	}
}

// SetClock injects the time source. It is used by the worker (to share its
// clock so time-based tests stay deterministic) and by unit tests.
func (b *Breaker) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now != nil {
		b.now = now
	}
}

// SetBackoff overrides the geometric window parameters. base is the trip-1
// window; cap is the ceiling (and the renewal window). Zero or negative values
// are ignored so a misconfigured call cannot disable the window.
func (b *Breaker) SetBackoff(base, cap time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if base > 0 {
		b.backoffBase = base
	}
	if cap > 0 {
		b.openDuration = cap
	}
}

// SetOpenDuration overrides only the cap window. Values <= 0 are ignored.
func (b *Breaker) SetOpenDuration(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > 0 {
		b.openDuration = d
	}
}

// Allow reports the current state, performing the window-elapsed transition to
// half-open (setting probing and clearing openUntil) as a side effect, exactly
// as the worker's RunOnce gate did.
func (b *Breaker) Allow() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openUntil.IsZero() {
		if b.now().Before(b.openUntil) {
			return StateOpen
		}
		// Window elapsed: enter half-open.
		b.probing = true
		b.openUntil = time.Time{}
	}
	if b.probing {
		return StateHalfOpen
	}
	return StateClosed
}

// OpenUntil returns the instant until which the breaker is open (zero when not
// open). Useful for callers that log the window.
func (b *Breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil
}

// Trip records a throttle trip: it increments the consecutive-trip counter,
// computes the geometric window between backoffBase and openDuration, sets
// openUntil, and clears probing (a fresh open is full-open, not a probe).
func (b *Breaker) Trip() TripResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveTrips++
	window := backoff.Geometric(b.consecutiveTrips, b.backoffBase, b.openDuration)
	b.openUntil = b.now().Add(window)
	b.probing = false
	return TripResult{Trips: b.consecutiveTrips, Window: window, OpenUntil: b.openUntil}
}

// TripRenewal records a genuine token-renewal signal: it opens for the full cap
// (openDuration) immediately and does NOT advance the throttle ramp, so a later
// real throttle resumes from its true position. It clears probing.
func (b *Breaker) TripRenewal() TripResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openUntil = b.now().Add(b.openDuration)
	b.probing = false
	return TripResult{Trips: b.consecutiveTrips, Window: b.openDuration, OpenUntil: b.openUntil}
}

// RecordSuccess records a genuine provider success: it sets everProviderSuccess,
// resets the trip ramp, and clears probing. It returns whether the breaker
// transitioned out of probing (so the caller can log recovery only on the real
// transition).
func (b *Breaker) RecordSuccess() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.everProviderSuccess = true
	b.consecutiveTrips = 0
	transitioned := b.probing
	b.probing = false
	return transitioned
}

// RecordBenignMiss records a clean miss: a successful round-trip but NOT a
// token-proven success. It resets the trip ramp and clears probing, but does
// NOT set everProviderSuccess. It returns whether it transitioned out of probing.
func (b *Breaker) RecordBenignMiss() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveTrips = 0
	transitioned := b.probing
	b.probing = false
	return transitioned
}

// Trips reports the current consecutive-trip count (the throttle ramp position).
// It is a read-only accessor for callers and tests that need to observe the ramp.
func (b *Breaker) Trips() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutiveTrips
}

// EverSucceeded reports whether any genuine provider fetch has succeeded this
// session. It distinguishes a bare 401 that is egress-IP throttling (token
// already proven good) from one seen before any success (token suspect).
func (b *Breaker) EverSucceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.everProviderSuccess
}
