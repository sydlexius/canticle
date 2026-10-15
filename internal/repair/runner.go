package repair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// defaultCloseWait bounds Close's wait for a run that is ending.
const defaultCloseWait = 5 * time.Second

var (
	// ErrUnknownRepair reports a Start for a key the registry does not hold.
	ErrUnknownRepair = errors.New("repair: unknown repair")
	// ErrClosed reports a Start after Close.
	ErrClosed = errors.New("repair: the runner is closed")
	// ErrCloseTimeout reports that Close stopped waiting for a run that did
	// not return after its cancel.
	ErrCloseTimeout = errors.New("repair: a run ignored its cancel past the close wait")
)

// Runner runs at most one repair at a time, off the caller's goroutine, and
// reports it through Snapshot. Its owner (serve) builds it and injects it; it
// starts a run only when Start is called, never on its own.
type Runner struct {
	reg   *Registry
	store RunStore
	// now and closeWait are fields so a test can fake the clock and shrink the wait.
	now       func() time.Time
	closeWait time.Duration

	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc // the live run's; nil when none
	live   chan struct{}      // closed when the live run's goroutine returns
	// liveKey and liveID name the live run, for CancelKey.
	liveKey Key
	liveID  int64
	// last is the latest run's live channel, kept after the slot clears so
	// Close also waits out a run that is ending on its own (its final log).
	last chan struct{}
}

// NewRunner returns a Runner over reg's repairs. A nil store is a new MemStore.
func NewRunner(reg *Registry, store RunStore) *Runner {
	if store == nil {
		store = &MemStore{}
	}
	return &Runner{reg: reg, store: store, now: time.Now, closeWait: defaultCloseWait}
}

// Start starts the repair registered under key with o and returns the run's
// id. The caller resolves o.Backup for this run (the CLI writes a timestamped
// backup per run); o.Progress is replaced by the runner's own, which feeds
// Snapshot. It refuses with ErrClosed, ErrUnknownRepair, a CheckOptions error,
// or ErrBusy while a run is live.
func (r *Runner) Start(key Key, o Options) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	rep, ok := r.reg.Get(key)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnknownRepair, key)
	}
	info := rep.Info()
	if err := CheckOptions(info, o); err != nil {
		return 0, err
	}
	if r.live != nil {
		return 0, ErrBusy
	}
	started := r.now()
	id, err := r.store.Begin(Snapshot{Key: info.Key, Apply: o.Apply, Started: started})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	live := make(chan struct{})
	r.cancel, r.live, r.liveKey, r.liveID, r.last = cancel, live, info.Key, id, live
	go r.run(ctx, cancel, live, rep, info, o, id, started)
	return id, nil
}

// run runs one repair and records its outcome. The error's text and a panic's
// value are never recorded or logged: either may hold a library path.
func (r *Runner) run(ctx context.Context, cancel context.CancelFunc, live chan struct{}, rep Repair, info Info, o Options, id int64, started time.Time) {
	defer close(live) // last: Close waits for the whole goroutine
	defer cancel()
	var done, total atomic.Int64
	o.Progress = func(d, t int64) {
		done.Store(d)
		total.Store(t)
		r.store.Progress(id, d, t)
	}
	// Preset to failed so a Run that never returns normally (runtime.Goexit)
	// still ends the run and frees the slot. Finish and the slot clear happen
	// together under r.mu, so Snapshot never says done while Start says busy.
	s := Snapshot{Key: info.Key, Apply: o.Apply, Started: started, State: StateFailed, Failure: FailureError}
	defer func() {
		s.Finished, s.Done, s.Total = r.now(), done.Load(), total.Load()
		r.mu.Lock()
		r.store.Finish(id, s)
		if r.live == live {
			r.cancel, r.live, r.liveKey, r.liveID = nil, nil, "", 0
		}
		r.mu.Unlock()
		level := slog.LevelInfo
		if s.Failure != FailureNone {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, "repair: run finished", "repair", string(info.Key), "state", string(s.State),
			"failure", string(s.Failure), "duration_ms", s.Finished.Sub(started).Milliseconds())
	}()
	res, panicked, err := runRecovered(ctx, rep, o)
	switch {
	case panicked:
		s.State, s.Failure = StateFailed, FailurePanic
	case res.Check(info) != nil:
		s.State, s.Failure = StateFailed, FailureInvalidResult
	case err == nil:
		s.State, s.Failure, s.Counts = StateDone, FailureNone, res.Counts()
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		s.State, s.Failure, s.Counts = StateCanceled, FailureNone, res.Counts()
	default:
		s.State, s.Failure, s.Counts = StateFailed, FailureError, res.Counts()
	}
}

// runRecovered calls rep.Run, turning a panic into panicked.
func runRecovered(ctx context.Context, rep Repair, o Options) (res Result, panicked bool, err error) {
	defer func() {
		if recover() != nil {
			res, panicked, err = Result{}, true, nil
		}
	}()
	res, err = rep.Run(ctx, o)
	return res, false, err
}

// Snapshot returns the current or last run, with its Rate. A finished run's
// snapshot stays until the next Start.
func (r *Runner) Snapshot() Snapshot {
	s := r.store.Load()
	end := s.Finished
	if end.IsZero() {
		end = r.now()
	}
	if secs := end.Sub(s.Started).Seconds(); s.Done > 0 && !s.Started.IsZero() && secs > 0 {
		s.Rate = float64(s.Done) / secs
	}
	return s
}

// Cancel cancels the live run and reports whether there was one. The run
// stops at its next row boundary; Snapshot reports it running until Run
// returns, then canceled with its partial counts.
func (r *Runner) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel == nil {
		return false
	}
	r.cancel()
	return true
}

// CancelKey cancels the live run only when it is a run of key, and returns
// its id. The key check and the cancel happen under one lock, so a run of
// another repair that starts in between is never the one stopped.
func (r *Runner) CancelKey(key Key) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel == nil || r.liveKey != key {
		return 0, false
	}
	r.cancel()
	return r.liveID, true
}

// Close refuses later starts, cancels the live run and waits for the latest
// run's goroutine (live, or ending on its own), at most closeWait. It returns ErrCloseTimeout when the run has
// not returned by then. Safe to call more than once.
func (r *Runner) Close() error {
	r.mu.Lock()
	r.closed = true
	live := r.last
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	if live == nil {
		return nil
	}
	t := time.NewTimer(r.closeWait)
	defer t.Stop()
	select {
	case <-live:
		return nil
	case <-t.C:
		slog.Warn("repair: shutdown stopped waiting for a run that ignored its cancel")
		return ErrCloseTimeout
	}
}
