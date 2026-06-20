package providers

// AdaptivePacer is an optional interface a provider (or its underlying fetcher)
// may implement to receive throttle and success notifications from the
// orchestration layer. It lets the provider self-tune its effective request
// interval to whatever the egress IP's throttle threshold turns out to be,
// instead of relying on a hand-tuned static cooldown.
//
// The interface is optional: callers must use a type assertion to check for
// support before invoking these methods. A provider that does not implement
// AdaptivePacer simply receives no notifications.
type AdaptivePacer interface {
	// OnThrottle notifies the pacer that a throttle-attributable failure
	// occurred (e.g. a circuit trip on a 401 egress-IP throttle). It takes no
	// parameter on purpose: the pacer maintains its OWN ratcheting level that is
	// independent of the circuit breaker's trip count. Passing the breaker's
	// trip count instead would snap the multiplier back to 1x on every circuit
	// recovery (trips reset when the breaker closes), which is the exact
	// sawtooth bug this adaptive pacing fixes. The pacer's level only steps down
	// after a sustained clean streak (see OnSuccess).
	OnThrottle()

	// OnSuccess notifies the pacer that a genuine lyric fetch succeeded. After a
	// sustained streak of successes the pacer eases its effective interval back
	// down by one step.
	OnSuccess()
}
