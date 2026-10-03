package web

import (
	"strconv"
	"time"
)

// formatRelativeTime renders when an event happened relative to now as a
// human label: "just now" (under a minute), "N min ago", "N hour(s) ago",
// "N day(s) ago", with singular forms for 1. It is display text only; a
// column that sorts must keep sorting on the underlying timestamp.
//
// now is a parameter so callers and tests control the clock. A zero time
// renders "-" (the same placeholder an unknown time shows elsewhere), and a
// time after now (clock skew) renders "just now" rather than a negative span.
func formatRelativeTime(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	n, unit := spanBucket(d)
	name := map[time.Duration]string{time.Minute: "min", time.Hour: "hour", 24 * time.Hour: "day"}[unit]
	return relUnit(n, name)
}

// spanBucket truncates a span of at least a minute to a whole count of the
// coarsest unit it reaches (minute, hour or day). It is the one bucket switch
// behind both formatWaited and formatRelativeTime.
func spanBucket(d time.Duration) (int, time.Duration) {
	switch {
	case d < time.Hour:
		return int(d / time.Minute), time.Minute
	case d < 24*time.Hour:
		return int(d / time.Hour), time.Hour
	default:
		return int(d / (24 * time.Hour)), 24 * time.Hour
	}
}

func relUnit(n int, unit string) string {
	if n != 1 && unit != "min" {
		unit += "s"
	}
	return strconv.Itoa(n) + " " + unit + " ago"
}
