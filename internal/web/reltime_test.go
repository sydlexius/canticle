package web

import (
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
)

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero is dash", time.Time{}, "-"},
		{"future skew", now.Add(5 * time.Second), "just now"},
		{"same instant", now, "just now"},
		{"59s", ago(59 * time.Second), "just now"},
		{"60s", ago(60 * time.Second), "1 min ago"},
		{"119s", ago(119 * time.Second), "1 min ago"},
		{"120s", ago(120 * time.Second), "2 min ago"},
		{"59min", ago(59 * time.Minute), "59 min ago"},
		{"60min", ago(60 * time.Minute), "1 hour ago"},
		{"119min", ago(119 * time.Minute), "1 hour ago"},
		{"2h", ago(2 * time.Hour), "2 hours ago"},
		{"23h", ago(23 * time.Hour), "23 hours ago"},
		{"24h", ago(24 * time.Hour), "1 day ago"},
		{"47h", ago(47 * time.Hour), "1 day ago"},
		{"48h", ago(48 * time.Hour), "2 days ago"},
		{"400d", ago(400 * 24 * time.Hour), "400 days ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRelativeTime(tc.t, now); got != tc.want {
				t.Errorf("formatRelativeTime = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStampRecentRelative(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	recent := []reports.RecentOutcome{
		{Title: "a", CompletedAt: now.Add(-5 * time.Minute)},
		{Title: "b"},
	}
	rows := buildRecentRows(recent, nil)
	stampRecentRelative(rows, recent, now)
	if rows[0].CompletedAtRelative != "5 min ago" {
		t.Errorf("row 0 relative = %q", rows[0].CompletedAtRelative)
	}
	if rows[0].CompletedAt != "2026-10-03 11:55 UTC" {
		t.Errorf("row 0 absolute changed: %q", rows[0].CompletedAt)
	}
	if rows[1].CompletedAtRelative != "-" || rows[1].CompletedAtISO != "" {
		t.Errorf("row 1 = %q / %q, want dash and no ISO", rows[1].CompletedAtRelative, rows[1].CompletedAtISO)
	}
}
