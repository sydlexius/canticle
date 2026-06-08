package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/circuit"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
)

func laneFor(p *stubProvider) *Lane {
	return NewLane(p, circuit.New(60*time.Second, 30*time.Minute))
}

func TestNewModeRejectsUnknown(t *testing.T) {
	if _, err := New("parallel", nil); err == nil {
		t.Fatal("New(parallel) should error: only ordered is supported")
	}
	if _, err := New("ordered"); err != nil {
		t.Fatalf("New(ordered): %v", err)
	}
	if _, err := New(""); err != nil {
		t.Fatalf("New(empty) should default to ordered: %v", err)
	}
}

func TestOrderedReturnsFirstSuitable(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "primary"}}}
	p2 := &stubProvider{name: "petitlyrics", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "secondary"}}}
	o, _ := New("ordered", laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Lyrics.LyricsBody != "primary" {
		t.Fatalf("body = %q; want primary (first suitable wins)", song.Lyrics.LyricsBody)
	}
	if p2.calls != 0 {
		t.Fatalf("secondary calls = %d; want 0 (primary was suitable)", p2.calls)
	}
}

func TestOrderedFallsThroughToSuitable(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	p2 := &stubProvider{name: "petitlyrics", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "secondary"}}}
	o, _ := New("ordered", laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Lyrics.LyricsBody != "secondary" {
		t.Fatalf("body = %q; want secondary (primary missed)", song.Lyrics.LyricsBody)
	}
}

func TestOrderedSkipsUnsuitableInstrumentalForSyncedLater(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", song: models.Song{Track: models.Track{Instrumental: 1}}}
	p2 := &stubProvider{name: "petitlyrics", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "hi"}}}}}
	o, _ := New("ordered", laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualitySynced {
		t.Fatalf("quality = %v; want synced (instrumental is not suitable, synced lane wins)", QualityOf(song))
	}
}

func TestOrderedReturnsBestAvailableWhenNoSuitable(t *testing.T) {
	// No lane is suitable; the best available (instrumental) must be returned with
	// a nil error so the worker writes the instrumental marker.
	p1 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	p2 := &stubProvider{name: "petitlyrics", song: models.Song{Track: models.Track{Instrumental: 1}}}
	o, _ := New("ordered", laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v (best-available instrumental should return nil err)", err)
	}
	if QualityOf(song) != QualityInstrumental {
		t.Fatalf("quality = %v; want instrumental (best available)", QualityOf(song))
	}
}

func TestOrderedReturnsBenignMissWhenAllMiss(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	o, _ := New("ordered", laneFor(p1))

	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound (all lanes missed)", err)
	}
}

func TestOrderedAuthOutranksBenignMiss(t *testing.T) {
	// Primary throttles, secondary misses. The auth/rate-limit signal must win so
	// the worker backs off rather than recording a stable miss.
	p1 := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrRateLimited)}
	p2 := &stubProvider{name: "petitlyrics", err: musixmatch.ErrNotFound}
	o, _ := New("ordered", laneFor(p1), laneFor(p2))

	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited (auth outranks benign miss)", err)
	}
}

func TestOrderedAllUnavailableReturnsSentinel(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", err: fmt.Errorf("x: %w", musixmatch.ErrRateLimited)}
	l1 := laneFor(p1)
	fixed := time.Now()
	l1.Breaker().SetClock(func() time.Time { return fixed })
	l1.Breaker().Trip() // open it

	o, _ := New("ordered", l1)
	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("err = %v; want ErrLaneUnavailable (all lanes' breakers open)", err)
	}
	if p1.calls != 0 {
		t.Fatalf("provider calls = %d; want 0 (open breaker)", p1.calls)
	}
}

func TestOrderedGuardRejectionIsUnsuitable(t *testing.T) {
	// A synced result the guard rejects is not suitable. With no fallback lane the
	// orchestrator has no suitable and no better-than-none result, so it returns
	// the result as best-available (the worker's guard re-check then handles the
	// terminal policy rejection, preserving current single-lane behavior).
	p1 := &stubProvider{name: "musixmatch", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}}}}
	o, _ := New("ordered", laneFor(p1))
	o.SetGuard(acceptGuard{enabled: true, accept: false})

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualitySynced {
		t.Fatalf("quality = %v; want synced returned as best-available", QualityOf(song))
	}
}

// Ensure the orchestrator satisfies providers.Fetcher (drop-in for the worker).
var _ providers.Fetcher = (*Orchestrator)(nil)
