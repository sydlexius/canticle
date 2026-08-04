package ffmpeg

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxCapturedOutput bounds how much subprocess output may be carried in an
// error string. The captured text ends up in work_queue.last_error and is then
// rendered into the Failure Analysis report, so the ceiling is set by what an
// operator can read on a screen, not by what a decoder can print.
//
// Sized for the real failure mode: a corrupt MP3 makes ffmpeg emit one
// error-level line per bad frame until its decode-error-rate ceiling aborts the
// run. On a live install that produced a 531,138-byte last_error in a single
// row -- 34% of every last_error byte in the database (#731). 4 KiB comfortably
// holds an ordinary multi-line ffmpeg failure whole, so the bound only engages
// on the pathological case it exists for.
const maxCapturedOutput = 4096

// elisionMarker separates the retained head from the retained tail. It is
// deliberately conspicuous: without it, a truncated capture reads as a complete
// one, and a reader would draw conclusions from text that is missing its middle.
const elisionMarker = "... [%d bytes omitted] ..."

// BoundOutput trims and size-bounds captured subprocess output so it is safe to
// interpolate into an error.
//
// It keeps the HEAD AND THE TAIL rather than a plain prefix, because for a
// failing ffmpeg run the two ends carry different and equally necessary
// information: the opening lines name the stream and decoder that failed, while
// the closing lines carry the cause that actually terminated the run (the
// "Decode error rate ... exceeds maximum" line, in the case that motivated
// this). A head-only truncation keeps the symptom and discards the diagnosis.
// The repeated middle is what the operator does not need -- it is the same line
// thousands of times over.
//
// Input at or under the cap is returned trimmed and otherwise untouched, so the
// ordinary failure path is unaffected.
//
// This bounds SIZE ONLY. It is not redaction and must not be relied on as any
// part of one: secrets are kept out of these strings at their construction
// sites (#431), and a cap would in any case be as likely to preserve a secret
// as to cut it.
func BoundOutput(out string) string {
	s := strings.TrimSpace(out)
	if len(s) <= maxCapturedOutput {
		return s
	}

	// Reserve room for the marker so the result honors the cap including it.
	marker := fmt.Sprintf(elisionMarker, len(s)-maxCapturedOutput)
	budget := maxCapturedOutput - len(marker) - 2 // two newlines around the marker
	if budget < 2 {
		// A cap this small cannot carry the marker plus meaningful text. Degrade to
		// a plain head so the function stays total rather than returning nonsense;
		// unreachable at the current constant, and guarded so a future edit that
		// lowers it fails safe instead of slicing out of range.
		return truncateRunes(s, maxCapturedOutput)
	}

	headBudget := budget / 2
	tailBudget := budget - headBudget

	head := truncateRunes(s, headBudget)
	tail := truncateRunesFromEnd(s, tailBudget)

	return head + "\n" + marker + "\n" + tail
}

// truncateRunes returns at most n bytes from the start of s, cutting on a rune
// boundary. A byte-wise cut through a multi-byte character would put invalid
// UTF-8 into the database and then into the HTML report.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateRunesFromEnd returns at most n bytes from the end of s, cutting on a
// rune boundary.
func truncateRunesFromEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
