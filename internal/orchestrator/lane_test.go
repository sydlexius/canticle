package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/circuit"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
)

type stubProvider struct {
	name  string
	song  models.Song
	err   error
	calls int
}

func (p *stubProvider) Name() string { return p.name }
func (p *stubProvider) FindLyrics(context.Context, models.Track) (models.Song, error) {
	p.calls++
	if p.err != nil {
		return models.Song{}, p.err
	}
	return p.song, nil
}

func newTestLane(p *stubProvider) (*Lane, *circuit.Breaker) {
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)
	return l, cb
}

func TestLaneOpenBreakerSkipsProvider(t *testing.T) {
	p := &stubProvider{name: "musixmatch"}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip() // open the breaker

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("err = %v; want ErrLaneUnavailable", err)
	}
	if p.calls != 0 {
		t.Fatalf("provider calls = %d; want 0 (open breaker must not call provider)", p.calls)
	}
}

func TestLaneSuccessRecordsSuccess(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	l, cb := newTestLane(p)

	song, err := l.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Lyrics.LyricsBody != "ok" {
		t.Fatalf("song body = %q; want ok", song.Lyrics.LyricsBody)
	}
	if !cb.EverSucceeded() {
		t.Fatal("breaker EverSucceeded = false; a genuine fetch must record success")
	}
}

func TestLaneBenignMissRecordsBenignMiss(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	cb.Trip()
	// Advance past the window so the breaker is half-open (not open): a benign
	// miss reaching the provider is what resets the ramp.
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if cb.Trips() != 0 {
		t.Fatalf("trips = %d; want 0 (benign miss resets the ramp)", cb.Trips())
	}
	if cb.EverSucceeded() {
		t.Fatal("benign miss must NOT set EverSucceeded")
	}
}

func TestLaneRateLimitTripsBreaker(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrRateLimited)}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
	if cb.Trips() != 1 {
		t.Fatalf("trips = %d; want 1 (rate limit trips the ramp)", cb.Trips())
	}
	if cb.OpenUntil().Sub(fixed) != 60*time.Second {
		t.Fatalf("window = %v; want 60s", cb.OpenUntil().Sub(fixed))
	}
}

func TestLaneRenewalHoldsFullCapNoRamp(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrTokenRenewalRequired)}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.RecordSuccess()

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrTokenRenewalRequired) {
		t.Fatalf("err = %v; want ErrTokenRenewalRequired", err)
	}
	if cb.OpenUntil().Sub(fixed) != 30*time.Minute {
		t.Fatalf("window = %v; want 30m (renewal holds full cap)", cb.OpenUntil().Sub(fixed))
	}
	if cb.Trips() != 0 {
		t.Fatalf("trips = %d; want 0 (renewal must not advance the ramp)", cb.Trips())
	}
}

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestLaneHonest401NoSuccessYet(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	l, _ := newTestLane(p)
	logs := captureLogs(t, func() {
		_, _ = l.FindLyrics(context.Background(), models.Track{})
	})
	if !strings.Contains(logs, "no successful fetch yet this session") {
		t.Fatalf("logs = %q; want no-success-yet message", logs)
	}
}

func TestLaneHonest401AfterSuccess(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	l, cb := newTestLane(p)
	cb.RecordSuccess()
	logs := captureLogs(t, func() {
		_, _ = l.FindLyrics(context.Background(), models.Track{})
	})
	if !strings.Contains(logs, "token validated earlier this session") {
		t.Fatalf("logs = %q; want throttling-after-success message", logs)
	}
}

func TestLaneHonest401EscalatesAfterThreshold(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.RecordSuccess()
	var logs string
	for i := 0; i < escalationThreshold; i++ {
		// Each trip opens the breaker; advance past the window before the next
		// call so the lane half-opens and actually probes (and trips) again,
		// mirroring how RunOnce reaches the escalation threshold across cycles.
		cb.SetClock(func() time.Time { return fixed.Add(time.Duration(i) * time.Hour) })
		logs = captureLogs(t, func() {
			_, _ = l.FindLyrics(context.Background(), models.Track{})
		})
	}
	if !strings.Contains(logs, "may have expired") {
		t.Fatalf("logs = %q; want escalation message at threshold", logs)
	}
}

