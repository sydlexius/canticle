package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
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

	p.err = musixmatch.ErrNotFound
	now = now.Add(2 * time.Minute)
	_, _ = l.FindLyrics(context.Background(), models.Track{}, "")
	if h := o.LaneHealth()[0]; h.State != LaneStateClosed || h.Refused {
		t.Fatalf("health after the provider answered = %+v; want closed and not Refused", h)
	}
}

// A lane's transport-class failure is returned as a PartialFailureError only
// when another LYRICS lane answered with a clean miss; its class never changes.
func TestPartialFailureOnlyWhenALyricsLaneAnswered(t *testing.T) {
	transport := errors.New("dial tcp: connection refused")
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
	cases := []struct {
		name    string
		lanes   func() []*Lane
		partial bool
		is      error
	}{
		{"another lane missed", func() []*Lane {
			return []*Lane{provider("musixmatch", transport), provider("petitlyrics", petitlyrics.ErrNoMatch)}
		}, true, transport},
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
				if !errors.Is(err, tc.is) {
					t.Errorf("err = %v; want it to wrap %v", err, tc.is)
				}
				if tc.partial && ClassifyOutcome(err) != OutcomeTransport {
					t.Errorf("class = %v; want OutcomeTransport", ClassifyOutcome(err))
				}
			})
		}
	}
}
