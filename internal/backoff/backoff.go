// Package backoff provides shared retry-delay formulas used by the worker,
// the durable queue, and the legacy fetch loop. Keeping the formula in one
// place ensures all three surfaces present the same cadence to operators
// (1m, 2m, 4m, ..., capped at 1h with the defaults).
package backoff

import "time"

// Default base and cap durations for geometric backoff. Callers may override
// these via Geometric's parameters when they need a different cadence.
const (
	DefaultBase = time.Minute
	DefaultMax  = time.Hour
)

// DefaultMissBase is the initial re-check delay for a benign miss (no matching
// track or no usable lyrics). Doubling from here gives: 168h (7d), 336h (14d),
// 672h (28d, cap), then stays at DefaultMissCap.
const DefaultMissBase = 7 * 24 * time.Hour

// DefaultMissCap is the maximum re-check delay for a benign miss (28 days).
// Once miss_count is high enough for the geometric doubling to exceed this,
// every subsequent Defer uses this ceiling.
const DefaultMissCap = 28 * 24 * time.Hour

// MissCooldown returns the re-check delay for a benign miss given the current
// (post-increment) miss_count. It delegates to Geometric so the cadence is
// consistent with the worker's failure-backoff formula:
//
//	miss_count=1: base (168h / 7d default)
//	miss_count=2: 2*base (336h / 14d)
//	miss_count=3: 4*base (672h / 28d = cap)
//	...capped at max.
//
// A missCount < 1 is treated as 1 (same as Geometric). Zero or negative
// base/max returns 0.
func MissCooldown(missCount int, base, max time.Duration) time.Duration {
	return Geometric(missCount, base, max)
}

// Geometric returns the wait duration for the given 1-indexed attempt count,
// doubling from base each step and capping at max. Returns 0 if base or max
// is non-positive. Attempts < 1 are treated as 1.
func Geometric(attempts int, base, max time.Duration) time.Duration {
	if base <= 0 || max <= 0 {
		return 0
	}
	if attempts < 1 {
		attempts = 1
	}
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= max || delay > max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}
