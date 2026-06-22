package musixmatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

// newPacerTestClient is a thin helper that wires up a Client with injected
// now/sleep and the existing roundTripFunc transport.
func newPacerTestClient(minInterval time.Duration, nowFn func() time.Time, sleepFn func(context.Context, time.Duration) bool) *Client {
	body := minimalSyncedResponse()
	c := newTestClient(200, body)
	c.WithMinInterval(minInterval)
	c.now = nowFn
	c.sleep = sleepFn
	return c
}

// minimalSyncedResponse returns the JSON body for a track with synced lyrics.
func minimalSyncedResponse() string {
	return `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "t",
									"artist_name": "a",
									"has_subtitles": 1,
									"has_lyrics": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{
										"subtitle": {
											"subtitle_body": "[{\"text\":\"x\",\"time\":{\"total\":1.0,\"minutes\":0,\"seconds\":1,\"hundredths\":0}}]"
										}
									}
								]
							}
						}
					}
				}
			}
		}
	}`
}

func TestPacerNoopWhenMinIntervalZero(t *testing.T) {
	sleepCalled := false
	c := newPacerTestClient(0, time.Now, func(ctx context.Context, d time.Duration) bool {
		sleepCalled = true
		return true
	})

	track := models.Track{TrackName: "t", ArtistName: "a"}
	ctx := context.Background()

	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if sleepCalled {
		t.Fatal("sleep was called; pacer should be a no-op when minInterval is 0")
	}
}

func TestPacerEnforcesMinInterval(t *testing.T) {
	minInterval := 10 * time.Second
	// Fake clock starts at base. The sleep function advances it by the
	// requested duration so the re-check loop sees wait <= 0 after one sleep
	// and terminates without spinning.
	base := time.Unix(1000, 0)
	var mu sync.Mutex
	fakeNow := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}

	var gotSleep time.Duration
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		gotSleep = d
		mu.Lock()
		fakeNow = fakeNow.Add(d)
		mu.Unlock()
		return true
	}

	c := newPacerTestClient(minInterval, nowFn, sleepFn)
	track := models.Track{TrackName: "t", ArtistName: "a"}
	ctx := context.Background()

	// First call: no prior lastRequest (zero time), so elapsed is huge and
	// wait <= 0 -- no sleep. Sets lastRequest = fakeNow (base).
	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if gotSleep != 0 {
		t.Fatalf("first call triggered sleep %v; want none", gotSleep)
	}

	// Second call at the same simulated time (elapsed=0 < minInterval=10s).
	// sleep advances the clock by minInterval so the re-check exits cleanly.
	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if gotSleep != minInterval {
		t.Fatalf("second call sleep = %v; want %v", gotSleep, minInterval)
	}
}

func TestPacerNoSleepWhenAlreadyElapsed(t *testing.T) {
	minInterval := 10 * time.Second
	base := time.Unix(1000, 0)
	// Fake clock: starts at base (so first FindLyrics sets lastRequest=base),
	// then advances to base+minInterval for the second FindLyrics so elapsed
	// >= minInterval and no sleep is needed. A mutex guards fakeNow because
	// pace() reads it under c.mu (different lock).
	var mu sync.Mutex
	fakeNow := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}

	sleepCalled := false
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		sleepCalled = true
		return true
	}

	c := newPacerTestClient(minInterval, nowFn, sleepFn)
	track := models.Track{TrackName: "t", ArtistName: "a"}
	ctx := context.Background()

	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Advance fake clock by minInterval before second call so elapsed >= minInterval.
	mu.Lock()
	fakeNow = base.Add(minInterval)
	mu.Unlock()

	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if sleepCalled {
		t.Fatal("sleep was called; elapsed >= minInterval, so no sleep expected")
	}
}

func TestPacerCtxCancelDuringSleep(t *testing.T) {
	minInterval := 10 * time.Second
	base := time.Unix(1000, 0)
	nowFn := func() time.Time { return base }

	ctx, cancel := context.WithCancel(context.Background())
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		cancel() // simulate context canceled during wait
		return false
	}

	c := newPacerTestClient(minInterval, nowFn, sleepFn)
	track := models.Track{TrackName: "t", ArtistName: "a"}

	// Prime lastRequest so second call triggers the wait.
	if _, err := c.FindLyrics(context.Background(), track); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := c.FindLyrics(ctx, track)
	if err == nil {
		t.Fatal("expected error on ctx cancel; got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
}

func TestPacerGoroutineSafe(t *testing.T) {
	// Multiple goroutines call FindLyrics concurrently; the pacer must not race.
	// Use a tiny non-zero interval so the 10 goroutines actually exercise the
	// locked paced path (minInterval=0 skips the mutex entirely). The fake
	// sleep advances a shared clock by the requested duration and returns true
	// immediately so no real time elapses and the re-check loop always terminates.
	var mu sync.Mutex
	fakeNow := time.Unix(2000, 0)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		fakeNow = fakeNow.Add(d)
		mu.Unlock()
		return true
	}
	c := newPacerTestClient(time.Nanosecond, nowFn, sleepFn)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
		}()
	}
	wg.Wait()
}

