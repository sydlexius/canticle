package backoff

import (
	"testing"
	"time"
)

func TestGeometricRampsAndCaps(t *testing.T) {
	base := time.Second
	max := 4 * time.Second

	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: time.Second},
		{attempts: 1, want: time.Second},
		{attempts: 2, want: 2 * time.Second},
		{attempts: 3, want: 4 * time.Second},
		{attempts: 4, want: 4 * time.Second},
		{attempts: 100, want: 4 * time.Second},
	}
	for _, tc := range tests {
		if got := Geometric(tc.attempts, base, max); got != tc.want {
			t.Fatalf("Geometric(%d, %s, %s) = %s; want %s", tc.attempts, base, max, got, tc.want)
		}
	}
}

func TestGeometricZeroBaseOrMaxReturnsZero(t *testing.T) {
	if got := Geometric(3, 0, time.Hour); got != 0 {
		t.Fatalf("Geometric with zero base = %s; want 0", got)
	}
	if got := Geometric(3, time.Minute, 0); got != 0 {
		t.Fatalf("Geometric with zero max = %s; want 0", got)
	}
	if got := Geometric(3, -time.Minute, time.Hour); got != 0 {
		t.Fatalf("Geometric with negative base = %s; want 0", got)
	}
}

func TestGeometricDefaultsCadence(t *testing.T) {
	want := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		time.Hour,
		time.Hour,
	}
	for i, w := range want {
		if got := Geometric(i+1, DefaultBase, DefaultMax); got != w {
			t.Fatalf("Geometric(%d, defaults) = %s; want %s", i+1, got, w)
		}
	}
}

// TestMissCooldownCadenceAndCap verifies the documented escalation schedule
// for benign misses: 168h (7d), 336h (14d), 672h (28d, cap), 672h, ...
func TestMissCooldownCadenceAndCap(t *testing.T) {
	tests := []struct {
		missCount int
		want      time.Duration
	}{
		{1, 168 * time.Hour},
		{2, 336 * time.Hour},
		{3, 672 * time.Hour},  // cap
		{4, 672 * time.Hour},  // still cap
		{5, 672 * time.Hour},  // still cap
		{6, 672 * time.Hour},  // still cap
		{7, 672 * time.Hour},  // still cap
		{10, 672 * time.Hour}, // still cap
	}
	for _, tc := range tests {
		got := MissCooldown(tc.missCount, DefaultMissBase, DefaultMissCap)
		if got != tc.want {
			t.Fatalf("MissCooldown(%d) = %s; want %s", tc.missCount, got, tc.want)
		}
	}
}

// TestMissCooldownZeroAttemptTreatedAsOne ensures missCount<1 maps to base.
func TestMissCooldownZeroAttemptTreatedAsOne(t *testing.T) {
	if got := MissCooldown(0, DefaultMissBase, DefaultMissCap); got != DefaultMissBase {
		t.Fatalf("MissCooldown(0) = %s; want %s (treat 0 as 1)", got, DefaultMissBase)
	}
}

// TestMissCooldownZeroBaseOrCap returns 0 (inherits Geometric behavior).
func TestMissCooldownZeroBaseOrCap(t *testing.T) {
	if got := MissCooldown(3, 0, DefaultMissCap); got != 0 {
		t.Fatalf("MissCooldown(3, 0, cap) = %s; want 0", got)
	}
	if got := MissCooldown(3, DefaultMissBase, 0); got != 0 {
		t.Fatalf("MissCooldown(3, base, 0) = %s; want 0", got)
	}
}
