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
		// silently. The last row pins the OTHER half of the same regex: a
		// widened seconds field would read "1:2" as 62s, which no other row
		// observes (854-R4F1):
		{"1:02:03", 0}, // h:mm:ss does not match; fails open to "not supplied"
		{"100:00", 0},  // >= 100 minutes exceeds the two-digit cap
		{"9:99", 639},  // an impossible seconds field matches and is combined
		{"1:2", 0},     // the seconds field is fixed-width: "m:s" is not a duration
	}
	for _, tc := range cases {
		if got := parseDurationSeconds(tc.in); got != tc.want {
			t.Errorf("parseDurationSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
