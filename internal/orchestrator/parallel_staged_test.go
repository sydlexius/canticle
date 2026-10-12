package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// Staged dispatch (#528): under parallel mode the detector lane runs outside
// the provider race, before it when first and after it when last.

func gateDetector(instrumental bool) *stubDetector {
	return &stubDetector{res: detector.Result{Instrumental: instrumental, Version: "1.5.0"}}
}

func stagedDetectorLane(d detector.Detector) *Lane {
	return NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil)
}

// attemptSet renders the attribution order-independently: parallel mode lists
// lanes in report order, ordered mode in lane order.
func attemptSet(song models.Song) string {
	out := make([]string, 0, len(song.LaneAttempts))
	for _, a := range song.LaneAttempts {
		out = append(out, fmt.Sprintf("%s hit=%v local=%v", a.Lane, a.Hit, a.Local))
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

func stagedFind(t *testing.T, sourcePath string, lanes ...*Lane) (models.Song, error) {
	t.Helper()
	return stagedRun(t, ModeParallel, sourcePath, 2*time.Second, nil, lanes...)
}

func stagedRun(t *testing.T, mode, sourcePath string, deadline time.Duration, blocks lyrics.BlockChecker, lanes ...*Lane) (models.Song, error) {
	t.Helper()
	o, err := New(mode, lanes...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.SetRaceWait(500 * time.Millisecond)
	o.SetBlockChecker(blocks)
	// A deadline, so a dispatch waiting on a lane nobody started fails, not hangs.
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	return o.FindLyrics(ctx, models.Track{TrackName: "Glass Harbor"}, sourcePath)
}

func TestParallelStaged_GatePositiveStartsNoProviderLane(t *testing.T) {
	d := gateDetector(true)
	p1 := &delayProvider{name: "musixmatch", song: syncedSong()}
	p2 := &delayProvider{name: "petitlyrics", song: syncedSong()}
	song, err := stagedFind(t, "/music/x.flac", stagedDetectorLane(d), delayLane(p1), delayLane(p2))
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	// Long enough for a stray lane goroutine to reach its provider.
	time.Sleep(30 * time.Millisecond)
	if n := atomic.LoadInt32(&p1.started) + atomic.LoadInt32(&p2.started); n != 0 {
		t.Errorf("provider requests = %d; a gate-positive detector-first dispatch must start no provider lane", n)
	}
	if song.WinningLane != detectorLaneName || d.calls != 1 {
		t.Errorf("winning lane = %q, detector calls = %d; want detector, 1", song.WinningLane, d.calls)
	}
	if got, want := attemptSet(song), "[detector hit=true local=true]"; got != want {
		t.Errorf("attempts = %s; want %s (local only, so the worker owes no pacing wait)", got, want)
	}
}

func TestParallelStaged_GateNegativeRacesAsBefore(t *testing.T) {
	d := gateDetector(false)
	fast := &delayProvider{name: "musixmatch", song: unsyncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 40 * time.Millisecond}
	song, err := stagedFind(t, "/music/x.flac", stagedDetectorLane(d), delayLane(fast), delayLane(slow))
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualitySynced || song.WinningLane != "petitlyrics" {
		t.Errorf("quality = %v, lane = %q; want the slower synced result to preempt the held unsynced one", QualityOf(song), song.WinningLane)
	}
	want := "[detector hit=false local=true musixmatch hit=false local=false petitlyrics hit=true local=false]"
	if got := attemptSet(song); got != want || d.calls != 1 {
		t.Errorf("attempts = %s, detector calls = %d; want %s, 1", got, d.calls, want)
	}
}

func TestParallelStaged_OutageFallsThroughAndTrips(t *testing.T) {
	d := &stubDetector{err: errors.New("connection reset")}
	br := circuit.New(time.Minute, time.Hour)
	p := &delayProvider{name: "musixmatch", song: syncedSong()}
	o, err := New(ModeParallel, NewDetectorLane(d, br, nil), delayLane(p))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for pass, wantAttempts := range []string{
		"[detector hit=false local=true musixmatch hit=true local=false]",
		// Breaker open: the lane is skipped, not attempted.
		"[musixmatch hit=true local=false]",
	} {
		song, err := o.FindLyrics(ctx, models.Track{}, "/music/x.flac")
		if err != nil || song.WinningLane != "musixmatch" {
			t.Fatalf("pass %d: lane = %q, err = %v; want the provider's lyric", pass, song.WinningLane, err)
		}
		if got := attemptSet(song); got != wantAttempts {
			t.Errorf("pass %d: attempts = %s; want %s", pass, got, wantAttempts)
		}
	}
	if d.calls != 1 || br.Trips() != 1 || p.started != 2 {
		t.Errorf("detector calls = %d, trips = %d, provider calls = %d; want 1, 1, 2", d.calls, br.Trips(), p.started)
	}
}

func TestParallelStaged_NotReadyOutranksProviderMiss(t *testing.T) {
	d := &scriptedDetector{steps: []scriptStep{{err: notReadyErr()}}}
	p := &delayProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	_, err := stagedFind(t, "/music/x.flac", stagedDetectorLane(d), delayLane(p))
	if !errors.Is(err, ErrLaneNotReady) {
		t.Fatalf("err = %v; want ErrLaneNotReady (the row is released, not charged a miss)", err)
	}
}

// The karaoke skip reaches the lane as an empty path (worker skipDetectorFirst).
func TestParallelStaged_EmptyPathNeverCallsDetect(t *testing.T) {
	d := gateDetector(true)
	p := &delayProvider{name: "musixmatch", song: syncedSong()}
	song, err := stagedFind(t, "", stagedDetectorLane(d), delayLane(p))
	if err != nil || song.WinningLane != "musixmatch" || d.calls != 0 {
		t.Fatalf("lane = %q, err = %v, detector calls = %d; want musixmatch, nil, 0", song.WinningLane, err, d.calls)
	}
}

func TestParallelStaged_DemotedRunsOnlyAfterAllMiss(t *testing.T) {
	miss := func() *delayProvider { return &delayProvider{name: "musixmatch", err: musixmatch.ErrNotFound} }
	for _, tc := range []struct {
		name      string
		second    *delayProvider
		wantLane  string
		wantCalls int
	}{
		// The detector would answer long before the slow lane does.
		{"slow synced lyric", &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 40 * time.Millisecond}, "petitlyrics", 0},
		{"held unsynced lyric", &delayProvider{name: "petitlyrics", song: unsyncedSong(), delay: 10 * time.Millisecond}, "petitlyrics", 0},
		{"every provider misses", &delayProvider{name: "petitlyrics", err: musixmatch.ErrNotFound, delay: 10 * time.Millisecond}, detectorLaneName, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := gateDetector(true)
			song, err := stagedFind(t, "/music/x.flac", delayLane(miss()), delayLane(tc.second), stagedDetectorLane(d))
			if err != nil {
				t.Fatalf("FindLyrics: %v", err)
			}
			if song.WinningLane != tc.wantLane || d.calls != tc.wantCalls {
				t.Errorf("lane = %q, detector calls = %d; want %q, %d", song.WinningLane, d.calls, tc.wantLane, tc.wantCalls)
			}
			// Both providers were asked first, so the attribution is never local-only.
			if len(song.LaneAttempts) != 2+tc.wantCalls {
				t.Errorf("attempts = %s; want both providers and %d detector row(s)", attemptSet(song), tc.wantCalls)
			}
		})
	}
}

func TestParallelStaged_OnlyStagedLanesDoesNotHang(t *testing.T) {
	d := gateDetector(false)
	song, err := stagedFind(t, "/music/x.flac", stagedDetectorLane(d))
	if !errors.Is(err, ErrLaneBenignMiss) {
		t.Fatalf("err = %v; want the detector's benign miss (a deadline error means the dispatch waited on a race with no lanes)", err)
	}
	if got, want := attemptSet(song), "[detector hit=false local=true]"; got != want {
		t.Errorf("attempts = %s; want %s", got, want)
	}
}

// blockAll is a block check that refuses every result it is shown.
type blockAll struct{}

func (blockAll) SongBlocked(context.Context, models.Song) bool { return true }

// TestParallelStaged_AgreesWithOrdered: runStaged is a second copy of the
// per-lane rules in findOrdered, so the same lanes and verdicts must give the
// same winner, error class and attempt set in both modes.
func TestParallelStaged_AgreesWithOrdered(t *testing.T) {
	positive := func() detector.Detector { return gateDetector(true) }
	negative := func() detector.Detector { return gateDetector(false) }
	outage := func() detector.Detector { return &stubDetector{err: errors.New("connection reset")} }
	booting := func() detector.Detector { return &scriptedDetector{steps: []scriptStep{{err: notReadyErr()}}} }
	for _, tc := range []struct {
		name     string
		det      func() detector.Detector
		front    bool
		provider delayProvider
		blocks   lyrics.BlockChecker
	}{
		{"front positive", positive, true, delayProvider{song: syncedSong()}, nil},
		{"front negative, hit", negative, true, delayProvider{song: unsyncedSong()}, nil},
		{"front negative, miss", negative, true, delayProvider{err: musixmatch.ErrNotFound}, nil},
		{"front outage, miss", outage, true, delayProvider{err: musixmatch.ErrNotFound}, nil},
		{"front booting, miss", booting, true, delayProvider{err: musixmatch.ErrNotFound}, nil},
		{"front booting, throttled", booting, true, delayProvider{err: musixmatch.ErrRateLimited}, nil},
		{"demoted positive, hit", positive, false, delayProvider{song: syncedSong()}, nil},
		{"demoted positive, miss", positive, false, delayProvider{err: musixmatch.ErrNotFound}, nil},
		{"demoted positive, provider instrumental", positive, false, delayProvider{song: instrumentalSong()}, nil},
		{"demoted outage, miss", outage, false, delayProvider{err: musixmatch.ErrNotFound}, nil},
		{"front positive, blocked", positive, true, delayProvider{err: musixmatch.ErrNotFound}, blockAll{}},
		{"demoted positive, blocked", positive, false, delayProvider{err: musixmatch.ErrNotFound}, blockAll{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got [2]string
			for i, mode := range []string{ModeOrdered, ModeParallel} {
				p := tc.provider
				p.name = "musixmatch"
				lanes := []*Lane{stagedDetectorLane(tc.det()), delayLane(&p)}
				if !tc.front {
					lanes[0], lanes[1] = lanes[1], lanes[0]
				}
				song, err := stagedRun(t, mode, "/music/x.flac", 2*time.Second, tc.blocks, lanes...)
				got[i] = fmt.Sprintf("lane=%q err=%v attempts=%s", song.WinningLane, err, attemptSet(song))
			}
			if got[0] != got[1] {
				t.Errorf("modes disagree:\nordered  %s\nparallel %s", got[0], got[1])
			}
		})
	}
}

// A demoted detector answers only after an asked provider (#528): with every
// provider breaker open the row waits, exactly as it did before the lane existed.
func TestParallelStaged_DemotedNeedsAnAskedProvider(t *testing.T) {
	for _, asked := range []bool{false, true} {
		d := gateDetector(true)
		open := delayLane(&delayProvider{name: "musixmatch"})
		second := delayLane(&delayProvider{name: "petitlyrics", err: musixmatch.ErrNotFound})
		open.Breaker().Trip()
		wantLane, wantCalls, wantErr := detectorLaneName, 1, error(nil)
		if !asked {
			second.Breaker().Trip()
			wantLane, wantCalls, wantErr = "", 0, ErrLaneUnavailable
		}
		song, err := stagedFind(t, "/music/x.flac", open, second, stagedDetectorLane(d))
		if !errors.Is(err, wantErr) || song.WinningLane != wantLane || d.calls != wantCalls {
			t.Errorf("asked=%v: err = %v, lane = %q, detector calls = %d; want %v, %q, %d", asked, err, song.WinningLane, d.calls, wantErr, wantLane, wantCalls)
		}
	}
}

// lateDetector blocks until the dispatch is canceled, then answers gate-positive.
type lateDetector struct{}

func (lateDetector) Detect(ctx context.Context, _ string) (detector.Result, error) {
	<-ctx.Done()
	return detector.Result{Instrumental: true, Version: "1.5.0"}, nil
}

func TestParallelStaged_CanceledDuringStagedLaneCommitsNothing(t *testing.T) {
	for _, front := range []bool{true, false} {
		p := &delayProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
		lanes := []*Lane{stagedDetectorLane(lateDetector{}), delayLane(p)}
		wantStarted := int32(0)
		if !front {
			lanes[0], lanes[1], wantStarted = lanes[1], lanes[0], 1
		}
		song, err := stagedRun(t, ModeParallel, "/music/x.flac", 50*time.Millisecond, nil, lanes...)
		if n := atomic.LoadInt32(&p.started); !errors.Is(err, context.DeadlineExceeded) || song.WinningLane != "" || n != wantStarted {
			t.Errorf("front=%v: err = %v, lane = %q, provider requests = %d; want the deadline error, no winner, %d", front, err, song.WinningLane, n, wantStarted)
		}
	}
}
