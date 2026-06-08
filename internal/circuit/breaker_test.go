package circuit

import (
	"sync"
	"testing"
	"time"
)

const (
	testBase = 60 * time.Second
	testCap  = 30 * time.Minute
)

// newTestBreaker builds a breaker with a frozen clock for deterministic tests.
func newTestBreaker() (*Breaker, time.Time) {
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	b := New(testBase, testCap)
	b.SetClock(func() time.Time { return fixed })
	return b, fixed
}

func TestNewStartsClosed(t *testing.T) {
	b, _ := newTestBreaker()
	if got := b.Allow(); got != StateClosed {
		t.Fatalf("Allow() = %v; want StateClosed on a fresh breaker", got)
	}
	if !b.OpenUntil().IsZero() {
		t.Fatalf("OpenUntil() = %v; want zero on a fresh breaker", b.OpenUntil())
	}
	if b.EverSucceeded() {
		t.Fatal("EverSucceeded() = true on a fresh breaker; want false")
	}
}

func TestTripOpensAndAllowReportsOpen(t *testing.T) {
	b, fixed := newTestBreaker()
	res := b.Trip()
	if res.Trips != 1 {
		t.Fatalf("Trip().Trips = %d; want 1", res.Trips)
	}
	if res.Window != testBase {
		t.Fatalf("Trip().Window = %v; want %v (trip 1 uses the geometric base)", res.Window, testBase)
	}
	if !res.OpenUntil.Equal(fixed.Add(testBase)) {
		t.Fatalf("Trip().OpenUntil = %v; want %v", res.OpenUntil, fixed.Add(testBase))
	}
	// now == fixed, which is before openUntil, so the circuit is open.
	if got := b.Allow(); got != StateOpen {
		t.Fatalf("Allow() = %v; want StateOpen while the window is in the future", got)
	}
	if !b.OpenUntil().Equal(fixed.Add(testBase)) {
		t.Fatalf("OpenUntil() = %v; want %v", b.OpenUntil(), fixed.Add(testBase))
	}
}

func TestTripRampIncrementsAndCaps(t *testing.T) {
	b, fixed := newTestBreaker()
	// trip 6 reaches the cap: 60 -> 120 -> 240 -> 480 -> 960 -> 1800 (capped).
	wantDeltas := []time.Duration{
		60 * time.Second, 120 * time.Second, 240 * time.Second,
		480 * time.Second, 960 * time.Second, 30 * time.Minute,
	}
	for i, want := range wantDeltas {
		res := b.Trip()
		if res.Trips != i+1 {
			t.Fatalf("trip %d: Trips = %d; want %d", i+1, res.Trips, i+1)
		}
		if res.Window != want {
			t.Fatalf("trip %d: Window = %v; want %v", i+1, res.Window, want)
		}
		if got := b.OpenUntil().Sub(fixed); got != want {
			t.Fatalf("trip %d: OpenUntil delta = %v; want %v", i+1, got, want)
		}
	}
	// The cap holds on further trips.
	res := b.Trip()
	if res.Window != testCap {
		t.Fatalf("trip 7: Window = %v; want cap %v", res.Window, testCap)
	}
}

func TestHalfOpenTransitionWhenWindowElapses(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	now := fixed
	b := New(testBase, testCap)
	b.SetClock(func() time.Time { return now })

	b.Trip() // opens until fixed+60s
	if got := b.Allow(); got != StateOpen {
		t.Fatalf("Allow() = %v; want StateOpen before the window elapses", got)
	}
	// Advance past the window.
	now = fixed.Add(2 * time.Minute)
	if got := b.Allow(); got != StateHalfOpen {
		t.Fatalf("Allow() = %v; want StateHalfOpen after the window elapses", got)
	}
	// The transition cleared openUntil as a side effect.
	if !b.OpenUntil().IsZero() {
		t.Fatalf("OpenUntil() = %v; want zero after entering half-open", b.OpenUntil())
	}
	// Subsequent Allow stays half-open (probing), not closed, until a success.
	if got := b.Allow(); got != StateHalfOpen {
		t.Fatalf("Allow() = %v; want StateHalfOpen to persist until a success closes it", got)
	}
}

func TestRecordSuccessClosesAndReportsTransition(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	now := fixed
	b := New(testBase, testCap)
	b.SetClock(func() time.Time { return now })

	b.Trip()
	now = fixed.Add(2 * time.Minute)
	if got := b.Allow(); got != StateHalfOpen {
		t.Fatalf("Allow() = %v; want StateHalfOpen", got)
	}
	if transitioned := b.RecordSuccess(); !transitioned {
		t.Fatal("RecordSuccess() = false; want true (probing -> closed transition)")
	}
	if !b.EverSucceeded() {
		t.Fatal("EverSucceeded() = false; want true after a genuine success")
	}
	if got := b.Allow(); got != StateClosed {
		t.Fatalf("Allow() = %v; want StateClosed after recovery", got)
	}
	// A success while already closed does not re-report a transition.
	if transitioned := b.RecordSuccess(); transitioned {
		t.Fatal("RecordSuccess() = true while already closed; want false")
	}
}

