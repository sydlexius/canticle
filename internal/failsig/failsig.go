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
	// A bracketed IPv6 endpoint, e.g. [2001:db8::1]:8080. First, because the
	// brackets must be consumed with the address rather than left stranded.
	{regexp.MustCompile(`\[[0-9a-fA-F:]+\]:\d+`), `<addr>`},
	// A bare IPv6 address, in two alternatives -- the "::"-compressed form first
	// so it wins on a compressed address, then the full uncompressed 8-group form.
	//
	// DELIBERATELY NARROW, because a loose colon-group pattern eats CLOCK TIMES:
	// "12:30:45" and "1:02:03" are hex-group-shaped, and collapsing them to <ip>
	// would merge distinct timings. Requiring either a literal "::" or all eight
	// groups excludes a 3-field time by construction. A first draft that merely
	// required 2+ groups both ate clock times AND stranded a digit
	// ("2001:db8::1" -> "<ip>1"), which is why this is probed rather than assumed.
	{regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){1,7}:(?:[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4})*)?|\b[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4}){7}\b`), `<ip>`},
	// An ephemeral IPv4 host:port inside a dial/read address, e.g. 127.0.0.1:56723.
	// Before the bare-IP rule, which would otherwise leave the port stranded.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}:\d+`), `<addr>`},
	// A bare IPv4 address, e.g. a container IP that changes on every restart.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), `<ip>`},
	// A hex memory address in ALLOCATOR SYNTAX ONLY, e.g. ffmpeg's
	// "[mp3float @ 0x14e1cf868a80]". The allocator hands out a different address
	// every run, so two occurrences of ONE ffmpeg failure never grouped together.
	//
	// ANCHORED TO "@ ", DELIBERATELY. A bare \b0x[0-9a-fA-F]+\b also matches a hex
	// STATUS code -- "exit status 0xC0000005" (access violation) and
	// "exit status 0xC0000409" (stack buffer overrun) are different failures that
	// collapsed into one group. That is the over-normalization this package is
	// supposed to prevent, so the rule is narrowed to the one syntax where a hex
	// token is provably an address rather than a code.
	{regexp.MustCompile(`@ 0x[0-9a-fA-F]+`), `@ <addr>`},
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
	// IT MUST ALSO NOT RUN TO END-OF-LINE. An earlier version terminated on
	// ": " OR end-of-line, and the end-of-line branch swallowed the diagnostic
	// suffix: "read /mnt/a permission denied" and "read /mnt/a checksum mismatch"
	// both became "read <path>", merging two distinct root causes. Erasing the
	// cause is strictly worse than leaking the path, because the report then
	// shows one group that means nothing.
	//
	// So the ONLY terminator is ": " (colon-SPACE), the delimiter these messages
	// actually use, plus a newline. A path that ends a line without a delimiter is
	// left alone rather than guessed at -- see pathToEOL below, which handles the
	// one shape where end-of-line is provably safe. RE2 has no lookahead, so the
	// delimiter is captured and restored rather than peeked at.
	{regexp.MustCompile(`/[^"\n]*?(: |\n)`), `<path>${1}`},
	// A path that ends the line, but ONLY when the line has no further text after
	// it -- i.e. the path IS the tail. Anchored to a quote or a known
	// path-introducing token so an unquoted path followed by prose (the case
	// above) is never consumed.
	{regexp.MustCompile(`(output |file |path )/[^"\n]*$`), `${1}<path>`},
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
