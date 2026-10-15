package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptRepair is a Repair whose Run is a test's function.
type scriptRepair struct {
	info Info
	run  func(ctx context.Context, o Options) (Result, error)
}

func (s scriptRepair) Info() Info                                      { return s.info }
func (s scriptRepair) Pending(context.Context, Options) (int64, error) { return 0, nil }
func (s scriptRepair) Run(ctx context.Context, o Options) (Result, error) {
	return s.run(ctx, o)
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestRunner registers repairs on a fresh registry and returns a runner on a
// fake clock, closed when the test ends.
func newTestRunner(t *testing.T, repairs ...Repair) (*Runner, *fakeClock) {
	t.Helper()
	reg := NewRegistry()
	for _, rep := range repairs {
		if err := reg.Register(rep); err != nil {
			t.Fatal(err)
		}
	}
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := NewRunner(reg, nil)
	r.now = clock.now
	t.Cleanup(func() { _ = r.Close() })
	return r, clock
}

// waitRun waits for the live run's goroutine, if any, to return.
func waitRun(t *testing.T, r *Runner) {
	t.Helper()
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()
	if live == nil {
		return
	}
	select {
	case <-live:
	case <-time.After(10 * time.Second): // a hang guard, not a synchronization
		t.Fatal("run did not return")
	}
}

// captureLog sends slog's default logger to a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// blocking returns a repair that reports started, reports progress done of
// total, adds done to "checked" and waits for release or its cancel.
func blocking(key Key, done, total int64) (rep scriptRepair, started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	info := validInfo(key)
	rep = scriptRepair{info: info, run: func(ctx context.Context, o Options) (Result, error) {
		res := NewResult(info.Counts...)
		o.Report(done, total)
		res.Add("checked", done)
		close(started)
		select {
		case <-release:
			return res, nil
		case <-ctx.Done():
			return res, fmt.Errorf("stopped at a row boundary: %w", ctx.Err())
		}
	}}
	return rep, started, release
}

func TestRunnerStartRefusesSecondLiveRun(t *testing.T) {
	a, started, release := blocking("settle-tiers", 1, 2)
	b, _, _ := blocking("tag-reread", 0, 0)
	r, _ := newTestRunner(t, a, b)
	id, err := r.Start("settle-tiers", Options{Apply: true})
	if err != nil || id != 1 {
		t.Fatalf("Start = %d, %v", id, err)
	}
	<-started
	for _, key := range []Key{"settle-tiers", "tag-reread"} {
		if _, err := r.Start(key, Options{}); !errors.Is(err, ErrBusy) {
			t.Errorf("second Start(%s) = %v, want ErrBusy", key, err)
		}
	}
	if s := r.Snapshot(); s.State != StateRunning || s.Key != "settle-tiers" || !s.Apply || s.ID != 1 {
		t.Errorf("snapshot while running = %+v", s)
	}
	close(release)
	waitRun(t, r)
	s := r.Snapshot()
	if n, _ := count(s, "checked"); s.State != StateDone || s.Failure != FailureNone || n != 1 {
		t.Errorf("snapshot after release = %+v", s)
	}
	if _, err := r.Start("tag-reread", Options{}); err != nil {
		t.Errorf("Start after the run ended = %v", err)
	}
}

// admitAll is a RunStore that never refuses, as a jobs table might when a
// crashed process left no running row.
type admitAll struct{ MemStore }

func (a *admitAll) Begin(s Snapshot) (int64, error) {
	a.Finish(a.Load().ID, Snapshot{State: StateDone})
	return a.MemStore.Begin(s)
}

// TestRunnerOneGoroutineWhateverTheStore: the runner itself refuses a Start
// while its run's goroutine is live, so a store that admits too much cannot
// put two runs on one runner.
func TestRunnerOneGoroutineWhateverTheStore(t *testing.T) {
	rep, started, release := blocking("settle-tiers", 0, 0)
	reg := NewRegistry()
	if err := reg.Register(rep); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(reg, &admitAll{})
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := r.Start("settle-tiers", Options{}); !errors.Is(err, ErrBusy) {
		t.Errorf("second Start = %v, want ErrBusy", err)
	}
	close(release)
	waitRun(t, r)
}

func TestRunnerCancelReportsPartialCounts(t *testing.T) {
	rep, started, _ := blocking("settle-tiers", 3, 10)
	r, _ := newTestRunner(t, rep)
	if r.Cancel() {
		t.Error("Cancel with no run reported one")
	}
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if !r.Cancel() {
		t.Fatal("Cancel found no live run")
	}
	waitRun(t, r)
	s := r.Snapshot()
	if s.State != StateCanceled || s.Failure != FailureNone {
		t.Fatalf("state = %s/%s, want canceled", s.State, s.Failure)
	}
	if n, ok := count(s, "checked"); !ok || n != 3 || s.Done != 3 || s.Total != 10 {
		t.Errorf("partial counts = %+v, done %d of %d", s.Counts, s.Done, s.Total)
	}
	if r.Cancel() {
		t.Error("Cancel after the run ended reported a live run")
	}
}

func TestRunnerPanicFailsAndFreesTheSlot(t *testing.T) {
	logs := captureLog(t)
	info := validInfo("settle-tiers")
	panicky := scriptRepair{info: info, run: func(context.Context, Options) (Result, error) {
		panic("/music/Artist/secret.flac")
	}}
	r, _ := newTestRunner(t, panicky)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	if s := r.Snapshot(); s.State != StateFailed || s.Failure != FailurePanic || s.Counts != nil {
		t.Errorf("snapshot = %+v, want failed/panic with no counts", s)
	}
	if strings.Contains(logs.String(), "/music") {
		t.Errorf("the panic value reached the log: %s", logs)
	}
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Errorf("Start after a panic = %v; the slot was not freed", err)
	}
}

