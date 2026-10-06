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
	// URLs (#1167). FIRST, because the bare-path rule below would otherwise
	// mangle "//host/path" and strand the scheme.
	//
	// A file:// or smb:// URL is a library PATH in URL form, so it may contain
	// spaces ("smb://nas/music/Some Artist/x.flac"). It follows the path rules'
	// delimiter discipline: up to ": ", a quote, a tab or a newline...
	{regexp.MustCompile(`(?i)\b(?:file|smb)://[^"'\t\n<>]*?(: |"|'|\t|\n)`), `<url>${1}`},
	// ...or to end of TEXT when its last segment has no whitespace, the same
	// test the verb rule below uses to tell a path tail from trailing prose.
	//
	// No (?m) here, nor on the Windows extension rule, the "output " rule or
	// the unprefixed extension rule below: each has a delimited sibling whose
	// delimiter set includes "\n", so a path that ends a line INSIDE the text
	// is claimed before "$" is tried, and "$" only ever sees the end of the
	// whole value. A (?m) there was dead (no test could tell it apart), so it is
	// gone rather than left implying a case it does not handle. The verb rule
	// and the track rule keep theirs; each says why.
	{regexp.MustCompile(`(?i)\b(?:file|smb)://(?:[^"'\t\n<>]*/)?[^/\s"'<>]*$`), `<url>`},
	// Any other URL, with a bracketed IPv6 host ("http://[fd00::1]:8080/x") and
	// its whole query string: a request URL can carry credential-like parameters
	// ("?usertoken=..."). Host, path and query all stop only at whitespace, a
	// quote or an angle bracket. The path deliberately CONSUMES ")" and "]": an
	// earlier version stopped there, so "/a(b)c?usertoken=SECRET" left the whole
	// query behind. The cost is that a URL wrapped in "(...)" takes its closing
	// paren with it, which is stable and leaks nothing.
	{regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://(?:\[[0-9A-Fa-f:.]+\])?[^\s"'<>?]*(?:\?[^\s"'<>]*)?`), `<url>`},
	// A Windows drive path or UNC path (\\host\share\...), quoted or not, up to
	// ": ", a quote, a tab or a newline. NOT end of line: "open D:\x.flac
	// permission denied" and "... checksum mismatch" are two causes, and an
	// end-of-line exit merged them.
	{regexp.MustCompile(`(?:\b[A-Za-z]:\\|\\\\[^\\\s"]+\\)[^"\t\n]*?(: |"|\t|\n)`), `<path>${1}`},
	// A Windows/UNC path that ends the text in a KNOWN audio or sidecar
	// extension: the extension proves the path IS the tail, as in the POSIX
	// tail rule below (see mediaExt for why it is an allowlist).
	{regexp.MustCompile(`(?:\b[A-Za-z]:\\|\\\\[^\\\s"]+\\)[^"\t\n]*` + mediaExt + `$`), `<path>`},
	// Quoted track identity (#1167): lyrics.LRCWriter now emits
	// `nothing to save for "<artist>" - "<title>"` with Go quoting (LEGACY
	// since #1164: the writer now emits a plain "nothing to save", but old
	// work_queue.last_error rows keep this shape, so the rule stays), so each
	// field ends at its first unescaped quote and a title's own ": " can no
	// longer pass for a cause boundary. Whatever follows the closing quote (a
	// Go-convention ": cause") is kept. Runs before the legacy rule below, which
	// stays for rows written by older builds.
	{regexp.MustCompile(`\bnothing to save for "(?:[^"\\\n]|\\.)*" - "(?:[^"\\\n]|\\.)*"`), `nothing to save for <track>`},
	// Free-text track identity (#1167, #1164), LEGACY unquoted form. Anchored to the one former emitter,
	// lyrics.LRCWriter's "nothing to save for <artist> - <title>" (no longer
	// emitted, still present in old last_error rows); no other
	// emitter in internal/ prints a track after a fixed phrase. Deliberately NOT
	// a generic "no results for": petitlyrics.ErrProviderUnavailable's "no
	// results for 20 consecutive lookups (application id revoked?)" is fixed
	// text that must survive. Stops at ": " followed by a LOWER-CASE letter, so
	// a cause in Go's error convention ("...: context deadline exceeded")
	// survives for grouping and Classify, while a title's own ": Part Two" (and
	// a " (feat. Other Artist)", hence no " (" stop) is still stripped. The
	// writer's error is innermost today, so no cause follows it in practice; a
	// title with ": lower-case" text leaves that tail, the accepted cost of not
	// erasing a cause. (?m) so a line inside an errors.Join value is caught. RE2
	// has no lookahead, so the delimiter letter is captured and restored.
	{regexp.MustCompile(`(?m)\bnothing to save for [^\n]*?(: [a-z]|$)`), `nothing to save for <track>${1}`},
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
	//
	// A tab is a delimiter too (#1167): no library path contains one, and a
	// tab-separated cause must survive like a ": "-separated one.
	{regexp.MustCompile(`/[^"\t\n]*?(: |\t|\n)`), `<path>${1}`},
	// A path after a known path-taking verb that ends the line with no delimiter
	// (#1167), e.g. "stat /srv/Some Artist/Album" (no extension, so the tail
	// rule below cannot claim it). Only when the LAST segment has no whitespace:
	// that is what separates a path tail from a path followed by prose, so
	// "read /mnt/a permission denied" keeps its cause. POSIX, drive and UNC
	// roots. The verbs are the os.PathError/LinkError ops plus ffmpeg's wrapper.
	// (?m) is live here, unlike the tail rules: a bare UNC host ("\\nas") has
	// no second backslash, so no delimited sibling claims it mid-text.
	{regexp.MustCompile(`(?m)\b(open|stat|lstat|read|write|mkdir|rename|remove|readdir|opendir|symlink|link|ffmpeg exited:) (?:/|[A-Za-z]:\\|\\\\)(?:[^"\t\n]*[/\\])?[^/\\\s"]*$`), `${1} <path>`},
	// A path that ends the line, but ONLY when the line has no further text after
	// it -- i.e. the path IS the tail. Anchored to a quote or a known
	// path-introducing token so an unquoted path followed by prose (the case
	// above) is never consumed.
	{regexp.MustCompile(`(output |file |path )/[^"\t\n]*$`), `${1}<path>`},
	// An unprefixed path that ends the text in a known extension (#1167), e.g.
	// "open /share/Music/A B/track.flac". The extension is what proves the path
	// IS the tail: "read /mnt/a permission denied" ends in prose, has no
	// extension, and keeps its diagnostic. A directory with no extension and no
	// delimiter is still left alone rather than guessed at.
	{regexp.MustCompile(`(^|\s)/[^"\t\n]*` + mediaExt + `$`), `${1}<path>`},
}

// mediaExt matches a trailing extension canticle actually reads or writes:
// the audio formats a library holds, the sidecars it writes, and the temp
// file's suffix. Case-insensitive, since "TRACK.FLAC" is as real as
// "track.flac".
//
// AN ALLOWLIST, DELIBERATELY. The first version accepted any 1-5 alphanumeric
// token after a dot, so every trailing dotted word read as an extension and
// erased the prose before it: "read /mnt/a timed out after 2.5" and "... after
// 3.0" merged, as did "... in v1.2" and "... via host.com", and Musixmatch's
// own "Cannot GET /ws/1.1/track.get" error body merged with ".../macro.get".
// That is the over-normalization this package exists to prevent.
// It must cover every extension scanner.supportedFileTypes accepts (a test pins
// that), or a supported format's library path survives into the signature.
const mediaExt = `\.(?i:flac|mp3|m4a|m4b|m4p|mp4|ogg|oga|opus|wav|aac|wma|ape|wv|dsf|dff|aif|aiff|alac|lrc|elrc|txt|tmp)`

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
