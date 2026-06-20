package web

import (
	"testing"
	"time"
)

func TestFormatReportTime(t *testing.T) {
	// Fixed UTC input: 2026-06-20 23:30:00 UTC.
	in := time.Date(2026, 6, 20, 23, 30, 0, 0, time.UTC)

	t.Run("zero value renders dash", func(t *testing.T) {
		if got := formatReportTime(time.Time{}, nil); got != "-" {
			t.Fatalf("zero value: got %q, want %q", got, "-")
		}
		// A non-nil loc must not change the zero-value guard.
		if got := formatReportTime(time.Time{}, time.UTC); got != "-" {
			t.Fatalf("zero value with loc: got %q, want %q", got, "-")
		}
	})

	t.Run("nil loc keeps the zone the timestamp carries", func(t *testing.T) {
		want := "2026-06-20 23:30:00 UTC"
		if got := formatReportTime(in, nil); got != want {
			t.Fatalf("nil loc: got %q, want %q", got, want)
		}
	})

	t.Run("nil loc normalizes a non-UTC-carrying timestamp to UTC", func(t *testing.T) {
		// Same instant as `in`, but carrying a fixed +05:00 zone (wall clock
		// 2026-06-21 04:30:00). With a nil loc this must render the UTC
		// wall-clock with a UTC label, not the +05:00 wall clock.
		shifted := in.In(time.FixedZone("X", 5*3600))
		want := "2026-06-20 23:30:00 UTC"
		if got := formatReportTime(shifted, nil); got != want {
			t.Fatalf("nil loc with non-UTC input: got %q, want %q", got, want)
		}
	})

	t.Run("explicit non-UTC loc converts the timestamp", func(t *testing.T) {
		// A fixed -7h zone stands in for PDT so the case needs no IANA tzdata
		// file (minimal containers/CI may ship without it). 23:30 UTC on
		// 2026-06-20 shifts to the 16:30 wall clock with the PDT label.
		loc := time.FixedZone("PDT", -7*3600)
		want := "2026-06-20 16:30:00 PDT"
		if got := formatReportTime(in, loc); got != want {
			t.Fatalf("non-UTC loc: got %q, want %q", got, want)
		}
	})
}
