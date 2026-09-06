package innertube

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The pacer is driven through Search here rather than through a synthetic
// caller. Search is one of the three real calls that must be paced, and it
// reaches postJSON by exactly the path production takes, so these tests
// exercise the wiring as well as the arithmetic.

// fakeClock is a manually advanced clock plus a sleep that records what it was
// asked to wait for and advances the clock by that much. No wall-clock time
// passes in any pacing test.
type fakeClock struct {
	now    time.Time
	waits  []time.Duration
	cancel func()
}

func (f *fakeClock) Now() time.Time { return f.now }

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) bool {
	f.waits = append(f.waits, d)
	f.now = f.now.Add(d)
	if f.cancel != nil {
		f.cancel()
		return false
	}
	return true
}

// withFakeClock installs a fake clock on c and returns it. The base time is a
// fixed instant well past the zero Time, so the FIRST request is not paced -- a
// zero lastRequest is an infinitely old one, which is the intended "no wait on
// the first call" behavior rather than a coincidence of the fixture.
func withFakeClock(c *Client) *fakeClock {
	fc := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.now = fc.Now
	c.sleep = fc.Sleep
	return fc
}

// --- the per-request contract ---

// TestPace_AppliesPerRequestNotPerCaller is the assertion behind the pacing
// DECISION documented on pace. Three outbound requests must wait twice: the
// first finds no prior request to pace against, and each subsequent one waits a
// full interval.
//
// A pacer that ran once per CALLER instead would record zero waits here while
// firing the same three requests back to back -- the 3x burst the interval
// exists to prevent -- so this test is what distinguishes the two designs.
func TestPace_AppliesPerRequestNotPerCaller(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)
	c.WithMinInterval(30 * time.Second)

	for i := range 3 {
		if _, err := c.Search(context.Background(), "Artist", "Title"); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}

	if got, want := len(fc.waits), 2; got != want {
		t.Fatalf("pacer waited %d times %v, want %d (three requests, the first unpaced)",
			got, fc.waits, want)
	}
	for i, w := range fc.waits {
		if w != 30*time.Second {
			t.Errorf("wait %d = %v, want the full 30s interval", i, w)
		}
	}
	if n := len(srv.snapshot()); n != 3 {
		t.Errorf("requests issued = %d, want 3", n)
	}
}

// TestPace_FirstRequestIsNotDelayed pins that a freshly built client does not
// pay an interval before its very first request: lastRequest's zero value is an
// infinitely old request, not a recent one.
func TestPace_FirstRequestIsNotDelayed(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)
	c.WithMinInterval(time.Hour)

	if _, err := c.Search(context.Background(), "Artist", "Title"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(fc.waits) != 0 {
		t.Errorf("pacer waited %v before the first request", fc.waits)
	}
}

func TestPace_DisabledByDefault(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)

	if c.MinInterval() != 0 {
		t.Fatalf("MinInterval = %v, want 0 by default", c.MinInterval())
	}
	for range 3 {
		if _, err := c.Search(context.Background(), "Artist", "Title"); err != nil {
			t.Fatalf("search: %v", err)
		}
	}
	if len(fc.waits) != 0 {
		t.Errorf("pacer waited %v with pacing disabled", fc.waits)
	}
}

// TestPace_ReChecksAfterAShortWait defends the loop's stated design: the waiter
// that wakes first CLAIMS the slot under the lock, and a waiter that wakes early
// must wait AGAIN rather than issue a request the winner already paid for.
//
// The fake sleep advances the clock by LESS than the wait it was asked for -- a
// spurious wake. A correct loop re-computes and waits a second time; a
// single-shot pacer (sleep once, then claim unconditionally) proceeds
// immediately and halves the effective interval under contention.
//
// The concurrency test below cannot see this: it runs at a 1us interval, where
// any sleep satisfies the whole wait and the re-check is trivially true.
func TestPace_ReChecksAfterAShortWait(t *testing.T) {
	c := NewClient()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	var waits []time.Duration

	c.now = func() time.Time { return now }
	c.sleep = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		// The FIRST wake is spurious: advance by only a quarter of the
		// requested wait, so the slot is not yet due and a correct loop must
		// re-check. Every later wake advances fully, so the test terminates.
		//
		// (Advancing by a fraction EVERY time does not terminate: once the
		// remaining wait drops below 4ns, integer division makes d/4 zero, the
		// clock stops moving, and pace spins forever.)
		if len(waits) == 1 {
			now = now.Add(d / 4)
			return true
		}
		now = now.Add(d)
		return true
	}
	c.minInterval = 40 * time.Second
	c.lastRequest = base

	if err := c.pace(context.Background()); err != nil {
		t.Fatalf("pace: %v", err)
	}

	if len(waits) < 2 {
		t.Fatalf("pacer waited %d time(s) %v, want at least 2; a waiter that wakes "+
			"early must re-check and wait again rather than claim the slot", len(waits), waits)
	}
	if elapsed := now.Sub(base); elapsed < 40*time.Second {
		t.Errorf("returned after %v, want >= the full 40s interval", elapsed)
	}
}

// --- the configuration surface ---

