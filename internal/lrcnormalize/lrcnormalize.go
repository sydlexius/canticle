// Package lrcnormalize expands compressed multi-timestamp LRC lines into one
// cue per timestamp and classifies [key:value] ID-tag lines distinctly from
// real cues. It is a pure transform (no I/O) intended as the shared foundation
// for the LRC-text provider parse lanes, the write/backfill path, and the
// upgrade match-scorer. Wired into the petitlyrics parse lane (#470);
// lrcbackfill drives the .lrc rewrite path.
package lrcnormalize

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// tsRe matches one leading LRC timestamp token: [mm:ss], [mm:ss.xx], or
// [mm:ss.xxx]. Anchored at the start of the (remaining) string.
var tsRe = regexp.MustCompile(`^\[(\d{1,2}):(\d{2})(?:[.:](\d{1,3}))?\]`)

// Tag is a classified LRC header/ID tag: [ar:Foo] -> {Key:"ar", Value:"Foo"}.
type Tag struct {
	Key   string
	Value string
	Raw   string
}

// Document is the expanded, classified result of parsing an LRC body.
type Document struct {
	Tags []Tag          // classified [key:value] tags, in source order
	Cues []models.Lines // one cue per timestamp, expanded and sorted ascending (stable)
}

// ParseBody parses a raw .lrc body (which may include a leading header block)
// into classified tags and expanded, time-sorted cues.
func ParseBody(body string) Document {
	var doc Document
	for _, raw := range splitLines(body) {
		cues := expandLine(raw)
		if len(cues) == 0 {
			// Not a cue line. Classify a [key:value] header tag; drop anything
			// else (blank or orphan text -- it cannot be a synced cue).
			if tag, ok := parseTag(raw); ok {
				doc.Tags = append(doc.Tags, tag)
			}
			continue
		}
		doc.Cues = append(doc.Cues, cues...)
	}
	sortCues(doc.Cues)
	return doc
}

// expandLine expands the consecutive leading timestamps on a single line into
// one cue per stamp, all sharing the line's trailing text. Once non-timestamp
// text begins, the remainder is literal -- this deliberately leaves enhanced/
// word-level <..> markup and mid-line [..] untouched. Returns nil for a line
// with no leading timestamp.
func expandLine(raw string) []models.Lines {
	rest := raw
	var stamps [][]string
	for {
		m := tsRe.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		stamps = append(stamps, m)
		// Advance past the stamp, trimming surrounding whitespace. Trimming cue
		// text is the shared convention across the provider parse lanes, keeps a
		// whitespace-separated following stamp recognizable, and guarantees the
		// remaining text can never re-expose a leading timestamp.
		rest = strings.TrimSpace(rest[len(m[0]):])
	}
	if len(stamps) == 0 {
		return nil
	}
	cues := make([]models.Lines, 0, len(stamps))
	for _, m := range stamps {
		cues = append(cues, newCue(m[1], m[2], m[3], rest))
	}
	return cues
}

// sortCues stably sorts cues ascending by timestamp.
func sortCues(cues []models.Lines) {
	sort.SliceStable(cues, func(i, j int) bool {
		return cues[i].Time.Total < cues[j].Time.Total
	})
}

// parseTag classifies a full-line LRC header tag [key:value] whose key begins
// with a non-digit (mirrors internal/lyrics.parseTagLine). Returns ok=false for
// a timestamp line, a non-bracketed line, or a malformed tag. Raw preserves the
// original line.
func parseTag(line string) (Tag, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return Tag{}, false
	}
	inner := s[1 : len(s)-1]
	idx := strings.IndexByte(inner, ':')
	if idx <= 0 {
		return Tag{}, false
	}
	key := inner[:idx]
	if key[0] >= '0' && key[0] <= '9' {
		return Tag{}, false // timestamp line, not a tag
	}
	return Tag{Key: key, Value: inner[idx+1:], Raw: line}, true
}

// Expand re-splits an already-parsed Synced whose cue .Text fields may still
// hold leading embedded timestamps (left by the single-timestamp-regex parse
// bug) and returns a one-cue-per-timestamp, time-sorted Synced. Idempotent: a Synced
// whose cues carry no embedded leading timestamp passes through unchanged.
func Expand(s models.Synced) models.Synced {
	var out []models.Lines
	for _, line := range s.Lines {
		// Split only the timestamps embedded in the cue's TEXT (the parse-bug
		// artifact). The cue's own Time is kept verbatim -- never re-derived from
		// a rendered string -- so Total, and any minute value, survive intact.
		embedded := expandLine(line.Text)
		if len(embedded) == 0 {
			out = append(out, line) // no embedded stamp: pass through unchanged
			continue
		}
		shared := embedded[0].Text // trailing text after all embedded stamps
		first := line
		first.Text = shared
		out = append(out, first)
		out = append(out, embedded...)
	}
	sortCues(out)
	return models.Synced{Lines: out}
}

