package musixmatch

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPacerStatsThrottleAndRecovery(t *testing.T) {
	cur := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	c := newPacerTestClient(10*time.Second, nowFn, func(context.Context, time.Duration) bool { return true })

	s := c.PacerStats()
	if s.Level != 0 || s.EffectiveInterval != 10*time.Second || !s.LastThrottle.IsZero() {
		t.Fatalf("initial stats = %+v", s)
	}

	c.OnThrottle()
	s = c.PacerStats()
	if s.Level != 1 || s.EffectiveInterval != 20*time.Second || !s.LastThrottle.Equal(cur) {
		t.Fatalf("after one throttle = %+v", s)
	}

	throttledAt := cur
	cur = cur.Add(time.Minute)
	c.OnThrottle()
	s = c.PacerStats()
	if s.Level != 2 || s.EffectiveInterval != 40*time.Second || !s.LastThrottle.Equal(cur) {
		t.Fatalf("after second throttle = %+v", s)
	}

	// Step down via the success streak: level falls but LastThrottle stays.
	lastThrottle := cur
	cur = cur.Add(time.Minute)
	for i := 0; i < adaptiveSuccessThreshold; i++ {
		c.OnSuccess()
	}
	s = c.PacerStats()
	if s.Level != 1 || s.EffectiveInterval != 20*time.Second {
		t.Fatalf("after success streak = %+v", s)
	}
	if !s.LastThrottle.Equal(lastThrottle) || s.LastThrottle.Equal(throttledAt) {
		t.Fatalf("LastThrottle moved on step-down: %v", s.LastThrottle)
	}
}

func TestPacerStatsUnpacedHasZeroInterval(t *testing.T) {
	c := NewClient("tok")
	c.OnThrottle()
	s := c.PacerStats()
	if s.Level != 1 || s.EffectiveInterval != 0 {
		t.Fatalf("unpaced stats = %+v", s)
	}
}

func TestPacerStatsConcurrentReaders(t *testing.T) {
	c := NewClient("tok").WithMinInterval(time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.OnThrottle()
				c.OnSuccess()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := c.PacerStats()
				if s.Level < 0 || s.Level > adaptiveMaxLevel {
					t.Errorf("level out of range: %d", s.Level)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPacerStatsReportsDecayWhenIdle(t *testing.T) {
	cur := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	c := newPacerTestClient(15*time.Second, nowFn, func(context.Context, time.Duration) bool { return true })
	for i := 0; i < 3; i++ {
		c.OnThrottle()
	}
	throttledAt := cur
	if s := c.PacerStats(); s.Level != 3 || s.EffectiveInterval != 2*time.Minute {
		t.Fatalf("before decay = %+v", s)
	}

	cur = cur.Add(2 * time.Hour)
	s := c.PacerStats()
	if s.Level != 0 || s.EffectiveInterval != 15*time.Second || !s.LastThrottle.Equal(throttledAt) {
		t.Fatalf("idle decayed stats = %+v", s)
	}
	// Read-only: asking again, and the raw state, are unchanged.
	if c.adaptiveLevel != 3 {
		t.Fatalf("PacerStats mutated adaptiveLevel to %d", c.adaptiveLevel)
	}
	// A real pace() agrees with the snapshot.
	if err := c.pace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s2 := c.PacerStats(); s2.Level != s.Level || s2.EffectiveInterval != s.EffectiveInterval {
		t.Fatalf("after pace = %+v, snapshot was %+v", s2, s)
	}
	if c.adaptiveLevel != 0 {
		t.Fatalf("pace left level %d", c.adaptiveLevel)
	}
}

func TestPacerStatsThrottleAtMaxLevelStampsLastThrottle(t *testing.T) {
	cur := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return cur }
	c := newPacerTestClient(10*time.Second, nowFn, func(context.Context, time.Duration) bool { return true })
	for i := 0; i < adaptiveMaxLevel+2; i++ {
		c.OnThrottle()
	}
	cur = cur.Add(time.Minute)
	c.OnThrottle()
	s := c.PacerStats()
	if s.Level != adaptiveMaxLevel || !s.LastThrottle.Equal(cur) {
		t.Fatalf("throttle at max = %+v, want level %d last %v", s, adaptiveMaxLevel, cur)
	}
}
