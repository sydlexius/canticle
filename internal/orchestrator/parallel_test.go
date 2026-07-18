package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// delayProvider is a concurrency-safe fake provider with a configurable per-call
// delay that honors context cancellation. started/finished track launched vs.
// returned calls so a test can assert every dispatched goroutine unblocked (no
// leak) without relying solely on a flaky runtime.NumGoroutine snapshot.
type delayProvider struct {
	name     string
	song     models.Song
	err      error
	delay    time.Duration
	started  int32
	finished int32
}

func (p *delayProvider) Name() string { return p.name }

func (p *delayProvider) FindLyrics(ctx context.Context, _ models.Track) (models.Song, error) {
	atomic.AddInt32(&p.started, 1)
	defer atomic.AddInt32(&p.finished, 1)
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return models.Song{}, ctx.Err()
		}
	}
	if p.err != nil {
		return models.Song{}, p.err
	}
	return p.song, nil
}

func delayLane(p *delayProvider) *Lane {
	return NewLane(p, circuit.New(60*time.Second, 30*time.Minute))
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestParallelSyncedPreemptsSlowerUnsynced(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: unsyncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 40 * time.Millisecond}
	o, _ := New("parallel", delayLane(fast), delayLane(slow))
	o.SetRaceWait(500 * time.Millisecond)

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualitySynced {
		t.Fatalf("quality = %v; want synced (synced upgrade preempts held unsynced within window)", QualityOf(song))
	}
}

func TestParallelUnsyncedCommittedWhenWindowElapses(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: unsyncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 200 * time.Millisecond}
	o, _ := New("parallel", delayLane(fast), delayLane(slow))
	o.SetRaceWait(20 * time.Millisecond)

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualityUnsynced {
		t.Fatalf("quality = %v; want unsynced (window elapsed before the synced result arrived)", QualityOf(song))
	}
}

func TestParallelFastSyncedWinsImmediately(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: syncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 300 * time.Millisecond}
	o, _ := New("parallel", delayLane(fast), delayLane(slow))
	o.SetRaceWait(5 * time.Second)

	start := time.Now()
	song, err := o.FindLyrics(context.Background(), models.Track{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualitySynced {
		t.Fatalf("quality = %v; want synced", QualityOf(song))
	}
	if elapsed > time.Second {
		t.Fatalf("returned in %v; a fast synced result must not wait for the slow lane or the race window", elapsed)
	}
}

func TestParallelSingleUnsyncedCommitsImmediately(t *testing.T) {
	// A held unsynced result with no other lane in flight cannot be upgraded, so it
	// must commit immediately rather than burn the full race window.
	p := &delayProvider{name: "musixmatch", song: unsyncedSong()}
	o, _ := New("parallel", delayLane(p))
	o.SetRaceWait(5 * time.Second)

	start := time.Now()
	song, err := o.FindLyrics(context.Background(), models.Track{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if QualityOf(song) != QualityUnsynced {
		t.Fatalf("quality = %v; want unsynced", QualityOf(song))
	}
	if elapsed > time.Second {
		t.Fatalf("returned in %v; a single unsynced lane has no possible upgrade and must commit immediately", elapsed)
	}
}

func TestParallelAllMissReturnsBenignMiss(t *testing.T) {
	p1 := &delayProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	p2 := &delayProvider{name: "petitlyrics", err: musixmatch.ErrNotFound}
	o, _ := New("parallel", delayLane(p1), delayLane(p2))

	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound (all lanes missed)", err)
	}
}

func TestParallelBestAvailableWhenNoSuitable(t *testing.T) {
	p1 := &delayProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	p2 := &delayProvider{name: "petitlyrics", song: models.Song{Track: models.Track{Instrumental: 1}}}
	o, _ := New("parallel", delayLane(p1), delayLane(p2))

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v (best-available instrumental should return nil err)", err)
	}
	if QualityOf(song) != QualityInstrumental {
		t.Fatalf("quality = %v; want instrumental (best available)", QualityOf(song))
	}
}

func TestParallelAuthOutranksBenignMiss(t *testing.T) {
	p1 := &delayProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}
	p2 := &delayProvider{name: "petitlyrics", err: musixmatch.ErrNotFound}
	o, _ := New("parallel", delayLane(p1), delayLane(p2))

	_, err := o.FindLyrics(context.Background(), models.Track{})
	if !errors.Is(err, musixmatch.ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited (auth outranks benign miss)", err)
	}
}

