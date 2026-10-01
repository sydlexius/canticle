package lyrics

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/timing"
)

// a2MarkerRe matches one Enhanced-LRC (A2) inline word marker, `<mm:ss.xx>`.
// It is the capturing twin of timing's marker pattern (same shape: 1+ digit
// minutes, seconds bounded to 00-59, exactly two fractional digits), so a
// marker this parser accepts is exactly one timing.StripWordMarkers removes and
// a malformed one (`<00:60.00>`, `<1:2.3>`) stays literal text in both.
var a2MarkerRe = regexp.MustCompile(`<(\d+):([0-5]\d)\.(\d{2})>`)

// maxMarkerMinutes caps a marker's minute field so an absurd value cannot
// overflow the millisecond arithmetic. 35000 minutes is 2.1e9 ms, which with
// the seconds and hundredths still fits a 32-bit int, so the arithmetic cannot
// overflow on any platform. A cap is a clamp, not a rejection: the marker still
// reads as a marker (it must not leak into the line text).
const maxMarkerMinutes = 35_000

// TimedWord is one A2 word: its start in milliseconds from track start and its
// text, trimmed of the padding the writer leaves between words.
type TimedWord struct {
	StartMS int
	Text    string
}

// TimedLine is one parsed cue. Text carries the words with A2 markers removed.
// Words is nil for a plain line-level cue; otherwise they are in source order,
// which may not be monotonic for a hand-edited or third-party file (not sorted). Decorative marks a cue with no lyric
// text (blank, note glyphs, an [key:value] tag), judged by timing.IsDecorative
// so it agrees with the timing guard on which cues count.
type TimedLine struct {
	StartMS    int
	Text       string
	Words      []TimedWord
	Decorative bool
}

// TimedLRC is the read-side result for a .lrc or its word-synced .elrc
// companion body.
type TimedLRC struct {
	Lines []TimedLine
	// Tags are the classified [key:value] header tags, in source order.
	Tags []lrcnormalize.Tag
	// HasWords reports whether at least one line carries word timings.
	HasWords bool
}

// ParseTimedLRC parses an LRC body, plain or Enhanced (A2), into lines and,
// where inline `<mm:ss.xx>` markers are present, word timings. It is the read
// counterpart of the writer's a2Words rendering; the .elrc companion is the
// same line format with every marked cue's text replaced by markers, so one
// parser serves both files.
//
// Line cues, ID tags, whitespace trimming and stacked-timestamp expansion
// (`[t1][t2]text` -> one line per stamp, each with its own copy of the words)
// all come from lrcnormalize.ParseBody; no second line parser lives here. Lines
// come back ascending by start (the order ParseBody guarantees).
//
// It never fails and never panics: a malformed marker is left in the text as
// literal, text before the first marker is kept as line text only, and a
// marker-less cue simply yields no words.
//
// Words keep source order and may go backwards in a hand-edited or third-party
// file (canticle's own writer always sorts); they are deliberately not sorted.
// An `[offset:]` tag is returned in Tags but never applied to any start time.
func ParseTimedLRC(body string) TimedLRC {
	doc := lrcnormalize.ParseBody(strings.TrimPrefix(body, utf8BOM))
	out := TimedLRC{Tags: doc.Tags, Lines: make([]TimedLine, 0, len(doc.Cues))}
	for _, cue := range doc.Cues {
		words := parseA2Words(cue.Text)
		text := strings.TrimSpace(timing.StripWordMarkers(cue.Text))
		out.Lines = append(out.Lines, TimedLine{
			StartMS:    cueStartMS(cue.Time),
			Text:       text,
			Words:      words,
			Decorative: timing.IsDecorative(text),
		})
		if len(words) > 0 {
			out.HasWords = true
		}
	}
	return out
}

// cueStartMS derives milliseconds from the cue's integer fields rather than
// its float Total, so the value is exactly what the file spelled.
func cueStartMS(t models.Time) int {
	return t.Minutes*60000 + t.Seconds*1000 + t.Hundredths*10
}

// parseA2Words splits a cue's text on its A2 markers. Each marker opens a word
// that runs to the next marker or the end of the text. Words with no text after
// trimming are dropped (a marker followed by another marker or by padding
// carries nothing to highlight). Returns nil when the cue has no markers.
func parseA2Words(text string) []TimedWord {
	if !strings.Contains(text, "<") {
		return nil
	}
	locs := a2MarkerRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return nil
	}
	var words []TimedWord
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		word := strings.TrimSpace(text[loc[1]:end])
		if word == "" {
			continue
		}
		words = append(words, TimedWord{
			StartMS: markerMS(text[loc[2]:loc[3]], text[loc[4]:loc[5]], text[loc[6]:loc[7]]),
			Text:    word,
		})
	}
	return words
}

// markerMS converts a marker's minute/second/hundredth digit strings to
// milliseconds. The regex guarantees digits, so the only Atoi failure is
// overflow, which clamps to the cap.
func markerMS(minutes, seconds, hundredths string) int {
	m, err := strconv.Atoi(minutes)
	if err != nil || m > maxMarkerMinutes {
		m = maxMarkerMinutes
	}
	s, _ := strconv.Atoi(seconds)
	h, _ := strconv.Atoi(hundredths)
	return m*60000 + s*1000 + h*10
}
