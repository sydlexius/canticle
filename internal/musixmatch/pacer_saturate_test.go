package musixmatch

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestEffectiveIntervalScalesNormalFloor(t *testing.T) {
	floor := 10 * time.Second
	for level := 0; level <= 3; level++ {
		want := floor * time.Duration(1<<level)
		if got := effectiveInterval(floor, level); got != want {
			t.Errorf("level %d: got %v want %v", level, got, want)
		}
	}
}

func TestEffectiveIntervalSaturates(t *testing.T) {
	maxD := time.Duration(math.MaxInt64)
	if got := effectiveInterval(maxD, 0); got != maxD {
		t.Errorf("level 0: got %v want %v", got, maxD)
	}
	for level := 1; level <= 3; level++ {
		if got := effectiveInterval(maxD, level); got != maxD {
			t.Errorf("level %d: got %v want saturated max", level, got)
		}
		if got := effectiveInterval(maxD/2+1, level); got != maxD {
			t.Errorf("level %d just over half: got %v want saturated max", level, got)
		}
	}
}

func TestPacerStatsHugeFloorSaturates(t *testing.T) {
	c := newPacerTestClient(time.Duration(math.MaxInt64), time.Now, func(context.Context, time.Duration) bool { return true })
	c.OnThrottle()
	if got := c.PacerStats().EffectiveInterval; got != time.Duration(math.MaxInt64) {
		t.Fatalf("EffectiveInterval = %v; want saturated max", got)
	}
}

func TestPaceHugeFloorAtElevatedLevelStillWaits(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var slept []time.Duration
	c := newPacerTestClient(time.Duration(math.MaxInt64),
		func() time.Time { return fixed },
		func(_ context.Context, d time.Duration) bool { slept = append(slept, d); return true })
	c.OnThrottle()
	c.OnThrottle()
	ctx := context.Background()
	if err := c.pace(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.pace(ctx); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] <= 0 {
		t.Fatalf("slept = %v; want one positive wait (huge floor must not read as unpaced)", slept)
	}
}