func TestPacerConcurrentSlotAllocation(t *testing.T) {
	// Two callers race pace() against a FROZEN clock (now never advances). The
	// pacer reserves each caller's slot under the lock by advancing lastRequest,
	// so the two callers claim sequential slots: the first waits 0, the second
	// waits exactly one interval. Total sleep is ~1x interval, NOT 2x (the
	// convoy bug, where both would read the same lastRequest and each sleep a
	// full interval). adaptiveLevel is 0 at the start, so the effective interval
	// equals minInterval (multiplier 1).
	//
	// Note: this test would HANG under the pre-fix loop-and-recheck pace(): with
	// a frozen clock the waiting caller would recompute the same wait forever.
	// Reserving the slot under the lock is what makes it terminate.
	minInterval := 10 * time.Second
	base := time.Unix(5000, 0)
	nowFn := func() time.Time { return base } // frozen

	var mu sync.Mutex
	var totalSleep time.Duration
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		totalSleep += d
		mu.Unlock()
		return true
	}

	c := newPacerTestClient(minInterval, nowFn, sleepFn)
	if got := readAdaptiveLevel(c); got != 0 {
		t.Fatalf("adaptiveLevel = %d at test start; want 0 (multiplier 1)", got)
	}
	track := models.Track{TrackName: "t", ArtistName: "a"}
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.FindLyrics(ctx, track); err != nil {
				t.Errorf("FindLyrics: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	got := totalSleep
	mu.Unlock()
	if got != minInterval {
		t.Fatalf("total sleep across 2 concurrent callers = %v; want %v (1x interval, not 2x convoy)", got, minInterval)
	}
}

func TestWithMinIntervalReturnsClient(t *testing.T) {
	c := NewClient("token")
	got := c.WithMinInterval(5 * time.Second)
	if got != c {
		t.Fatal("WithMinInterval did not return the receiver")
	}
}

func TestMinIntervalAccessor(t *testing.T) {
	c := NewClient("token")
	if c.MinInterval() != 0 {
		t.Fatalf("default MinInterval = %v; want 0", c.MinInterval())
	}
	c.WithMinInterval(7 * time.Second)
	if c.MinInterval() != 7*time.Second {
		t.Fatalf("MinInterval = %v; want 7s", c.MinInterval())
	}
}

func TestCtxSleepCompletesNormally(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	got := ctxSleep(ctx, 10*time.Millisecond)
	if !got {
		t.Fatal("ctxSleep returned false; want true (no cancel)")
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("sleep was too short: %v", elapsed)
	}
}

func TestCtxSleepReturnsFalseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	got := ctxSleep(ctx, time.Hour)
	if got {
		t.Fatal("ctxSleep returned true on already-canceled ctx; want false")
	}
}

// readAdaptiveLevel returns the client's adaptive level under the mutex.
func readAdaptiveLevel(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.adaptiveLevel
}

// readConsecutiveSuccesses returns the success counter under the mutex.
func readConsecutiveSuccesses(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveSuccesses
}

func TestOnThrottleRatchetsToCap(t *testing.T) {
	c := NewClient("token")
	if got := readAdaptiveLevel(c); got != 0 {
		t.Fatalf("initial level = %d; want 0", got)
	}
	want := []int{1, 2, 3, 3, 3} // 0->1->2->3, then stays capped at adaptiveMaxLevel
	for i, w := range want {
		c.OnThrottle()
		if got := readAdaptiveLevel(c); got != w {
			t.Fatalf("after OnThrottle call %d: level = %d; want %d", i+1, got, w)
		}
	}
	if adaptiveMaxLevel != 3 {
		t.Fatalf("adaptiveMaxLevel = %d; test assumes 3", adaptiveMaxLevel)
	}
}

func TestOnThrottleIndependentOfTripCount(t *testing.T) {
	// The pacer takes no trip-count parameter: each OnThrottle increments by
	// exactly one regardless of any external counter. Verify monotonic +1 steps.
	c := NewClient("token")
	c.OnThrottle()
	c.OnThrottle()
	if got := readAdaptiveLevel(c); got != 2 {
		t.Fatalf("level after two throttles = %d; want 2 (one step each)", got)
	}
}

