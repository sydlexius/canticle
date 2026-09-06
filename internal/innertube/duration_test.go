package innertube

import "testing"

// TestParseDurationSeconds pins the duration-text-run parser used by search
// candidate extraction, including its fail-open-on-unparsable contract
// (SearchCandidate.DurationSeconds documents zero as "not supplied").
func TestParseDurationSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"2:06", 126},
		{"0:09", 9},
		{"12:34", 754},
		{"Song", 0},
		{" • ", 0},
		{"", 0},
		// The edge behaviors parseDurationSeconds' doc comment calls out as
		// deliberate (854-F8), pinned so a future "fix" cannot change them
		// silently. Each row kills a distinct mutation of the pattern or the
		// arithmetic:
		{"1:02:03", 0}, // h:mm:ss does not match; fails open to "not supplied"
		{"100:00", 0},  // >= 100 minutes exceeds the two-digit minutes cap
		{"1:2", 0},     // the seconds field is fixed-width: "m:s" is not a duration
		// Seconds are range-bounded to 00-59, so an impossible seconds field is
		// REJECTED rather than combined into a confidently wrong number. A wrong
		// duration is worse than a missing one: 0 fails open, 639 gets acted on.
		{"9:99", 0},
		{"1:60", 0},   // the first value above the boundary
		{"1:59", 119}, // the boundary itself still parses
	}
	for _, tc := range cases {
		if got := parseDurationSeconds(tc.in); got != tc.want {
			t.Errorf("parseDurationSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