func TestRecordSuccessResetsRamp(t *testing.T) {
	b, fixed := newTestBreaker()
	b.Trip()
	b.Trip()
	b.Trip()
	b.RecordSuccess()
	// The next trip restarts at the base, proving the ramp reset.
	res := b.Trip()
	if res.Trips != 1 {
		t.Fatalf("Trips = %d; want 1 after a success reset", res.Trips)
	}
	if res.Window != testBase {
		t.Fatalf("Window = %v; want base %v after a success reset", res.Window, testBase)
	}
	if got := b.OpenUntil().Sub(fixed); got != testBase {
		t.Fatalf("OpenUntil delta = %v; want %v", got, testBase)
	}
}

func TestRecordBenignMissResetsRampWithoutMarkingSuccess(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	now := fixed
	b := New(testBase, testCap)
	b.SetClock(func() time.Time { return now })

	b.Trip()
	b.Trip()
	b.Trip() // trip 3 opens for 240s
	now = fixed.Add(5 * time.Minute)
	if got := b.Allow(); got != StateHalfOpen {
		t.Fatalf("Allow() = %v; want StateHalfOpen", got)
	}
	if transitioned := b.RecordBenignMiss(); !transitioned {
		t.Fatal("RecordBenignMiss() = false; want true (probing -> closed transition)")
	}
	if b.EverSucceeded() {
		t.Fatal("EverSucceeded() = true after a benign miss; a clean miss is not a provider match")
	}
	if got := b.Allow(); got != StateClosed {
		t.Fatalf("Allow() = %v; want StateClosed after the benign miss closed it", got)
	}
	// Ramp reset: next trip restarts at the base.
	res := b.Trip()
	if res.Trips != 1 || res.Window != testBase {
		t.Fatalf("after benign miss: Trips=%d Window=%v; want 1, %v", res.Trips, res.Window, testBase)
	}
}

func TestRecordBenignMissNoTransitionWhenClosed(t *testing.T) {
	b, _ := newTestBreaker()
	if transitioned := b.RecordBenignMiss(); transitioned {
		t.Fatal("RecordBenignMiss() = true while closed; want false")
	}
	if b.EverSucceeded() {
		t.Fatal("EverSucceeded() = true; a benign miss must never set it")
	}
}

func TestTripRenewalHoldsFullCapAndDoesNotAdvanceRamp(t *testing.T) {
	b, fixed := newTestBreaker()
	// Two throttle trips establish a ramp position.
	b.Trip()
	b.Trip()
	if b.Trips() != 2 {
		t.Fatalf("Trips() = %d; want 2 after two throttle trips", b.Trips())
	}
	res := b.TripRenewal()
	if res.Window != testCap {
		t.Fatalf("TripRenewal().Window = %v; want the full cap %v", res.Window, testCap)
	}
	if !res.OpenUntil.Equal(fixed.Add(testCap)) {
		t.Fatalf("TripRenewal().OpenUntil = %v; want %v", res.OpenUntil, fixed.Add(testCap))
	}
	if b.Trips() != 2 {
		t.Fatalf("Trips() = %d; want 2 (renewal must not advance the throttle ramp)", b.Trips())
	}
	// A subsequent throttle resumes from the preserved ramp position (trip 3).
	next := b.Trip()
	if next.Trips != 3 {
		t.Fatalf("post-renewal Trip().Trips = %d; want 3 (ramp position preserved)", next.Trips)
	}
}

func TestSetBackoffOverridesAndIgnoresNonPositive(t *testing.T) {
	b, fixed := newTestBreaker()
	b.SetBackoff(0, 0) // ignored
	b.SetBackoff(10*time.Second, time.Hour)
	res := b.Trip()
	if res.Window != 10*time.Second {
		t.Fatalf("Trip().Window = %v; want overridden base 10s", res.Window)
	}
	if !res.OpenUntil.Equal(fixed.Add(10 * time.Second)) {
		t.Fatalf("OpenUntil = %v; want %v", res.OpenUntil, fixed.Add(10*time.Second))
	}
}

func TestSetClockNilIsIgnored(t *testing.T) {
	b, fixed := newTestBreaker()
	b.SetClock(nil) // must not clear the injected clock
	res := b.Trip()
	if !res.OpenUntil.Equal(fixed.Add(testBase)) {
		t.Fatalf("OpenUntil = %v; want clock unchanged at %v", res.OpenUntil, fixed.Add(testBase))
	}
}

// TestConcurrentAccessIsRaceFree drives the breaker from many goroutines so
// the race detector can prove every method takes the mutex.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	b := New(testBase, testCap)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Allow()
				b.Trip()
				b.TripRenewal()
				b.Trips()
				b.OpenUntil()
				b.RecordSuccess()
				b.RecordBenignMiss()
				b.EverSucceeded()
			}
		}()
	}
	wg.Wait()
}