func TestRunnerFailedRunRecordsNoErrorText(t *testing.T) {
	logs := captureLog(t)
	info := validInfo("settle-tiers")
	failing := scriptRepair{info: info, run: func(context.Context, Options) (Result, error) {
		res := NewResult(info.Counts...)
		res.Add("settled", 2)
		// A spontaneous Canceled with the run's context live is a failure too.
		return res, fmt.Errorf("%w: %w", context.Canceled, &fs.PathError{Op: "open", Path: "/music/Artist/secret.flac", Err: fs.ErrPermission})
	}}
	r, _ := newTestRunner(t, failing)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	s := r.Snapshot()
	if n, _ := count(s, "settled"); s.State != StateFailed || s.Failure != FailureError || n != 2 {
		t.Errorf("snapshot = %+v, want failed/error with its counts", s)
	}
	if strings.Contains(logs.String(), "/music") || strings.Contains(fmt.Sprintf("%+v", s), "/music") {
		t.Error("the run error's text was recorded")
	}
}

func TestRunnerChecksTheResult(t *testing.T) {
	info := validInfo("settle-tiers")
	leaky := scriptRepair{info: info, run: func(context.Context, Options) (Result, error) {
		res := NewResult(info.Counts...)
		res.Add("/music/x.flac", 1)
		return res, nil
	}}
	r, _ := newTestRunner(t, leaky)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	if s := r.Snapshot(); s.State != StateFailed || s.Failure != FailureInvalidResult || s.Counts != nil {
		t.Errorf("snapshot = %+v, want failed/invalid_result with no counts", s)
	}
}

func TestRunnerSnapshotSurvivesUntilNextStart(t *testing.T) {
	rep, _, release := blocking("settle-tiers", 4, 4)
	next, started2, _ := blocking("tag-reread", 0, 0)
	r, clock := newTestRunner(t, rep, next)
	if s := r.Snapshot(); s.State != StateIdle {
		t.Fatalf("a new runner reports %s, want idle", s.State)
	}
	close(release)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	first := r.Snapshot()
	clock.advance(time.Hour)
	if again := r.Snapshot(); !reflect.DeepEqual(again, first) || first.State != StateDone {
		t.Errorf("finished snapshot changed: %+v then %+v", first, again)
	}
	if _, err := r.Start("tag-reread", Options{}); err != nil {
		t.Fatal(err)
	}
	<-started2
	if s := r.Snapshot(); s.ID != 2 || s.Key != "tag-reread" || s.State != StateRunning || s.Counts != nil {
		t.Errorf("snapshot after the next Start = %+v", s)
	}
}