func TestLaneHonest401Truncated(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrTruncatedResponse)}
	l, _ := newTestLane(p)
	logs := captureLogs(t, func() {
		_, _ = l.FindLyrics(context.Background(), models.Track{})
	})
	if !strings.Contains(logs, "truncated response") {
		t.Fatalf("logs = %q; want truncated-response message", logs)
	}
}

func TestLaneRecoveryLogsOnHalfOpenSuccess(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	l, cb := newTestLane(p)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })
	cb.Trip()
	// Advance past the window so Allow transitions to half-open.
	cb.SetClock(func() time.Time { return fixed.Add(2 * time.Hour) })
	logs := captureLogs(t, func() {
		_, err := l.FindLyrics(context.Background(), models.Track{})
		if err != nil {
			t.Fatalf("FindLyrics: %v", err)
		}
	})
	if !strings.Contains(logs, "recovered") {
		t.Fatalf("logs = %q; want recovery message after half-open success", logs)
	}
	if cb.Allow() != circuit.StateClosed {
		t.Fatalf("breaker state after recovery = %v; want closed", cb.Allow())
	}
}

// adaptiveStubProvider implements both providers.LyricsProvider and
// providers.AdaptivePacer, recording the adaptive notifications it receives so
// tests can assert the Lane wiring drives it correctly.
type adaptiveStubProvider struct {
	name       string
	song       models.Song
	err        error
	throttles  int
	successes  int
	pacerLevel int // mock ratchet: +1 per throttle (capped at 3), tracks independence
}

func (p *adaptiveStubProvider) Name() string { return p.name }
func (p *adaptiveStubProvider) FindLyrics(context.Context, models.Track) (models.Song, error) {
	if p.err != nil {
		return models.Song{}, p.err
	}
	return p.song, nil
}
func (p *adaptiveStubProvider) OnThrottle() {
	p.throttles++
	if p.pacerLevel < 3 {
		p.pacerLevel++
	}
}
func (p *adaptiveStubProvider) OnSuccess() { p.successes++ }

func TestLaneCallsOnThrottleOnTripAfterSuccess(t *testing.T) {
	// A 401 AFTER the token has succeeded this session is an egress-IP throttle:
	// the pacer must ratchet.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	cb.RecordSuccess()
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized", err)
	}
	if p.throttles != 1 {
		t.Fatalf("OnThrottle calls = %d; want 1 (throttle after a prior success)", p.throttles)
	}
	if p.successes != 0 {
		t.Fatalf("OnSuccess calls = %d; want 0 on a throttle trip", p.successes)
	}
}

func TestLaneCallsOnThrottleOnRateLimitNoPriorSuccess(t *testing.T) {
	// A rate-limit is ALWAYS a throttle signal: the pacer must ratchet even with
	// no successful fetch yet this session.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrRateLimited)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
	if cb.EverSucceeded() {
		t.Fatal("precondition: breaker must report !EverSucceeded for this case")
	}
	if p.throttles != 1 {
		t.Fatalf("OnThrottle calls = %d; want 1 (rate-limit is always a throttle, even with no prior success)", p.throttles)
	}
}

func TestLaneCallsOnThrottleOnRateLimitAfterSuccess(t *testing.T) {
	// A rate-limit AFTER a prior success is likewise a throttle: the pacer must
	// ratchet.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrRateLimited)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	cb.RecordSuccess()
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
	if p.throttles != 1 {
		t.Fatalf("OnThrottle calls = %d; want 1 (rate-limit after a prior success is a throttle)", p.throttles)
	}
}

func TestLaneCallsOnThrottleOnTruncated(t *testing.T) {
	// A truncated response is always a throttle signal, even with no prior
	// success this session: the pacer must ratchet.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrTruncatedResponse)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrTruncatedResponse) {
		t.Fatalf("err = %v; want ErrTruncatedResponse", err)
	}
	if p.throttles != 1 {
		t.Fatalf("OnThrottle calls = %d; want 1 (truncated response is always a throttle)", p.throttles)
	}
}