func TestOnSuccessStepsDownAfterThreshold(t *testing.T) {
	c := NewClient("token")
	// Ratchet up to level 2.
	c.OnThrottle()
	c.OnThrottle()
	if got := readAdaptiveLevel(c); got != 2 {
		t.Fatalf("level = %d; want 2", got)
	}
	// Below threshold: no step-down yet.
	for i := 0; i < adaptiveSuccessThreshold-1; i++ {
		c.OnSuccess()
		if got := readAdaptiveLevel(c); got != 2 {
			t.Fatalf("after %d successes: level = %d; want 2 (below threshold)", i+1, got)
		}
	}
	// The threshold-th success steps the level down by one and resets the counter.
	c.OnSuccess()
	if got := readAdaptiveLevel(c); got != 1 {
		t.Fatalf("after threshold successes: level = %d; want 1", got)
	}
	if got := readConsecutiveSuccesses(c); got != 0 {
		t.Fatalf("success counter = %d after step-down; want 0", got)
	}
}

func TestOnSuccessFlooredAtZero(t *testing.T) {
	c := NewClient("token")
	// No throttles: level is 0. A full streak must not drive it negative.
	for i := 0; i < adaptiveSuccessThreshold; i++ {
		c.OnSuccess()
	}
	if got := readAdaptiveLevel(c); got != 0 {
		t.Fatalf("level = %d after streak at floor; want 0", got)
	}
}

func TestOnThrottleResetsSuccessCounter(t *testing.T) {
	c := NewClient("token")
	c.OnThrottle() // level 1
	// Accumulate a partial streak below the threshold.
	c.OnSuccess()
	c.OnSuccess()
	if got := readConsecutiveSuccesses(c); got != 2 {
		t.Fatalf("success counter = %d; want 2", got)
	}
	// A throttle mid-streak resets the success counter.
	c.OnThrottle()
	if got := readConsecutiveSuccesses(c); got != 0 {
		t.Fatalf("success counter = %d after throttle; want 0", got)
	}
	if got := readAdaptiveLevel(c); got != 2 {
		t.Fatalf("level = %d after second throttle; want 2", got)
	}
}

func TestPaceUsesAdaptiveInterval(t *testing.T) {
	minInterval := 10 * time.Second
	base := time.Unix(1000, 0)
	var mu sync.Mutex
	fakeNow := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}
	var gotSleep time.Duration
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		gotSleep = d
		mu.Lock()
		fakeNow = fakeNow.Add(d)
		mu.Unlock()
		return true
	}

	c := newPacerTestClient(minInterval, nowFn, sleepFn)
	track := models.Track{TrackName: "t", ArtistName: "a"}
	ctx := context.Background()

	// Ratchet to level 2 => effective interval = minInterval * (1 << 2) = 40s.
	c.OnThrottle()
	c.OnThrottle()

	// First call sets lastRequest (huge elapsed, no sleep).
	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if gotSleep != 0 {
		t.Fatalf("first call slept %v; want 0", gotSleep)
	}
	// Second call at the same simulated instant must wait the derived interval.
	if _, err := c.FindLyrics(ctx, track); err != nil {
		t.Fatalf("second call: %v", err)
	}
	wantWait := minInterval * time.Duration(1<<2)
	if gotSleep != wantWait {
		t.Fatalf("adaptive sleep = %v; want %v (minInterval * 1<<level)", gotSleep, wantWait)
	}
}

func TestAdaptiveLevelPersistsAcrossRecovery(t *testing.T) {
	// Simulate a circuit recovery cycle: throttle, then some (sub-threshold)
	// successes as the breaker recovers, then another throttle. The level must
	// NOT have reset to base on recovery -- it persists and ratchets further.
	c := NewClient("token")
	c.OnThrottle() // level 1
	c.OnThrottle() // level 2
	// Breaker recovers; a few successes arrive but not enough to step down.
	for i := 0; i < adaptiveSuccessThreshold-1; i++ {
		c.OnSuccess()
	}
	if got := readAdaptiveLevel(c); got != 2 {
		t.Fatalf("level dropped during recovery = %d; want 2 (must persist)", got)
	}
	// IP throttles again: level keeps ratcheting from where it was, not from 0.
	c.OnThrottle()
	if got := readAdaptiveLevel(c); got != 3 {
		t.Fatalf("level after re-throttle = %d; want 3 (ratchets from persisted 2)", got)
	}
}

func TestAdaptiveStateConcurrencySafe(t *testing.T) {
	// Hammer OnThrottle/OnSuccess/pace concurrently; the mutex must serialize all
	// adaptive-state access. Run under -race to catch data races.
	base := time.Unix(3000, 0)
	var mu sync.Mutex
	fakeNow := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}
	sleepFn := func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		fakeNow = fakeNow.Add(d)
		mu.Unlock()
		return true
	}
	c := newPacerTestClient(time.Nanosecond, nowFn, sleepFn)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.OnThrottle()
				c.OnSuccess()
				_ = c.pace(context.Background())
			}
		}()
	}
	wg.Wait()

	if got := readAdaptiveLevel(c); got < 0 || got > adaptiveMaxLevel {
		t.Fatalf("level = %d out of bounds [0,%d]", got, adaptiveMaxLevel)
	}
}
