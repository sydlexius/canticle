package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Close cancels a running conversion and returns only once it has stopped:
// no converter outlives Close, its dir is gone, and Get then fails.
func TestFlacCacheCloseStopsConversion(t *testing.T) {
	started, stopped, never := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c, err := newFlacCache(t.TempDir(), 1<<20, func(ctx context.Context, _ *os.File, _, out string) error {
		close(started)
		select {
		case <-ctx.Done():
			close(stopped)
			return ctx.Err()
		case <-never: // a converter Close neither cancels nor waits for keeps running
			return os.WriteFile(out, fakeFlac, 0o600)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close(never)
	p, open, fi := srcNamed(t, t.TempDir(), "s.m4a")
	done := getAsync(c, context.Background(), p, open, fi)
	<-started
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: it waits on a conversion it never canceled")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Close returned while the conversion was still running")
	}
	if _, err := os.Stat(c.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close left the cache dir: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("a Get canceled by Close succeeded")
	}
	if _, err := c.Get(context.Background(), p, fi, open); !errors.Is(err, errFlacCacheClosed) {
		t.Fatalf("Get after Close: err = %v, want errFlacCacheClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A cache hit whose open lands just as Close begins returns no handle: Get
// rechecks closed after the open, so it never hands out a file from a cache
// Close is tearing down.
func TestFlacCacheHitRacingCloseReturnsClosed(t *testing.T) {
	c, err := newFlacCache(t.TempDir(), 1<<20, (&countingConverter{}).convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := srcNamed(t, t.TempDir(), "s.m4a")
	if err := <-getAsync(c, context.Background(), p, open, fi); err != nil {
		t.Fatal(err) // now cached: the next Get takes the hit path
	}
	prev := flacHitOpened
	t.Cleanup(func() { flacHitOpened = prev })
	hits := 0
	flacHitOpened = func() {
		hits++
		// Close's dir removal can fail on Windows while the hit is open; only
		// that Close has begun matters here.
		_ = c.Close()
	}
	f, err := c.Get(context.Background(), p, fi, open)
	if f != nil {
		_ = f.Close()
	}
	if hits != 1 {
		t.Fatalf("the hit path ran %d times, want 1 (the test did not exercise it)", hits)
	}
	if f != nil || !errors.Is(err, errFlacCacheClosed) {
		t.Fatalf("Get racing Close = %v, %v: want no handle and errFlacCacheClosed", f, err)
	}
}

// Close as a conversion finishes (it ignores the cancel, as a conversion
// already writing its last bytes does) still leaves nothing behind.
func TestFlacCacheCloseRacesFinishingConversion(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		parent := t.TempDir()
		gate, started := make(chan struct{}), make(chan struct{})
		c, err := newFlacCache(parent, 1<<20, func(_ context.Context, _ *os.File, _, out string) error {
			close(started)
			<-gate
			time.Sleep(time.Duration(iter%5) * 100 * time.Microsecond)
			return os.WriteFile(out, fakeFlac, 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
		p, open, fi := srcNamed(t, t.TempDir(), "s.m4a")
		done := getAsync(c, context.Background(), p, open, fi)
		<-started
		close(gate)
		if err := c.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", iter, err)
		}
		if ents, _ := os.ReadDir(parent); len(ents) != 0 {
			t.Fatalf("iteration %d: Close left %d entries under the parent", iter, len(ents))
		}
		<-done
	}
}

func requireFlock(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no liveness lock and no sweep on " + runtime.GOOS)
	}
}

// A new cache removes a sibling dir whose lock is free (its process died
// without Close), and keeps one whose lock is held, one with no lock yet, and
// anything not named like a cache dir.
func TestFlacCacheSweepsOnlyDeadSiblings(t *testing.T) {
	requireFlock(t)
	parent := t.TempDir()
	live, err := newFlacCache(parent, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	dead := filepath.Join(parent, flacDirPrefix+"dead")
	noLock := filepath.Join(parent, flacDirPrefix+"creating")
	other := filepath.Join(parent, "unrelated")
	for _, d := range []string{dead, noLock, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "x.flac"), fakeFlac, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{dead, other} {
		if err := os.WriteFile(filepath.Join(d, flacLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := newFlacCache(parent, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for d, want := range map[string]bool{dead: false, noLock: true, other: true, live.dir: true, c.dir: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

// A sibling whose owner holds its lock survives a new cache with a conversion
// in flight; its lock is gone only once it closes, and the next cache then
// finds nothing to sweep.
func TestFlacCacheKeepsLiveSibling(t *testing.T) {
	requireFlock(t)
	parent := t.TempDir()
	gate := make(chan struct{})
	conv := &countingConverter{gate: gate}
	first, err := newFlacCache(parent, 1<<20, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := srcNamed(t, t.TempDir(), "s.m4a")
	done := getAsync(first, context.Background(), p, open, fi)
	waitFor(t, "the conversion to start", func() bool { return conv.calls.Load() == 1 })
	second, err := newFlacCache(parent, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("live sibling's Get after a new cache started: %v", err)
	}
	if flacDirDead(first.dir) {
		t.Fatal("a live cache's dir reads as dead")
	}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("Close left %d entries under the parent", len(ents))
	}
}

// A queued job (both slots busy) whose only waiter leaves exits at once and
// never converts; and a job's timeout starts when it gets a slot, not while
// it waits for one.
func TestFlacCacheQueuedJobLeavesAndTimesFromItsSlot(t *testing.T) {
	gate := make(chan struct{})
	var deadline time.Time
	conv := &countingConverter{gate: gate}
	c := newTestCache(t, 1<<20, func(ctx context.Context, in *os.File, name, out string) error {
		if filepath.Base(name) == "late.m4a" {
			deadline, _ = ctx.Deadline()
		}
		return conv.convert(ctx, in, name, out)
	})
	src := t.TempDir()
	var errs []chan error
	for _, name := range []string{"a.m4a", "b.m4a"} {
		p, open, fi := srcNamed(t, src, name)
		errs = append(errs, getAsync(c, context.Background(), p, open, fi))
	}
	waitFor(t, "both slots to fill", func() bool { return conv.calls.Load() == flacMaxConversions })

	q, qopen, qfi := srcNamed(t, src, "q.m4a")
	ctx, cancel := context.WithCancel(context.Background())
	left := getAsync(c, ctx, q, qopen, qfi)
	waitFor(t, "the queued request", func() bool { return c.waiters(flacKey(q, qfi)) == 1 })
	c.mu.Lock()
	queued := c.inflight[flacKey(q, qfi)]
	c.mu.Unlock()
	late, lopen, lfi := srcNamed(t, src, "late.m4a")
	errs = append(errs, getAsync(c, context.Background(), late, lopen, lfi))
	waitFor(t, "the late request", func() bool { return c.waiters(flacKey(late, lfi)) == 1 })

	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaver err = %v, want context canceled", err)
	}
	select {
	case <-queued.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a queued job whose waiter left still holds its goroutine while the slots are busy")
	}
	released := time.Now()
	close(gate)
	for _, ch := range errs {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if n := conv.calls.Load(); n != 3 {
		t.Fatalf("conversions = %d, want 3 (the abandoned queued job must not convert)", n)
	}
	if deadline.Before(released.Add(flacConvertTimeout)) {
		t.Fatalf("timeout counted from before the slot: deadline %v is earlier than release + %v", deadline, flacConvertTimeout)
	}
}

// A job canceled before it gets a slot never converts, even when a slot is
// free and select may pick it over the cancel.
func TestFlacCacheCanceledJobNeverConverts(t *testing.T) {
	conv := &countingConverter{}
	c := newTestCache(t, 1<<20, conv.convert)
	p, open, _ := srcNamed(t, t.TempDir(), "s.m4a")
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		job := &flacJob{done: make(chan struct{}), cancel: cancel}
		c.runs.Add(1)
		c.run(ctx, "k.flac", job, open, p, filepath.Join(c.dir, "k.flac"))
		if !errors.Is(job.err, context.Canceled) || conv.calls.Load() != 0 {
			t.Fatalf("iteration %d: err = %v, conversions = %d, want canceled and none", i, job.err, conv.calls.Load())
		}
	}
}

// A cache whose lock cannot be taken removes the dir it created, since no
// sweep would reclaim a fresh lockless dir.
func TestFlacCacheLockFailureRemovesDir(t *testing.T) {
	old := flacLockDir
	flacLockDir = func(string) (*os.File, error) { return nil, errors.New("no lock") }
	t.Cleanup(func() { flacLockDir = old })
	parent := t.TempDir()
	if _, err := newFlacCache(parent, 1<<20, nil); err == nil {
		t.Fatal("newFlacCache succeeded without its lock")
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("a failed lock left %d entries under the parent", len(ents))
	}
}
