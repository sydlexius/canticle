package scan_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/schedule"
)

// countingResults records how many library passes have been persisted, safely
// across the goroutine Run is driven from.
type countingResults struct {
	mu    sync.Mutex
	calls int
}

func (c *countingResults) Upsert(context.Context, int64, []models.ScanResult, scan.UpsertOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

func (c *countingResults) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func schedulerWith(store scan.ResultStore, sch schedule.Schedule, now func() time.Time) *scan.Scheduler {
	return &scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner:   fakeScanner{results: []models.ScanResult{{FilePath: "/music/a.mp3"}}},
		Schedule:  sch,
		Now:       now,
	}
}

// TestScheduler_DailyScheduleDoesNotScanAtStartup is the issue's headline
// acceptance criterion (#726): a scheduler configured "daily at 04:00" with
// scan_on_start off must perform NO library walk when the process starts. The
// old code walked unconditionally, which under a nightly restart was the only
// scan that ever happened.
func TestScheduler_DailyScheduleDoesNotScanAtStartup(t *testing.T) {
	store := &countingResults{}
	now := time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC)
	s := schedulerWith(store, schedule.Schedule{
		Frequency:   schedule.FrequencyDaily,
		At:          schedule.TimeOfDay{Hour: 4},
		ScanOnStart: false,
	}, func() time.Time { return now })

	// Cancel immediately: Run must reach the wait for the next fire without
	// having scanned. A startup walk would already have been recorded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v; want context.Canceled", err)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("library passes at startup = %d; want 0 (scan_on_start is off)", got)
	}
}

// TestScheduler_ScanOnStartOptsBackIn is the positive control for the test
// above: the same schedule with the flag on DOES walk once at startup, so a
// zero count there proves the flag and not a broken scan path.
func TestScheduler_ScanOnStartOptsBackIn(t *testing.T) {
	store := &countingResults{}
	now := time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC)
	s := schedulerWith(store, schedule.Schedule{
		Frequency:   schedule.FrequencyDaily,
		At:          schedule.TimeOfDay{Hour: 4},
		ScanOnStart: true,
	}, func() time.Time { return now })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, func() bool { return store.count() == 1 }, "the startup walk")
	cancel()
	<-done
	if got := store.count(); got != 1 {
		t.Fatalf("library passes = %d; want exactly 1 (the next fire is hours away)", got)
	}
}

// TestScheduler_WaitsForTheWallClockNotProcessStart proves the wait is derived
// from the CALENDAR: with the injected clock parked just before 04:00 the
// scheduler fires almost immediately, and with it parked just after, it does
// not fire at all within the same window. Under the old ticker both cases
// waited the same full interval from process start.
func TestScheduler_WaitsForTheWallClockNotProcessStart(t *testing.T) {
	sch := schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 4}}

	t.Run("just before the anchor fires promptly", func(t *testing.T) {
		store := &countingResults{}
		now := time.Date(2026, time.March, 3, 3, 59, 59, 950*int(time.Millisecond), time.UTC)
		s := schedulerWith(store, sch, func() time.Time { return now })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		waitFor(t, func() bool { return store.count() >= 1 }, "the 04:00 scan")
		cancel()
		<-done
	})

	t.Run("just after the anchor waits for tomorrow", func(t *testing.T) {
		store := &countingResults{}
		now := time.Date(2026, time.March, 3, 4, 0, 1, 0, time.UTC)
		s := schedulerWith(store, sch, func() time.Time { return now })
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if err := s.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error = %v; want context.DeadlineExceeded", err)
		}
		if got := store.count(); got != 0 {
			t.Fatalf("library passes = %d; want 0 (the missed window is skipped, not run late)", got)
		}
	})
}

// TestScheduler_OffStopsAfterTheOptionalStartupWalk confirms "off" ends the
// loop rather than spinning: it returns nil, and it returns nil promptly.
func TestScheduler_OffStopsAfterTheOptionalStartupWalk(t *testing.T) {
	store := &countingResults{}
	s := schedulerWith(store, schedule.Schedule{Frequency: schedule.FrequencyOff, ScanOnStart: true}, nil)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.count(); got != 1 {
		t.Fatalf("library passes = %d; want 1 (the startup walk only)", got)
	}
}

// TestScheduler_LegacyIntervalStillWalksAtStartup is the migration guarantee: a
// Scheduler built the old way (Interval set, no Schedule) keeps its historical
// behavior, so upgrading does not silently stop scanning for anyone.
func TestScheduler_LegacyIntervalStillWalksAtStartup(t *testing.T) {
	store := &countingResults{}
	s := &scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner:   fakeScanner{results: []models.ScanResult{{FilePath: "/music/a.mp3"}}},
		Interval:  time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, func() bool { return store.count() == 1 }, "the legacy startup walk")
	cancel()
	<-done
}

// TestScheduler_LegacyIntervalRepeats confirms the deprecated mode still
// repeats on its duration, so keeping the key alive for a release is a real
// promise rather than a startup-walk-only stub.
func TestScheduler_LegacyIntervalRepeats(t *testing.T) {
	store := &countingResults{}
	s := &scan.Scheduler{
		Libraries: fakeLibraries{libs: []models.Library{{ID: 7, Path: "/music", Name: "Music"}}},
		Results:   store,
		Scanner:   fakeScanner{results: []models.ScanResult{{FilePath: "/music/a.mp3"}}},
		Interval:  5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, func() bool { return store.count() >= 3 }, "three interval-mode passes")
	cancel()
	<-done
}

// waitFor polls cond until it holds or the test's patience runs out. Polling a
// counter beats sleeping a fixed duration: it fails with what was being waited
// for rather than flaking on a slow machine.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
