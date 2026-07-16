// Package lrcnormalize expands compressed multi-timestamp LRC lines into one
// cue per timestamp and classifies [key:value] ID-tag lines distinctly from
// real cues. It is a pure transform (no I/O) intended as the shared foundation
// for the LRC-text provider parse lanes, the write/backfill path, and the
// upgrade match-scorer; no consumer is wired yet.
package lrcnormalize

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/doxazo-net/canticle/internal/models"
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
		// Advance past the stamp, trimming surrounding whitespace. This matches
		// petitlyrics.parseLRC's TrimSpace on cue text, keeps a whitespace-
		// separated following stamp recognizable, and guarantees the remaining
		// text can never re-expose a leading timestamp.
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
