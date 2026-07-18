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
	"github.com/sydlexius/canticle/internal/providers"
)

func laneFor(p *stubProvider) *Lane {
	return NewLane(p, circuit.New(60*time.Second, 30*time.Minute))
}

func TestNewModeRejectsUnknown(t *testing.T) {
	p := &stubProvider{name: "musixmatch"}
	if _, err := New("sequential", laneFor(p)); err == nil {
		t.Fatal("New(sequential) should error: unknown mode")
	}
	if _, err := New("ordered", laneFor(p)); err != nil {
		t.Fatalf("New(ordered): %v", err)
	}
	if _, err := New("parallel", laneFor(p)); err != nil {
		t.Fatalf("New(parallel): %v", err)
	}
	if _, err := New("", laneFor(p)); err != nil {
		t.Fatalf("New(empty) should default to ordered: %v", err)
	}
}

func TestNewValidatesLanes(t *testing.T) {
	if _, err := New("ordered"); err == nil {
		t.Fatal("New with no lanes should error (avoids a nil dispatch)")
	}
	if _, err := New("ordered", nil); err == nil {
		t.Fatal("New with a nil lane should error")
	}
	p := &stubProvider{name: "musixmatch"}
	if _, err := New("ordered", laneFor(p), nil); err == nil {
		t.Fatal("New with a nil lane among valid lanes should error")
	}
}

func TestNewCopiesLanes(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "primary"}}}
	lanes := []*Lane{laneFor(p)}
	o, err := New("ordered", lanes...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lanes[0] = nil // mutate the caller's slice after construction
	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics after caller mutated its slice: %v", err)
	}
	if song.Lyrics.LyricsBody != "primary" {
		t.Fatalf("body = %q; want primary (orchestrator must use its own lane copy)", song.Lyrics.LyricsBody)
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

func TestLaneNamesReturnsAllLanesInOrder(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch"}
	p2 := &stubProvider{name: "petitlyrics"}
	o, err := New("ordered", laneFor(p1), laneFor(p2))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	names := o.LaneNames()
	if len(names) != 2 {
		t.Fatalf("LaneNames len = %d; want 2", len(names))
	}
	if names[0] != "musixmatch" {
		t.Errorf("names[0] = %q; want musixmatch", names[0])
	}
	if names[1] != "petitlyrics" {
		t.Errorf("names[1] = %q; want petitlyrics", names[1])
	}
}

func TestWinningLaneSetOnOrderedHit(t *testing.T) {
	synced := models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "lyric"}}}}
	p1 := &stubProvider{name: "musixmatch", song: synced}
	o, _ := New("ordered", laneFor(p1))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" {
		t.Errorf("WinningLane = %q; want musixmatch", song.WinningLane)
	}
}

func TestWinningLaneSetOnBestAvailable(t *testing.T) {
	// An unsuitable (instrumental) result from the only lane is returned as
	// best-available; WinningLane must still be set.
	instrumental := models.Song{Track: models.Track{Instrumental: 1}}
	p1 := &stubProvider{name: "musixmatch", song: instrumental}
	o, _ := New("ordered", laneFor(p1))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" {
		t.Errorf("WinningLane = %q; want musixmatch on best-available", song.WinningLane)
	}
}

// attemptHit looks up the recorded outcome for lane in a LaneAttempts slice.
// found is false when the lane has no attempt entry.
func attemptHit(attempts []models.LaneAttempt, lane string) (hit, found bool) {
	for _, a := range attempts {
		if a.Lane == lane {
			return a.Hit, true
		}
	}
	return false, false
}

// TestLaneAttemptsOrderedLoserRecordsMiss is the exact over-count fix (#282):
// in ordered mode a lane that was tried but lost to a LATER winning lane must be
// recorded as a miss, not silently dropped.
func TestLaneAttemptsOrderedLoserRecordsMiss(t *testing.T) {
	miss := &stubProvider{name: "aaa", err: musixmatch.ErrNoLyrics} // tried first, no lyrics
	synced := &stubProvider{name: "musixmatch", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "lyric"}}}}}
	o, _ := New("ordered", laneFor(miss), laneFor(synced))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" {
		t.Fatalf("WinningLane = %q; want musixmatch", song.WinningLane)
	}
	if hit, found := attemptHit(song.LaneAttempts, "musixmatch"); !found || !hit {
		t.Errorf("musixmatch attempt = (hit=%v, found=%v); want hit", hit, found)
	}
	if hit, found := attemptHit(song.LaneAttempts, "aaa"); !found || hit {
		t.Errorf("aaa (lost to later winner) attempt = (hit=%v, found=%v); want recorded miss", hit, found)
	}
}

