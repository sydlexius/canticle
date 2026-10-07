package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
)

// A Musixmatch HTTP 403 opens the lane and marks it refused, and the first
// answer after the window closes it and clears the mark (#1372).
func TestMusixmatchForbiddenOpensLaneAsRefused(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("edge: %w", musixmatch.ErrForbidden)}
	l, cb := newTestLane(p)
	now := time.Now()
	cb.SetClock(func() time.Time { return now })
	o, _ := New(ModeOrdered, l)

	if _, err := l.FindLyrics(context.Background(), models.Track{}, ""); !errors.Is(err, musixmatch.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden preserved", err)
	}
	if h := o.LaneHealth()[0]; h.State != LaneStateOpen || !h.Refused {
		t.Fatalf("health after a 403 = %+v; want open and Refused", h)
	}

	// The window elapses: the lane is re-probed, and still reads refused until
	// a probe is answered.
	p.err = musixmatch.ErrNotFound
	now = now.Add(2 * time.Minute)
	if h := o.LaneHealth()[0]; h.State != LaneStateHalfOpen || !h.Refused {
		t.Fatalf("health once the window elapsed = %+v; want half-open and still Refused", h)
	}
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if h := o.LaneHealth()[0]; h.State != LaneStateClosed || h.Refused {
		t.Fatalf("health after the provider answered = %+v; want closed and not Refused", h)
	}
}

// A stale innertube client version (HTTP 400, wrapping ErrForbidden) opens the
// lane but is not a provider refusal, so it must not read as one.
func TestInnertubeClientVersionIsNotRefused(t *testing.T) {
	l, _ := newTestLane(&stubProvider{name: "innertube", err: fmt.Errorf("innertube: HTTP 400: %w", innertube.ErrClientVersion)})
	o, _ := New(ModeOrdered, l)
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if h := o.LaneHealth()[0]; h.State != LaneStateOpen || h.Refused {
		t.Fatalf("health after a stale client version = %+v; want open and not Refused", h)
	}
	l, _ = newTestLane(&stubProvider{name: "innertube", err: fmt.Errorf("innertube: HTTP 403: %w", innertube.ErrForbidden)})
	o, _ = New(ModeOrdered, l)
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if h := o.LaneHealth()[0]; !h.Refused {
		t.Fatalf("health after an innertube 403 = %+v; want Refused", h)
	}
}

// A lane's transport-class failure is returned as a PartialFailureError only
// when another LYRICS lane answered with a clean miss AND the failure opened
// its lane (a refusal); its class never changes.
func TestPartialFailureOnlyWhenALyricsLaneAnswered(t *testing.T) {
	transport := errors.New("dial tcp: connection refused")
	refusal := fmt.Errorf("edge: %w", musixmatch.ErrForbidden)
	detectorMiss := func() *Lane {
		return &Lane{
			name: "detector", breaker: circuit.New(time.Minute, time.Hour), instrumentalOnly: true,
			resolve: func(context.Context, models.Track, string) (models.Song, error) {
				return models.Song{}, ErrLaneBenignMiss
			},
			classifyErr: detectorClassifier,
		}
	}
	provider := func(name string, err error) *Lane { return laneFor(&stubProvider{name: name, err: err}) }
	// halfOpenProbe is a lane whose breaker tripped and whose window elapsed,
	// so the dispatch's call is the half-open probe.
	halfOpenProbe := func(name string, err error) *Lane {
		l, cb := newTestLane(&stubProvider{name: name, err: err})
		now := time.Now()
		cb.SetClock(func() time.Time { return now })
		cb.Trip()
		now = now.Add(2 * time.Minute)
		return l
	}
	cases := []struct {
		name    string
		lanes   func() []*Lane
		partial bool
		is      error
	}{
		{"refused lane opened, another lane missed", func() []*Lane {
			return []*Lane{provider("musixmatch", refusal), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, true, musixmatch.ErrForbidden},
		// The lane stays closed, so nothing bounds the fault: plain, as on main.
		{"failing lane left closed, another lane missed", func() []*Lane {
			return []*Lane{provider("musixmatch", transport), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, false, transport},
		// Which lane's error surfaces here is intentionally unspecified (it
		// differs by mode and arrival order); only the plain form is asserted.
		{"one lane opened, one left closed, another missed", func() []*Lane {
			return []*Lane{provider("musixmatch", refusal), provider("innertube", transport), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, false, nil},
		// A half-open probe that fails without tripping leaves the lane unbounded.
		{"half-open probe failed without tripping, another lane missed", func() []*Lane {
			return []*Lane{halfOpenProbe("musixmatch", transport), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, false, transport},
		{"every lane failed", func() []*Lane {
			return []*Lane{provider("musixmatch", transport), provider("petitlyrics", transport)}
		}, false, transport},
		{"only the detector answered", func() []*Lane {
			return []*Lane{provider("musixmatch", transport), detectorMiss()}
		}, false, transport},
		{"throttle outranks the miss", func() []*Lane {
			return []*Lane{provider("musixmatch", musixmatch.ErrRateLimited), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, false, musixmatch.ErrRateLimited},
	}
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				o, err := New(mode, tc.lanes()...)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				_, err = o.FindLyrics(context.Background(), models.Track{}, "")
				var partial *PartialFailureError
				if got := errors.As(err, &partial); got != tc.partial {
					t.Errorf("PartialFailureError = %v; want %v (err %v)", got, tc.partial, err)
				}
				if tc.is != nil && !errors.Is(err, tc.is) {
					t.Errorf("err = %v; want it to wrap %v", err, tc.is)
				}
				if tc.partial && ClassifyOutcome(err) != OutcomeTransport {
					t.Errorf("class = %v; want OutcomeTransport", ClassifyOutcome(err))
				}
			})
		}
	}
}

// A canceled half-open probe (parallel mode, another lane won) is not an
// answer, so it must not clear a standing refusal (#1372).
func TestCanceledProbeKeepsRefused(t *testing.T) {
	p := &stubProvider{name: "musixmatch", err: fmt.Errorf("edge: %w", musixmatch.ErrForbidden)}
	l, cb := newTestLane(p)
	now := time.Now()
	cb.SetClock(func() time.Time { return now })
	o, _ := New(ModeOrdered, l)
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")

	p.err = context.Canceled
	now = now.Add(2 * time.Minute)
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if h := o.LaneHealth()[0]; !h.Refused {
		t.Fatalf("health after a canceled probe = %+v; want Refused kept", h)
	}
}
