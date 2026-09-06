package innertube

import (
	"regexp"
	"strconv"
	"strings"
)

// durationPattern matches the "m:ss" or "mm:ss" duration text run innertube
// includes in a search subtitle, e.g. "2:06".
var durationPattern = regexp.MustCompile(`^\d{1,2}:\d{2}$`)

// parseDurationSeconds parses a duration text run, returning 0 (meaning "not
// supplied") for anything that does not match -- see
// SearchCandidate.DurationSeconds, which must fail open on zero.
//
// Documented edge behavior (854-F8), left as-is deliberately: the pattern caps
// the minutes field at two digits, so an "h:mm:ss" run (e.g. "1:02:03") or a
// track >= 100 minutes (e.g. "100:00") does not match and returns 0 rather
// than a parsed value; an impossible seconds component like "9:99" DOES match
// and is arithmetically combined anyway (9*60+99 = 639).
//
// Note the two compose: because the seconds field is not range-checked, the
// RETURNED value can still exceed 100 minutes even though the INPUT text
// cannot ("99:99" = 6039s). The two-digit cap bounds what is matched, not what
// is returned.
//
// The safety argument rests only on what is guaranteed today: every caller
// treats 0 as "not supplied" and fails open on it (types.go), so an unparsed
// long-form duration loses a pre-filter rather than causing a wrong rejection.
// It deliberately does NOT rest on how a consumer compares durations -- there
// is no consumer yet, and types.go records that as #853's open decision. If
// #853 lands an exact comparison, revisit the "9:99" tolerance here rather
// than assuming this comment already blessed it.
func parseDurationSeconds(s string) int {
	if !durationPattern.MatchString(s) {
		return 0
	}
	parts := strings.Split(s, ":")
	minutes, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	seconds, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return minutes*60 + seconds
}