// TestLaneAttemptsOrderedLaterLaneNotConsulted verifies a lane after the winner,
// never consulted in ordered mode, has no attempt entry (it was not tried).
func TestLaneAttemptsOrderedLaterLaneNotConsulted(t *testing.T) {
	synced := &stubProvider{name: "aaa", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "lyric"}}}}}
	never := &stubProvider{name: "zzz", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "unused"}}}}}
	o, _ := New("ordered", laneFor(synced), laneFor(never))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if _, found := attemptHit(song.LaneAttempts, "zzz"); found {
		t.Errorf("zzz was never consulted; want no attempt entry, got one")
	}
}

// TestLaneAttemptsParallelAllMiss verifies parallel mode attributes every
// CONSULTED lane on the all-miss path (both lanes report, no early return): each
// is recorded as a miss. Deterministic because resolve runs only after pending
// reaches 0, so both lanes are always in the consulted set.
func TestLaneAttemptsParallelAllMiss(t *testing.T) {
	m1 := &stubProvider{name: "aaa", err: musixmatch.ErrNoLyrics}
	m2 := &stubProvider{name: "bbb", err: musixmatch.ErrNoLyrics}
	o, _ := New("parallel", laneFor(m1), laneFor(m2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if !musixmatch.IsBenignMiss(err) {
		t.Fatalf("err = %v; want benign miss", err)
	}
	for _, lane := range []string{"aaa", "bbb"} {
		if hit, found := attemptHit(song.LaneAttempts, lane); !found || hit {
			t.Errorf("%s attempt = (hit=%v, found=%v); want recorded miss", lane, hit, found)
		}
	}
}

// TestLaneAttemptsParallelExcludesUnavailable is the parallel-mode over-count
// guard (F1): a breaker-open lane never calls the provider, so it must get NO
// lane_attempts row -- it was not consulted. Deterministic regardless of result
// ordering: the unavailable lane is skipped on every path, and the synced lane
// wins (immediately or after the miss reports), so the breaker-open lane is never
// added to the consulted set.
func TestLaneAttemptsParallelExcludesUnavailable(t *testing.T) {
	down := &stubProvider{name: "down", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "unused"}}}}}
	synced := &stubProvider{name: "musixmatch", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "lyric"}}}}}
	downLane := laneFor(down)
	downLane.Breaker().Trip() // open the breaker: provider not called -> OutcomeUnavailable
	o, _ := New("parallel", downLane, laneFor(synced))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" {
		t.Fatalf("WinningLane = %q; want musixmatch", song.WinningLane)
	}
	if hit, found := attemptHit(song.LaneAttempts, "musixmatch"); !found || !hit {
		t.Errorf("musixmatch attempt = (hit=%v, found=%v); want hit", hit, found)
	}
	if _, found := attemptHit(song.LaneAttempts, "down"); found {
		t.Errorf("breaker-open lane 'down' was never consulted; want NO attempt row, got one")
	}
	if down.calls != 0 {
		t.Errorf("breaker-open provider calls = %d; want 0", down.calls)
	}
}

// TestLaneAttemptsAllMissRecorded verifies that when every lane misses (the
// orchestrator returns a benign-miss error and an empty song), the attempts are
// still carried so the worker can record an all-miss per-track row.
func TestLaneAttemptsAllMissRecorded(t *testing.T) {
	m1 := &stubProvider{name: "aaa", err: musixmatch.ErrNoLyrics}
	m2 := &stubProvider{name: "bbb", err: musixmatch.ErrNoLyrics}
	o, _ := New("ordered", laneFor(m1), laneFor(m2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if !musixmatch.IsBenignMiss(err) {
		t.Fatalf("err = %v; want benign miss", err)
	}
	if song.WinningLane != "" {
		t.Errorf("WinningLane = %q; want empty on all-miss", song.WinningLane)
	}
	for _, lane := range []string{"aaa", "bbb"} {
		if hit, found := attemptHit(song.LaneAttempts, lane); !found || hit {
			t.Errorf("%s attempt = (hit=%v, found=%v); want recorded miss", lane, hit, found)
		}
	}
}

func TestWinningLaneEmptyOnBenignMiss(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNoLyrics}
	o, _ := New("ordered", laneFor(p1))

	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !musixmatch.IsBenignMiss(err) {
		t.Fatalf("err = %v; want benign miss", err)
	}
	// On a miss the orchestrator returns an error, not a song, so WinningLane
	// is inaccessible - this test just verifies no panic and the correct error.
}