func TestParallelNoGoroutineLeak(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: syncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: 150 * time.Millisecond}
	o, _ := New("parallel", delayLane(fast), delayLane(slow))

	if _, err := o.FindLyrics(context.Background(), models.Track{}); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}

	// The losing slow lane must observe the cancel and return; finished catches up
	// to started for every dispatched goroutine. This is a deterministic per-test
	// leak signal, unlike a process-global runtime.NumGoroutine() snapshot which can
	// flap on unrelated background goroutines.
	waitFor(t, func() bool {
		return atomic.LoadInt32(&slow.finished) == atomic.LoadInt32(&slow.started) &&
			atomic.LoadInt32(&fast.finished) == atomic.LoadInt32(&fast.started)
	}, "dispatched provider goroutines did not all unblock after cancel")
}

func TestParallelParentCancelReturnsErr(t *testing.T) {
	slow := &delayProvider{name: "musixmatch", song: syncedSong(), delay: time.Second}
	o, _ := New("parallel", delayLane(slow))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := o.FindLyrics(ctx, models.Track{})
		errCh <- err
	}()
	// Require the lane to be genuinely in flight before canceling; fail loudly if
	// it never is, so the test cannot pass without exercising the in-flight branch.
	waitFor(t, func() bool {
		return atomic.LoadInt32(&slow.started) != 0
	}, "slow lane never entered the in-flight state before cancel")
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled (parent canceled mid-flight)", err)
	}
	waitFor(t, func() bool {
		return atomic.LoadInt32(&slow.finished) == atomic.LoadInt32(&slow.started)
	}, "in-flight lane did not terminate after parent cancel")
}

// TestParallelParentCancelAfterHeldReturnsErr asserts that once an unsynced result
// is held awaiting a synced upgrade, a parent cancellation still wins: the held
// result must NOT be committed as a successful write during shutdown/abort.
func TestParallelParentCancelAfterHeldReturnsErr(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: unsyncedSong()}
	slow := &delayProvider{name: "petitlyrics", song: syncedSong(), delay: time.Second}
	o, _ := New("parallel", delayLane(fast), delayLane(slow))
	o.SetRaceWait(5 * time.Second) // wide window: the held unsynced is parked when cancel lands

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := o.FindLyrics(ctx, models.Track{})
		errCh <- err
	}()
	// Require the held-unsynced + armed-race-window state before canceling; fail
	// loudly if it is never reached, so the test cannot pass without exercising it.
	waitFor(t, func() bool {
		return atomic.LoadInt32(&fast.finished) != 0 && atomic.LoadInt32(&slow.started) != 0
	}, "held-unsynced / race-window state was never reached before cancel")
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled (cancel must beat a held unsynced commit)", err)
	}
	// Both lanes must drain after the cancel: the held (fast) lane already returned,
	// and the in-flight slow lane must observe the cancel and exit (no leak).
	waitFor(t, func() bool {
		return atomic.LoadInt32(&fast.finished) == atomic.LoadInt32(&fast.started) &&
			atomic.LoadInt32(&slow.finished) == atomic.LoadInt32(&slow.started)
	}, "provider goroutines did not terminate after parent cancel")
}

func TestParallelGuardDrivesSuitability(t *testing.T) {
	// A guard-rejected synced result is not suitable; the other lane's suitable
	// synced result must win the race.
	rejected := &delayProvider{name: "musixmatch", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "reject"}}}}}
	accepted := &delayProvider{name: "petitlyrics", song: models.Song{Subtitles: models.Synced{Lines: []models.Lines{{Text: "keep"}}}}}
	o, _ := New("parallel", delayLane(rejected), delayLane(accepted))
	o.SetGuard(scriptOnlyGuard{accept: func(s models.Song) bool {
		return len(s.Subtitles.Lines) > 0 && s.Subtitles.Lines[0].Text == "keep"
	}})

	song, err := o.FindLyrics(context.Background(), models.Track{})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) == 0 || song.Subtitles.Lines[0].Text != "keep" {
		t.Fatalf("got %+v; want the guard-accepted result", song.Subtitles.Lines)
	}
}

// scriptOnlyGuard is an always-enabled guard whose accept decision is a closure,
// used to verify the orchestrator consults the guard during parallel suitability.
type scriptOnlyGuard struct{ accept func(models.Song) bool }

func (g scriptOnlyGuard) Enabled() bool { return true }
func (g scriptOnlyGuard) Accept(s models.Song) (bool, string) {
	if g.accept(s) {
		return true, ""
	}
	return false, "rejected"
}