func TestRunnerProgressAndRate(t *testing.T) {
	rep, started, release := blocking("settle-tiers", 50, 100)
	r, clock := newTestRunner(t, rep)
	if _, err := r.Start("settle-tiers", Options{Progress: func(int64, int64) {
		t.Error("the caller's Progress was called; the runner owns progress")
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	clock.advance(10 * time.Second)
	if s := r.Snapshot(); s.Done != 50 || s.Total != 100 || s.Rate != 5 {
		t.Errorf("running snapshot = done %d of %d at %v/s, want 50 of 100 at 5/s", s.Done, s.Total, s.Rate)
	}
	clock.advance(15 * time.Second)
	close(release)
	waitRun(t, r)
	clock.advance(time.Hour)
	if s := r.Snapshot(); s.Rate != 2 {
		t.Errorf("finished rate = %v, want 2 (50 over 25s, frozen at Finished)", s.Rate)
	}
}

func TestRunnerRefusesBeforeStarting(t *testing.T) {
	runs := 0
	info := validInfo("settle-tiers")
	info.NeedsBackup = true
	rep := scriptRepair{info: info, run: func(context.Context, Options) (Result, error) {
		runs++
		return NewResult(info.Counts...), nil
	}}
	r, _ := newTestRunner(t, rep)
	if _, err := r.Start("unknown", Options{}); !errors.Is(err, ErrUnknownRepair) {
		t.Errorf("Start(unknown) = %v, want ErrUnknownRepair", err)
	}
	if _, err := r.Start("settle-tiers", Options{Apply: true}); !errors.Is(err, ErrBackupRequired) {
		t.Errorf("Start without a backup = %v, want ErrBackupRequired", err)
	}
	waitRun(t, r)
	if s := r.Snapshot(); runs != 0 || s.State != StateIdle {
		t.Errorf("a refused Start ran (runs %d, state %s)", runs, s.State)
	}
}

func TestRunnerCloseLeavesNoGoroutine(t *testing.T) {
	baseline := runtime.NumGoroutine()
	rep, started, _ := blocking("settle-tiers", 1, 0)
	r, _ := newTestRunner(t, rep)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if s := r.Snapshot(); s.State != StateCanceled {
		t.Errorf("state after Close = %s, want canceled", s.State)
	}
	// The run's goroutine has closed its channel; let it finish returning.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("%d goroutines after Close, %d before Start", n, baseline)
	}
	if _, err := r.Start("settle-tiers", Options{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func TestRunnerCloseIsBounded(t *testing.T) {
	release := make(chan struct{})
	stubborn := scriptRepair{info: validInfo("settle-tiers"), run: func(context.Context, Options) (Result, error) {
		<-release // ignores its cancel
		return Result{}, nil
	}}
	r, _ := newTestRunner(t, stubborn)
	r.closeWait = time.Millisecond
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, ErrCloseTimeout) {
		t.Errorf("Close = %v, want ErrCloseTimeout", err)
	}
	close(release)
	waitRun(t, r)
}

// finishStore drops Progress, as a throttling durable store may, records
// Finish's Snapshot and can hold the first Finish open.
type finishStore struct {
	MemStore
	mu       sync.Mutex
	finished Snapshot
	entered  chan struct{} // closed when Finish is entered, when non-nil
	hold     chan struct{} // the held Finish waits on it
}

func (f *finishStore) Progress(int64, int64, int64) {}

func (f *finishStore) Finish(id int64, s Snapshot) {
	f.mu.Lock()
	f.finished = s
	entered, hold := f.entered, f.hold
	f.entered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
		<-hold
	}
	f.MemStore.Finish(id, s)
}

// quick is a repair that returns an empty Result at once.
func quick() scriptRepair {
	info := validInfo("settle-tiers")
	return scriptRepair{info: info, run: func(context.Context, Options) (Result, error) {
		return NewResult(info.Counts...), nil
	}}
}

// TestRunnerFinishCarriesTheLastProgress: a store that drops Progress still
// gets the final Done and Total in Finish's Snapshot.
func TestRunnerFinishCarriesTheLastProgress(t *testing.T) {
	rep, _, release := blocking("settle-tiers", 7, 9)
	close(release)
	store := &finishStore{}
	r, _ := newTestRunner(t, rep)
	r.store = store
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	if s := store.finished; s.Done != 7 || s.Total != 9 {
		t.Errorf("Finish got done %d of %d, want 7 of 9", s.Done, s.Total)
	}
}

// TestRunnerGoexitEndsFailedAndFreesTheSlot: a Run that leaves through
// runtime.Goexit must not wedge the runner.
func TestRunnerGoexitEndsFailedAndFreesTheSlot(t *testing.T) {
	ok := quick()
	calls := 0
	rep := scriptRepair{info: ok.info, run: func(ctx context.Context, o Options) (Result, error) {
		if calls++; calls == 1 {
			runtime.Goexit()
		}
		return ok.run(ctx, o)
	}}
	r, _ := newTestRunner(t, rep)
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	waitRun(t, r)
	if s := r.Snapshot(); s.State != StateFailed || s.Failure != FailureError {
		t.Errorf("after Goexit state = %s/%s, want failed/error", s.State, s.Failure)
	}
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Errorf("Start after Goexit = %v, want nil", err)
	}
}

// TestRunnerNoDoneButBusyWindow: while Finish is in flight a Start waits for
// the slot clear instead of being refused ErrBusy beside a done snapshot.
func TestRunnerNoDoneButBusyWindow(t *testing.T) {
	entered, hold := make(chan struct{}), make(chan struct{})
	r, _ := newTestRunner(t, quick())
	r.store = &finishStore{entered: entered, hold: hold}
	if _, err := r.Start("settle-tiers", Options{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	errc := make(chan error, 1)
	go func() { _, err := r.Start("settle-tiers", Options{}); errc <- err }()
	select {
	case err := <-errc:
		t.Fatalf("Start returned %v while Finish was in flight, want it to wait", err)
	case <-time.After(100 * time.Millisecond): // only the broken ordering returns early
	}
	close(hold)
	if err := <-errc; err != nil {
		t.Errorf("Start after Finish = %v, want nil", err)
	}
}
