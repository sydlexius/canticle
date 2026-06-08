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
