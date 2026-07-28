package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/instrumentalbackfill"
)

// backfillTestConfig is a Config with a classifier wired, so newBackfillDetector
// reaches its construction path rather than short-circuiting on an empty URL.
func backfillTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.InstrumentalDetector.ClassifierURL = "http://127.0.0.1:9999"
	cfg.InstrumentalDetector.CooldownSeconds = 5 // the worker's interactive pacing
	cfg.InstrumentalDetector.Backfill.Enabled = true
	return cfg
}

// TestNewBackfillDetectorUsesItsOwnCooldown pins the reason the sweep does not
// share the worker's detector.
//
// HTTPDetector.Detect takes d.mu and then SLEEPS OUT its cooldown while still
// holding it (internal/detector/http.go), so two callers sharing one instance
// serialize on that mutex. A sweep cycle of 100 rows at the worker's default
// 5-second cooldown would hold the lock for over eight minutes, parking the
// worker's interactive detection behind background work. A separate instance
// with its own cooldown is what keeps the two independent -- and it is invisible
// from the outside, which is why it is asserted here rather than left to a
// comment.
func TestNewBackfillDetectorUsesItsOwnCooldown(t *testing.T) {
	cfg := backfillTestConfig(t)
	cfg.InstrumentalDetector.Backfill.CooldownSeconds = 0 // contiguous burst

	det, err := newBackfillDetector(cfg, "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	if det == nil {
		t.Fatal("detector is nil; the sweep cannot run without one")
	}

	httpDet, ok := det.(*detector.HTTPDetector)
	if !ok {
		t.Fatalf("detector is %T; want *detector.HTTPDetector", det)
	}
	if got := httpDet.CooldownSeconds(); got != 0 {
		t.Errorf("backfill detector cooldown = %ds; want 0 (its own value), not the worker's 5. "+
			"Detect() sleeps the cooldown while holding the detector mutex, so inheriting the "+
			"worker's value would both slow the sweep and block the worker behind it", got)
	}
}

// TestNewBackfillDetectorIsSeparateFromTheWorkers: the two must be DISTINCT
// instances, not the same pointer with different settings. Sharing one would
// reintroduce the mutex contention regardless of the cooldown values.
func TestNewBackfillDetectorIsSeparateFromTheWorkers(t *testing.T) {
	cfg := backfillTestConfig(t)

	workerDet, err := newAudioDetector(cfg, "ffmpeg")
	if err != nil {
		t.Fatalf("newAudioDetector: %v", err)
	}
	backfillDet, err := newBackfillDetector(cfg, "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	if workerDet == backfillDet {
		t.Error("the sweep shares the worker's detector instance; Detect() holds the detector mutex " +
			"across its cooldown sleep, so a sweep cycle would serialize the worker behind it")
	}
	// The worker's own cooldown must be untouched: newBackfillDetector copies the
	// config by value, and a pointer-ish mistake there would silently retune the
	// worker's pacing as a side effect of enabling the sweep.
	if cfg.InstrumentalDetector.CooldownSeconds != 5 {
		t.Errorf("worker cooldown mutated to %d; newBackfillDetector must not modify the caller's config",
			cfg.InstrumentalDetector.CooldownSeconds)
	}
}

// TestNewBackfillDetectorDisabledReturnsNil: an operator who turns the sweep off
// should not get a detector built for it, and serve must treat that as "do not
// start the goroutine" rather than as an error.
func TestNewBackfillDetectorDisabledReturnsNil(t *testing.T) {
	cfg := backfillTestConfig(t)
	cfg.InstrumentalDetector.Backfill.Enabled = false

	det, err := newBackfillDetector(cfg, "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	if det != nil {
		t.Error("built a detector for a disabled sweep; nothing will ever call it")
	}
}

// TestNewBackfillDetectorNoClassifierReturnsNil: with no classifier URL there is
// nothing to talk to, and the sweep must not start. A nil-with-nil-error return
// is the contract newAudioDetector already uses for this case.
func TestNewBackfillDetectorNoClassifierReturnsNil(t *testing.T) {
	cfg := backfillTestConfig(t)
	cfg.InstrumentalDetector.ClassifierURL = ""

	det, err := newBackfillDetector(cfg, "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	if det != nil {
		t.Error("built a detector with no classifier configured")
	}
}

// --- sweep loop / cycle coverage -----------------------------------------

// fakeBackfiller records each Run call's options and returns a staged result,
// so the sweep's bounds and error handling are testable without a database, a
// detector sidecar, or real audio.
type fakeBackfiller struct {
	calls []instrumentalbackfill.Options
	res   instrumentalbackfill.Result
	err   error
	// onRun, when set, fires on each call -- lets a test cancel the context from
	// inside the cycle rather than racing the loop from outside.
	onRun func()
}

func (f *fakeBackfiller) Run(_ context.Context, opts instrumentalbackfill.Options) (instrumentalbackfill.Result, error) {
	f.calls = append(f.calls, opts)
	if f.onRun != nil {
		f.onRun()
	}
	return f.res, f.err
}