func TestLaneNoOnThrottleOnBadTokenNeverSucceeded(t *testing.T) {
	// A bare 401 with NO successful fetch yet this session is a bad/expired token,
	// not a throttle: the pacer must NOT ratchet. Slowing requests cannot fix a
	// bad token.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized", err)
	}
	if cb.EverSucceeded() {
		t.Fatal("precondition: breaker must report !EverSucceeded for this case")
	}
	if p.throttles != 0 {
		t.Fatalf("OnThrottle calls = %d; want 0 (bad token, never succeeded, is not a throttle)", p.throttles)
	}
}

func TestLaneNoOnThrottleOnRenewal(t *testing.T) {
	// A token-renewal trip is not an IP throttle: slowing requests cannot fix an
	// expired token, so the pacer must NOT ratchet -- even after a prior success.
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrTokenRenewalRequired)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	cb.RecordSuccess()
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrTokenRenewalRequired) {
		t.Fatalf("err = %v; want ErrTokenRenewalRequired", err)
	}
	if p.throttles != 0 {
		t.Fatalf("OnThrottle calls = %d; want 0 (token renewal is not a throttle)", p.throttles)
	}
}

func TestLaneCallsOnSuccessOnFetch(t *testing.T) {
	p := &adaptiveStubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)

	if _, err := l.FindLyrics(context.Background(), models.Track{}); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if p.successes != 1 {
		t.Fatalf("OnSuccess calls = %d; want 1 on a successful fetch", p.successes)
	}
	if p.throttles != 0 {
		t.Fatalf("OnThrottle calls = %d; want 0 on success", p.throttles)
	}
}

func TestLaneNoOnSuccessOnBenignMiss(t *testing.T) {
	// A benign miss is a successful round-trip but not a catalog hit; the pacer
	// must not be stabilized by it.
	p := &adaptiveStubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)

	_, err := l.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if p.successes != 0 {
		t.Fatalf("OnSuccess calls = %d; want 0 on a benign miss", p.successes)
	}
	if p.throttles != 0 {
		t.Fatalf("OnThrottle calls = %d; want 0 on a benign miss", p.throttles)
	}
}

func TestLaneBackwardCompatNonAdaptiveProvider(t *testing.T) {
	// A provider that implements only LyricsProvider must drive the Lane fine; no
	// adaptive notifications are attempted (the type assertion simply fails).
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	l := NewLane(p, cb)
	if _, err := l.FindLyrics(context.Background(), models.Track{}); !errors.Is(err, musixmatch.ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized", err)
	}
	// Also exercise the success path on a non-adaptive provider.
	ok := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	l2 := NewLane(ok, circuit.New(60*time.Second, 30*time.Minute))
	if _, err := l2.FindLyrics(context.Background(), models.Track{}); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
}

func TestLaneMultipleThrottlesRatchetMock(t *testing.T) {
	p := &adaptiveStubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrUnauthorized)}
	cb := circuit.New(60*time.Second, 30*time.Minute)
	cb.RecordSuccess() // a 401 after a prior success this session is an egress-IP throttle
	l := NewLane(p, cb)
	fixed := time.Now()
	cb.SetClock(func() time.Time { return fixed })

	// Drive several throttle trips; the mock's level ratchets +1 each, capped at 3,
	// independent of the breaker's own trip count.
	for i := 0; i < 5; i++ {
		// Advance past the window each round so the breaker re-trips rather than
		// short-circuiting on an open gate.
		cb.SetClock(func() time.Time { return fixed.Add(time.Duration(i) * time.Hour) })
		_, _ = l.FindLyrics(context.Background(), models.Track{})
	}
	if p.throttles != 5 {
		t.Fatalf("OnThrottle calls = %d; want 5", p.throttles)
	}
	if p.pacerLevel != 3 {
		t.Fatalf("mock pacer level = %d; want 3 (ratchets capped, independent of trip count)", p.pacerLevel)
	}
}