func TestWithMinInterval_ClampsPositiveValuesUpToTheFloor(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		// A LITERAL on both sides, not MinAllowedInterval compared to itself:
		// the constant-valued rows below still pass if the floor moves, so
		// without this row the table cannot detect a changed floor at all.
		{"below the floor clamps up to 2s", 1500 * time.Millisecond, 2 * time.Second},
		{"below the floor clamps up", 500 * time.Millisecond, MinAllowedInterval},
		{"at the floor is kept", MinAllowedInterval, MinAllowedInterval},
		{"above the floor is kept", time.Minute, time.Minute},
		{"zero disables pacing", 0, 0},
		{"negative disables pacing", -time.Second, -time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient().WithMinInterval(tc.set)
			if got := c.MinInterval(); got != tc.want {
				t.Errorf("MinInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPacingConstantsArePinned pins both intervals to literals. They are the
// politeness policy toward a third party's infrastructure, and the long
// justification on MinAllowedInterval (2s, versus petitlyrics' 10s, on the
// grounds that this is a Google-scale gateway rather than one vendor's small
// API) hangs entirely off these two values.
//
// Without a literal here the clamp table compares the constant to itself on
// most of its rows and cannot see a change at all.
//
// Changing either number is a policy decision, not a refactor: revisit the
// reasoning in pacer.go's doc comments before editing this test to match.
func TestPacingConstantsArePinned(t *testing.T) {
	if MinAllowedInterval != 2*time.Second {
		t.Errorf("MinAllowedInterval = %v, want 2s", MinAllowedInterval)
	}
	if DefaultMinInterval != 10*time.Second {
		t.Errorf("DefaultMinInterval = %v, want 10s", DefaultMinInterval)
	}
	// The default must clear the floor, or the recommended setting would itself
	// be clamped -- an incoherence no single-constant assertion would catch.
	if DefaultMinInterval < MinAllowedInterval {
		t.Errorf("DefaultMinInterval (%v) is below MinAllowedInterval (%v)",
			DefaultMinInterval, MinAllowedInterval)
	}
}

// --- cancellation ---

// TestPace_CancelDuringTheWait drives cancellation at the point it is easiest to
// get wrong: inside the pacer's wait, not inside an HTTP call.
func TestPace_CancelDuringTheWait(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)
	c.WithMinInterval(30 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.cancel = cancel
	// Prime the pacer so the next request is already due to wait.
	c.lastRequest = fc.now

	_, err := c.Search(ctx, "Artist", "Title")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	// THE ERROR MUST NAME THE PACER, and that assertion is load-bearing rather
	// than cosmetic. TWO layers here honor ctx independently -- this pacer, and
	// NewRequestWithContext inside postJSON -- so a pacer that swallowed
	// cancellation and fell through would STILL surface context.Canceled, from
	// the transport, and still issue no request the server records. Both other
	// checks would pass against a pacer that does not honor ctx at all. Only the
	// layer's own prefix distinguishes which one refused.
	if !strings.Contains(err.Error(), "innertube: pace:") {
		t.Errorf("error = %q, want it to come from the pacer; cancellation during "+
			"the wait must be refused by pace, not left to the transport", err)
	}
	if n := len(srv.snapshot()); n != 0 {
		t.Errorf("requests issued = %d, want 0; cancellation must stop before the wire", n)
	}
}

// --- concurrency ---

// TestPace_ConcurrentCallersShareThePacerSafely backs the claim on the Client's
// pacer fields: one *Client is shared across the worker's goroutines, so
// lastRequest is read-modify-written concurrently and the mutex is required
// rather than defensive.
//
// PACING MUST BE ENABLED FOR THIS TEST TO TEST ANYTHING. pace() returns at its
// first check when minInterval <= 0, so with pacing off every goroutine takes
// the early return and the guarded lines are never reached -- the test would
// pass without executing the code it exists to defend.
//
// minInterval is set DIRECTLY rather than through WithMinInterval because that
// setter clamps any positive value up to MinAllowedInterval (2s), which would
// make 24 serialized requests take about a minute. A sub-millisecond interval
// exercises the same read-modify-write without the wait.
//
// The REAL clock is used deliberately: a fake clock's own unsynchronized fields
// would report a race in the TEST rather than in the code under test.
func TestPace_ConcurrentCallersShareThePacerSafely(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)
	c.minInterval = time.Microsecond

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Search(context.Background(), "Artist", "Title"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Search: %v", err)
	}

	if got := len(srv.snapshot()); got != goroutines {
		t.Errorf("requests = %d, want %d; a pacer that dropped or double-counted a "+
			"slot under contention would not land on this number", got, goroutines)
	}
}

// TestWithMinInterval_IsRaceFreeAgainstAConcurrentPacer is the #494 regression,
// ported from internal/musixmatch. The petitlyrics form this pacer was first
// modeled on writes minInterval unguarded and documents "not goroutine-safe",
// which is the shape musixmatch already fixed under that issue: a doc comment
// does not enforce, and #858 will wire a config-driven setter into exactly the
// world where this Client is shared.
//
// Under -race this fails against an unguarded write.
//
// THE INTERVAL IS ZERO ON PURPOSE, and the choice is what keeps this test
// honest AND fast. The race is on the minInterval FIELD -- WithMinInterval
// writing it while pace reads it -- and the value written is irrelevant to
// whether that is a data race. Zero still performs the write and still makes
// pace perform the read (pace reads the field under the lock BEFORE deciding
// pacing is disabled), so both sides of the race are exercised.
//
// A positive interval would exercise the identical field access and cost 21
// SECONDS of real sleeping: the first goroutine claims the slot and every
// other one then waits a full interval for a slot it does not need, serialized.
// That was measured, not guessed. A test that sleeps for 21s to observe a
// memory access that takes nanoseconds is one people start skipping.
func TestWithMinInterval_IsRaceFreeAgainstAConcurrentPacer(t *testing.T) {
	c := NewClient()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.WithMinInterval(0)
			_ = c.MinInterval()
			_ = c.pace(context.Background())
		}()
	}
	wg.Wait()
}
