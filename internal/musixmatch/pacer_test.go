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