// NormalizeBody expands stacked multi-timestamp lines in a raw LRC body in
// place, one cue per timestamp, and returns the rewritten body plus whether any
// line changed. It is minimally invasive: every non-stacked line (header tags,
// single-timestamp cues, blank lines, enhanced <..> word-sync markup, non-cue
// text) is preserved verbatim, the original timestamp substrings and formatting
// are kept (no re-render / precision loss), and line endings plus the trailing-
// newline state are preserved. Idempotent: a body with no stacked line returns
// unchanged.
func NormalizeBody(body string) (string, bool) {
	changed := false
	var b strings.Builder
	b.Grow(len(body))
	for _, seg := range splitKeepEOL(body) {
		expanded, did := splitStackedLine(seg.content)
		if !did {
			b.WriteString(seg.content)
			b.WriteString(seg.term)
			continue
		}
		changed = true
		// Separate the new sub-lines with the source line's own terminator so
		// each untouched line keeps its exact EOL (mixed LF/CRLF preserved). When
		// the stacked line had no terminator (EOF), use "\n" between the new lines
		// but leave the final one unterminated to preserve the no-trailing-newline
		// state.
		sep := seg.term
		if sep == "" {
			sep = "\n"
		}
		for i, e := range expanded {
			b.WriteString(e)
			if i < len(expanded)-1 {
				b.WriteString(sep)
			} else {
				b.WriteString(seg.term)
			}
		}
	}
	if !changed {
		return body, false
	}
	return b.String(), true
}

// eolSegment is one source line: its content (terminator stripped) and the exact
// terminator that followed it ("\n", "\r\n", or "" at EOF).
type eolSegment struct {
	content string
	term    string
}

// splitKeepEOL splits body into lines while preserving each line's exact
// terminator, so a rewrite can reproduce mixed line endings byte-for-byte.
func splitKeepEOL(body string) []eolSegment {
	var segs []eolSegment
	for i := 0; i < len(body); {
		j := strings.IndexByte(body[i:], '\n')
		if j < 0 {
			segs = append(segs, eolSegment{content: body[i:], term: ""})
			break
		}
		content := body[i : i+j]
		term := "\n"
		if strings.HasSuffix(content, "\r") {
			content = content[:len(content)-1]
			term = "\r\n"
		}
		segs = append(segs, eolSegment{content: content, term: term})
		i += j + 1
	}
	return segs
}

// splitStackedLine expands a single line carrying two or more adjacent leading
// timestamps into one line per timestamp, each preserving the exact original
// timestamp substring and the shared trailing text verbatim, sorted ascending by
// time (stable). Returns (nil, false) for a line with fewer than two leading
// timestamps -- adjacency is required (tsRe is anchored, so any non-'[' between
// stamps, including whitespace, ends the run and the line is left untouched).
func splitStackedLine(line string) ([]string, bool) {
	type stamp struct {
		raw   string
		total float64
	}
	var stamps []stamp
	rest := line
	for {
		m := tsRe.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		stamps = append(stamps, stamp{raw: m[0], total: stampSeconds(m[1], m[2], m[3])})
		rest = rest[len(m[0]):]
	}
	if len(stamps) < 2 {
		return nil, false
	}
	sort.SliceStable(stamps, func(i, j int) bool { return stamps[i].total < stamps[j].total })
	expanded := make([]string, len(stamps))
	for i, s := range stamps {
		expanded[i] = s.raw + rest
	}
	return expanded, true
}

// splitLines splits a body on newlines, trimming a trailing carriage return.
func splitLines(body string) []string {
	lines := regexp.MustCompile("\r?\n").Split(body, -1)
	return lines
}

// newCue builds a models.Lines cue from captured timestamp parts and text.
func newCue(min, sec, frac, text string) models.Lines {
	m, _ := strconv.Atoi(min)
	s, _ := strconv.Atoi(sec)
	h := normalizeHundredths(frac)
	return models.Lines{
		Text: text,
		Time: models.Time{
			Total:      float64(m*60+s) + float64(h)/100.0,
			Minutes:    m,
			Seconds:    s,
			Hundredths: h,
		},
	}
}

// stampSeconds returns a timestamp's absolute seconds at the fractional string's
// full precision, so the within-line ascending sort orders stamps that differ
// only in the third fractional digit correctly (normalizeHundredths would
// truncate them to a tie). Used only as a sort key; emitted substrings are the
// exact originals.
func stampSeconds(min, sec, frac string) float64 {
	m, _ := strconv.Atoi(min)
	s, _ := strconv.Atoi(sec)
	total := float64(m*60 + s)
	if frac != "" {
		n, _ := strconv.Atoi(frac)
		total += float64(n) / math.Pow10(len(frac))
	}
	return total
}

// normalizeHundredths converts a captured fractional-second string (0-3 digits)
// to hundredths of a second, matching the petitlyrics parser's convention.
func normalizeHundredths(frac string) int {
	switch len(frac) {
	case 0:
		return 0
	case 1:
		n, _ := strconv.Atoi(frac)
		return n * 10
	case 2:
		n, _ := strconv.Atoi(frac)
		return n
	default: // 3 digits: milliseconds -> hundredths
		n, _ := strconv.Atoi(frac[:2])
		return n
	}
}
