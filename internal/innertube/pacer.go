package innertube

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// MinAllowedInterval is the hard floor on the pacing interval for any positive
// value, so a misconfigured cooldown cannot make this lane impolite.
//
// Set by POLICY, not read from the service. Google publishes nothing about the
// innertube surface at all -- no documentation, no rate-limit response headers,
// no robots.txt covering it -- so there is no published limit to honor and
// deliberately no probing of the service until it refuses. What the floor is
// calibrated against instead is organic use: YouTube Music's own web client
// issues several innertube calls to render a single page, so a two-second
// inter-request floor is orders of magnitude below what the gateway already
// serves one ordinary listener.
//
// It is a different number from internal/petitlyrics.MinAllowedInterval (10s)
// on purpose. That service is one vendor's small API; this is a Google-scale
// gateway. Copying the petitlyrics figure would have looked consistent and been
// unjustified in both directions.
const MinAllowedInterval = 2 * time.Second

// DefaultMinInterval is the recommended pacing interval: 10s between outbound
// requests.
//
// Read that against the per-request contract documented on pace below. A lookup
// that finds nothing costs ONE request; a lookup that succeeds costs three
// (search, next, browse). So 10s buys roughly 360 misses or 120 hits per hour,
// which clears a library backfill no faster than a person browsing.
//
// #858 owns exposing this on the config surface; until then it is what a
// constructing caller should pass to WithMinInterval.
const DefaultMinInterval = 10 * time.Second

// ctxSleep waits d or until ctx is done, reporting whether the full duration
// elapsed. Mirrors internal/petitlyrics.ctxSleep; injectable so pacing tests
// need no wall-clock time.
func ctxSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// WithMinInterval sets the minimum duration between outbound requests and
// returns the receiver for chaining.
//
// A zero or negative value disables pacing (the default), which is what
// one-shot CLI fetches and tests want. Any POSITIVE value is clamped up to
// MinAllowedInterval.
//
// The clamp bounds requests this client ISSUES. It does not bound redirect
// hops: pacing runs once per http.Client.Do, and the transport follows any 3xx
// inside that call. checkRedirect confines those to the same host and net/http
// caps them at 10, so the exposure is bounded, but the guarantee is "paced
// calls", not "paced HTTP round-trips".
//
// THE WRITE IS GUARDED by the same mutex pace() reads minInterval under,
// following internal/musixmatch (#494) rather than internal/petitlyrics. The
// petitlyrics form -- an unguarded write plus a "not goroutine-safe" comment --
// is the shape #494 already fixed once, and a doc comment does not enforce:
// this Client is shared across the worker's goroutines, and #858 will wire a
// config-driven setter into exactly that world. A ported copy of musixmatch's
// own #494 regression test produced a genuine DATA RACE against the unguarded
// form.
//
// Race-free is not the same as a supported runtime knob. A concurrent caller
// may observe either the old or the new interval for any given pace() call,
// with no ordering guarantee beyond "no data race". Prefer setting it once
// before sharing the client.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	if d > 0 && d < MinAllowedInterval {
		slog.Warn("innertube: configured cooldown is below the allowed floor; clamping",
			"configured", d, "floor", MinAllowedInterval)
		d = MinAllowedInterval
	}
	c.mu.Lock()
	c.minInterval = d
	c.mu.Unlock()
	return c
}

// MinInterval returns the configured minimum request interval. Zero means
// pacing is disabled. Guarded by the same mutex WithMinInterval writes under
// (#494).
func (c *Client) MinInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.minInterval
}

// pace enforces the minimum interval between outbound requests. The wait is
// ctx-cancellable.
//
// TWO PACERS ARE BORROWED FROM, AND THE SPLIT IS DELIBERATE. The LOCKING
// follows internal/musixmatch (#494): both accessors and this read take the
// mutex. The SLOT ALGORITHM is internal/petitlyrics's re-check loop, NOT
// musixmatch's reservation. Naming only the first would mislead a reader into
// expecting reservation semantics, so both are stated.
//
// The difference is observable. musixmatch reserves the slot under the lock and
// each caller sleeps its own distinct wait; this loop has every waiter compute
// the SAME wait, sleep it, wake together, and re-check -- a thundering herd by
// construction (measured: 8 concurrent callers produce 28 sleep wakeups where
// reservation produces 7). Throughput is unaffected and no caller starved in
// repeated runs, but wake order is unfair and nondeterministic.
//
// The re-check form is kept because it claims the slot ONLY on the success
// branch, so there is no reservation to roll back when a caller is canceled
// mid-wait. musixmatch's form needs a best-effort release for exactly that case.
// At this package's intended concurrency -- a worker pool, single digits -- the
// herd costs a few extra wakeups and buys a simpler cancellation path.
//
// PACING IS PER OUTBOUND REQUEST, NOT PER LOOKUP, and that is a decision rather
// than a detail. The interval exists to bound the rate at which canticle draws
// on someone else's gateway, and a gateway counts REQUESTS -- it has no notion
// of our lookups. This provider spends up to three requests on one successful
// lookup (search, next, browse), so pacing per lookup would satisfy the
// configured interval on paper while firing three back-to-back requests: a 3x
// burst, the precise shape the floor exists to prevent.
//
// The wait therefore lives in postJSON, the single point all three calls funnel
// through, so no call site can opt out of it. The resulting cost is asymmetric
// and honest: a lookup that misses pays one interval, and one that hits pays
// three, because it genuinely consumes three times as much of the shared
// resource.
//
// The loop re-checks rather than sleeping once and proceeding: several
// goroutines can be waiting on the same client, and the one that wakes first
// claims the slot by writing lastRequest under the lock. A waiter that wakes
// after it must wait again rather than issue a request the winner just paid
// for.
func (c *Client) pace(ctx context.Context) error {
	for {
		c.mu.Lock()
		// READ UNDER THE LOCK (#494). Reading minInterval before taking it --
		// as an early `if c.minInterval <= 0 { return nil }` above the loop did
		// -- is a bare data race against WithMinInterval's write.
		minInterval := c.minInterval
		if minInterval <= 0 {
			c.mu.Unlock()
			return nil
		}
		now := c.now()
		wait := minInterval - now.Sub(c.lastRequest)
		if wait <= 0 {
			c.lastRequest = now
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()

		slog.Debug("innertube pacer: waiting before next request", "wait", wait)
		if !c.sleep(ctx, wait) {
			return fmt.Errorf("innertube: pace: %w", ctx.Err())
		}
	}
}
