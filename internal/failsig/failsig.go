// Package failsig normalizes a work_queue.last_error string into a stable
// grouping key: one root cause yields one signature, regardless of which item,
// file, or ephemeral port happened to produce it.
//
// It exists for two reasons that turn out to be the same fix (#478).
//
// GROUPING. The Failure Analysis report and the /metrics failure counter both
// GROUP BY the raw last_error. That already works well for benign misses, whose
// text is fixed -- 20,124 deferred rows collapse to 13 groups on the reference
// install. It barely works for hard failures, whose text embeds per-item
// variable data: 14 failed rows produced 10 "categories", so five write failures
// with a single root cause read as five separate problems.
//
// PRIVACY. The same variable text is private metadata. work_queue.last_error is
// grouped by queue.CountFailuresByReason and emitted VERBATIM as the
// mxlrcgo_queue_failures{reason="..."} Prometheus label
// (internal/server/metrics.go), which an external scraper stores and retains
// off-host. Five of the 14 failed signatures carried absolute library paths onto
// that surface. That is the same exposure removed from the detector's error in
// #731 and from a report in #431, reached by a third route.
//
// Normalizing the key fixes both at once, and drops label cardinality as a side
// effect -- the label previously earned a distinct time series per failing file.
//
// WHAT IT DOES NOT DO. This is not redaction and must not be relied on as any
// part of one: secrets are kept out of these strings at their construction
// sites. It removes text that is VARIABLE, which is a different property from
// text that is SECRET, and a value that is neither stays untouched.
package failsig

import (
	"regexp"
	"strings"
)

// The order of these patterns matters where they can overlap; each is applied in
// sequence by Normalize.
//
// Every placeholder is a fixed token, never a hash or an index, so the result is
// deterministic and idempotent: normalizing a normalized string is a no-op
// because the placeholders themselves match nothing here.
var replacements = []struct {
	re   *regexp.Regexp
	with string
}{
	// A quoted absolute path, e.g. output dir "/Share/Music/...". Handled before
	// the bare-path rule so the quotes are consumed with it rather than left as
	// an empty pair.
	{regexp.MustCompile(`"(?:/[^"\n]*)"`), `"<path>"`},
	// An ephemeral host:port inside a dial/read address, e.g. 127.0.0.1:56723.
	// Before the bare-IP rule, which would otherwise leave the port stranded.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}:\d+`), `<addr>`},
	// A bare IPv4 address, e.g. a container IP that changes on every restart.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), `<ip>`},
	// A hex memory address, e.g. ffmpeg's "[mp3float @ 0x14e1cf868a80]". The
	// allocator hands out a different address every run, so two occurrences of
	// ONE ffmpeg failure never grouped together -- verified against the real
	// signature before adding this. Same class as the ports and IPs above:
	// variable per run, carrying no diagnostic value.
	{regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`), `<addr>`},
	// A work_queue row id, e.g. "write item 77508 output". Anchored to the word
	// "item" so an ordinary number elsewhere in a message is not eaten -- notably
	// NOT a status code, which is the actionable signal and must survive.
	{regexp.MustCompile(`\bitem \d+\b`), `item <id>`},
	// A bare absolute path. Last, so the quoted and address forms above have
	// already claimed their text.
	//
	// IT MUST NOT STOP AT WHITESPACE. Library paths routinely contain spaces
	// ("/Share/Music/Some Artist/Album One/07. Track.lrc"), and a \S-based rule
	// shreds one path into fragments -- leaving "(27)" and "Album" and the
	// filename behind as residue that still varies per row, so the grouping does
	// not collapse AND the leaked text is only partly removed. Measured on the
	// real signatures: three write failures still produced three groups.
	//
	// These messages delimit a path with ": " (colon-SPACE) or end-of-line, so
	// that is the terminator. RE2 has no lookahead, so the delimiter is captured
	// and restored rather than peeked at.
	{regexp.MustCompile(`/[^"\n]*?(: |\n|$)`), `<path>${1}`},
}

// Normalize returns a stable grouping key for one last_error value.
//
// An empty or whitespace-only input returns empty: the SQL layer already maps
// that to "unknown", and minting a second empty bucket here would split one
// group in two.
//
// A signature carrying no variable text is returned unchanged, so the
// already-well-grouped benign-miss population is untouched.
func Normalize(s string) string {
	out := strings.TrimSpace(s)
	if out == "" {
		return ""
	}
	for _, r := range replacements {
		out = r.re.ReplaceAllString(out, r.with)
	}
	return out
}
