package innertube

import (
	"regexp"
	"strconv"
	"strings"
)

// durationPattern matches the "m:ss" or "mm:ss" duration text run innertube
// includes in a search subtitle, e.g. "2:06".
var durationPattern = regexp.MustCompile(`^\d{1,2}:[0-5]\d$`)

// parseDurationSeconds parses a duration text run, returning 0 (meaning "not
// supplied") for anything that does not match -- see
// SearchCandidate.DurationSeconds, which must fail open on zero.
//
// Documented edge behavior (854-F8): the pattern caps the minutes field at two
// digits, so an "h:mm:ss" run (e.g. "1:02:03") or a track >= 100 minutes
// (e.g. "100:00") does not match and returns 0 rather than a parsed value.
//
// Both fields are range-bounded, so a returned value never contradicts the
// pattern: seconds are restricted to 00-59, which makes 99:59 (5999s) the
// maximum. An impossible seconds component like "9:99" is therefore rejected
// outright rather than combined into a confidently wrong 639. That matters
// more than it looks: a rejected duration is 0, which every caller reads as
// "not supplied" and fails open on (types.go), whereas a wrong NUMBER would
// be acted on as if it were real.
func parseDurationSeconds(s string) int {
	if !durationPattern.MatchString(s) {
		return 0
	}
	// Both Atoi calls are infallible here rather than merely unlikely to fail:
	// durationPattern has already matched, so each part is one or two ASCII
	// digits (Go's \d is ASCII-only) and cannot overflow. Handling an error
	// that cannot occur would add a branch no test can reach, so the errors are
	// discarded deliberately -- if the pattern is ever widened, restore the
	// checks along with it.
	parts := strings.Split(s, ":")
	minutes, _ := strconv.Atoi(parts[0])
	seconds, _ := strconv.Atoi(parts[1])
	return minutes*60 + seconds
}
