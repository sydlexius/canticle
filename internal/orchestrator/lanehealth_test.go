package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
)

func TestLaneHealthWalksBreakerLifecycleInLaneOrder(t *testing.T) {
	pa := &stubProvider{name: "alpha", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	pb := &stubProvider{name: "beta"}
	la, cba := newTestLane(pa)
	lb, _ := newTestLane(pb)
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cba.SetClock(func() time.Time { return now })
	o, err := New(ModeOrdered, la, lb)
	if err != nil {
		t.Fatal(err)
	}

	h := o.LaneHealth()
	if len(h) != 2 || h[0].Provider != "alpha" || h[1].Provider != "beta" {
		t.Fatalf("order = %+v; want alpha, beta", h)
	}
	if h[0].State != LaneStateClosed || h[0].EverSucceeded || h[0].Trips != 0 {
		t.Fatalf("fresh alpha = %+v", h[0])
	}

	res := cba.Trip()
	h = o.LaneHealth()
	if h[0].State != LaneStateOpen || !h[0].OpenUntil.Equal(res.OpenUntil) || h[0].Trips != 1 {
		t.Fatalf("tripped alpha = %+v; want open until %v, 1 trip", h[0], res.OpenUntil)
	}
	if h[1].State != LaneStateClosed {
		t.Fatalf("beta = %+v; a sibling lane must not follow alpha's breaker", h[1])
	}

	now = res.OpenUntil
	if h = o.LaneHealth(); h[0].State != LaneStateHalfOpen || !h[0].OpenUntil.IsZero() {
		t.Fatalf("elapsed alpha = %+v; want half-open", h[0])
	}

	// A real dispatch probes and closes the breaker.
	if _, err := la.FindLyrics(context.Background(), models.Track{}, ""); err != nil {
		t.Fatal(err)
	}
	h = o.LaneHealth()
	if h[0].State != LaneStateClosed || h[0].Trips != 0 || !h[0].EverSucceeded {
		t.Fatalf("recovered alpha = %+v; want closed, 0 trips, ever succeeded", h[0])
	}

	// EverSucceeded must survive a later trip: it is not tied to the trip count.
	cba.Trip()
	h = o.LaneHealth()
	if h[0].State != LaneStateOpen || h[0].Trips != 1 || !h[0].EverSucceeded {
		t.Fatalf("re-tripped alpha = %+v; want open, 1 trip, ever succeeded", h[0])
	}
}

func TestLaneHealthIsReadOnly(t *testing.T) {
	l, cb := newTestLane(&stubProvider{name: "alpha", err: errors.New("x")})
	now := time.Now()
	cb.SetClock(func() time.Time { return now })
	o, err := New(ModeOrdered, l)
	if err != nil {
		t.Fatal(err)
	}
	res := cb.Trip()
	now = res.OpenUntil.Add(time.Second)
	for i := 0; i < 3; i++ {
		if got := o.LaneHealth()[0].State; got != LaneStateHalfOpen {
			t.Fatalf("scrape %d state = %q; want half-open", i, got)
		}
	}
	if cb.OpenUntil().IsZero() {
		t.Fatal("LaneHealth consumed the open window")
	}
}

func TestLaneHealthConcurrentWithBreaker(t *testing.T) {
	l, cb := newTestLane(&stubProvider{name: "alpha"})
	o, err := New(ModeOrdered, l)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cb.Trip()
			cb.RecordSuccess()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s := o.LaneHealth()[0]
			if s.State == LaneStateOpen && s.OpenUntil.IsZero() {
				t.Error("open snapshot without OpenUntil")
				return
			}
		}
	}()
	wg.Wait()
}

func TestLaneStateNameUnknownIsClosed(t *testing.T) {
	if got := laneStateName(circuit.BreakerState(99)); got != LaneStateClosed {
		t.Fatalf("got %q", got)
	}
}

func TestLaneHealthReportsLocalLane(t *testing.T) {
	lp, _ := newTestLane(&stubProvider{name: "alpha"})
	ll := newLocalTestLane(models.Song{})
	o, err := New(ModeOrdered, lp, ll)
	if err != nil {
		t.Fatal(err)
	}
	h := o.LaneHealth()
	if h[0].Local || !h[1].Local || h[1].Provider != "detector" {
		t.Fatalf("health = %+v; want provider lane non-local, detector lane local", h)
	}
}