// TestResolveBackfillBoundsGuards pins the two conditions that must stop the
// sweep from starting at all. Both are silent returns, so a regression here
// would spin a goroutine forever with nothing to do rather than fail loudly.
func TestResolveBackfillBoundsGuards(t *testing.T) {
	det, err := newBackfillDetector(backfillTestConfig(t), "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}

	t.Run("disabled by config", func(t *testing.T) {
		cfg := backfillTestConfig(t)
		cfg.InstrumentalDetector.Backfill.Enabled = false
		if _, ok := resolveBackfillBounds(cfg, det); ok {
			t.Error("sweep would start with backfill.enabled=false")
		}
	})

	t.Run("no detector", func(t *testing.T) {
		if _, ok := resolveBackfillBounds(backfillTestConfig(t), nil); ok {
			t.Error("sweep would start with no detector; every cycle would be a no-op")
		}
	})
}

// TestResolveBackfillBoundsFloors: a Config built in code (a test, or a future
// caller) skips config.Load's defaults, so zero values reach here. A zero batch
// classifies nothing and a zero interval PANICS time.NewTicker, which would take
// down serve mode -- these floors are the reason it does not.
func TestResolveBackfillBoundsFloors(t *testing.T) {
	det, err := newBackfillDetector(backfillTestConfig(t), "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	cfg := backfillTestConfig(t)
	cfg.InstrumentalDetector.Backfill.BatchSize = 0
	cfg.InstrumentalDetector.Backfill.IntervalMinutes = 0

	bounds, ok := resolveBackfillBounds(cfg, det)
	if !ok {
		t.Fatal("bounds rejected a config that only lacked its defaults")
	}
	if bounds.batch != defaultBackfillBatchSize {
		t.Errorf("batch = %d; want the %d floor -- a zero batch classifies nothing", bounds.batch, defaultBackfillBatchSize)
	}
	if bounds.interval != time.Duration(defaultBackfillIntervalMinutes)*time.Minute {
		t.Errorf("interval = %v; want the %d-minute floor -- time.NewTicker PANICS on a non-positive duration",
			bounds.interval, defaultBackfillIntervalMinutes)
	}
}

// TestResolveBackfillBoundsHonorsZeroCooldown: 0 is the documented default and a
// meaningful value (a contiguous burst), so it must NOT be floored the way batch
// and interval are. Flooring it would silently pace every install.
func TestResolveBackfillBoundsHonorsZeroCooldown(t *testing.T) {
	det, err := newBackfillDetector(backfillTestConfig(t), "ffmpeg")
	if err != nil {
		t.Fatalf("newBackfillDetector: %v", err)
	}
	cfg := backfillTestConfig(t)
	cfg.InstrumentalDetector.Backfill.CooldownSeconds = 0

	bounds, ok := resolveBackfillBounds(cfg, det)
	if !ok {
		t.Fatal("bounds rejected a valid config")
	}
	if bounds.cooldown != 0 {
		t.Errorf("cooldown = %d; want 0 preserved -- it is the documented default, not a missing value", bounds.cooldown)
	}
}

// TestRunBackfillCyclePassesItsBounds: the batch cap is the whole bounding
// mechanism. If Limit were dropped, one cycle would drain the entire backlog and
// hammer the library array -- the exact behavior the sweep is shaped to avoid.
func TestRunBackfillCyclePassesItsBounds(t *testing.T) {
	bf := &fakeBackfiller{res: instrumentalbackfill.Result{NotInstrumental: 3}}
	runBackfillCycle(context.Background(), bf, backfillSweepBounds{batch: 42}, true)

	if len(bf.calls) != 1 {
		t.Fatalf("Run called %d times; want 1", len(bf.calls))
	}
	if bf.calls[0].Limit != 42 {
		t.Errorf("Limit = %d; want 42. Without it a cycle drains the whole backlog in one burst", bf.calls[0].Limit)
	}
	if !bf.calls[0].GlobalDetectDefault {
		t.Error("GlobalDetectDefault not forwarded; rows with a NULL per-item decision would resolve differently " +
			"here than in the worker, so the same track could be classified two ways")
	}
	if bf.calls[0].Report != nil {
		t.Error("a Report callback was set; this sweep is continuous, so a JSONL backup would grow without bound")
	}
}

// TestRunBackfillCycleSurvivesAnError: a transient failure must not kill the
// goroutine. Serve mode runs this for the process lifetime, so a returned error
// or a panic here would silently end all future sweeps.
func TestRunBackfillCycleSurvivesAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"generic failure", errors.New("db is busy")},
		{"canceled context", context.Canceled},
		{"deadline exceeded", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bf := &fakeBackfiller{err: tc.err}
			runBackfillCycle(context.Background(), bf, backfillSweepBounds{batch: 10}, false)
			if len(bf.calls) != 1 {
				t.Fatalf("Run called %d times; want 1", len(bf.calls))
			}
		})
	}
}

// TestRunBackfillSweepLoopRunsAtStartupThenStops pins the startup cycle: a fresh
// backlog must begin draining immediately rather than waiting out the first
// interval (an hour by default). The interval here is deliberately long, so a
// second call could only come from the startup path firing twice.
func TestRunBackfillSweepLoopRunsAtStartupThenStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel from INSIDE the first cycle: the loop then hits its ctx.Done() arm on
	// the very next select, so the test neither races nor waits on the ticker.
	bf := &fakeBackfiller{onRun: cancel}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackfillSweepLoop(ctx, bf, backfillSweepBounds{batch: 5, interval: time.Hour}, true)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep loop did not return after its context was canceled; serve mode would hang on shutdown")
	}
	if len(bf.calls) != 1 {
		t.Errorf("Run called %d times; want exactly 1 (the startup cycle). A fresh backlog must not wait out the first interval", len(bf.calls))
	}
}
