package commands

import (
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/detector"
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
