package aligner

import (
	"math"
	"strings"
)

// SuggestedWord is one word unit of a suggested line. A unit runs from Token
// to the next unit's Token (or the line's end), so tokens the sidecar omitted
// belong to the unit before them; the first unit's Token is always 0.
type SuggestedWord struct {
	// Token indexes strings.Fields(line), the accept route's token index.
	Token int
	// StartMS is milliseconds from the start of the audio, as Word.StartMS.
	StartMS int
}

// Suggestion is a proposed retiming of the lines handed to Suggest.
type Suggestion struct {
	// LineMS holds one start per input line, in order, in ms from the start
	// of the audio. Guaranteed: never negative, non-decreasing, and consecutive
	// lines that shared a start before share one here (no group is split).
	// NOT guaranteed: that lines with different starts before keep different
	// stamps. Without room they share a hundredth, counted in Quality.Merged.
	LineMS []int
	// Words holds, per line, its word units, or nil. A kept line has two or more,
	// non-decreasing, each within [LineMS[i], the next group's LineMS].
	Words   [][]SuggestedWord
	Quality Quality
}

const (
	// maxStartMS mirrors lyrics.maxRetimeMS (24 hours), the accept route's bound.
	maxStartMS = 24 * 60 * 60 * 1000
	// stampMS: the .lrc writer truncates to hundredths, so two starts in one are ONE stamp.
	stampMS = 10
)

// Suggest maps an alignment result to per-line starts (#1008). lines is the
// list sent to Align, one entry per cue of the current .lrc in file order ("" for
// a cue that was not sent); starts is those cues' current starts in ms. Its
// consumer is the Auto poll payload in internal/web/auto_run.go (#1008 slice
// S7's web half), which must check the bool. Pure: no I/O, arguments unmodified.
//
// r must be a result from Align or AlignFile: words in line order, starts
// non-decreasing. Nothing else is repaired: Suggest returns false and the zero
// Suggestion when no word is used (nothing aligned: no suggestion to make), the
// used words go backwards in start or line index, lines and starts differ in
// length, or a current start is outside [0, 24 hours]. Times are ms.
//   - A word is used only if its LineIndex names a non-blank line, its StartMS
//     is within [0, 24 hours] and its EndMS is not before it.
//   - Consecutive lines with equal current starts are one same-stamp group with
//     ONE start: its first used word's StartMS, on any member, never moved.
//   - A group with no used word, between two aligned groups whose current
//     starts bracket its own, is interpolated between their aligned starts in
//     proportion to its current position. Any other (before the first aligned
//     group, after the last, or among unsorted starts) is carried by the shift
//     of the aligned group nearest by current start (the earlier on a tie),
//     within [0, 24 hours].
//   - Separation is judged in hundredths, where a group forms on disk.
//     Unaligned groups ahead of an aligned one are pulled back, last to first,
//     each to the end of the hundredth before the group after it. Then, first
//     to last, each is raised to the previous group's start and, if it shares
//     that group's hundredth, moved to the start of the next one, unless that
//     is the next aligned group's hundredth or past 24 hours.
//   - No room (too few hundredths between two aligned starts, aligned starts
//     sharing one, or the 24 hour bound): nothing is reordered, and
//     Quality.Merged counts the pair.
func Suggest(lines []string, starts []int, r Result) (Suggestion, bool) {
	n := len(lines)
	if n != len(starts) {
		return Suggestion{}, false
	}
	byLine := make([][]Word, n)
	lastLine, lastMS := 0, 0
	for _, w := range r.Words {
		if w.LineIndex < 0 || w.LineIndex >= n || w.StartMS < 0 || w.StartMS > maxStartMS || w.EndMS < w.StartMS || isPythonBlank(lines[w.LineIndex]) {
			continue
		}
		if w.LineIndex < lastLine || w.StartMS < lastMS {
			return Suggestion{}, false
		}
		lastLine, lastMS = w.LineIndex, w.StartMS
		byLine[w.LineIndex] = append(byLine[w.LineIndex], w)
	}

	// leader[i] leads i's same-stamp group; anchor[l] is group l's aligned start, or -1.
	leader, anchor := make([]int, n), make([]int, n)
	for i, st := range starts {
		if st < 0 || st > maxStartMS {
			return Suggestion{}, false
		}
		leader[i], anchor[i] = i, -1
		if i > 0 && st == starts[i-1] {
			leader[i] = leader[i-1]
		}
		if l := leader[i]; anchor[l] < 0 && len(byLine[i]) > 0 {
			anchor[l] = byLine[i][0].StartMS
		}
	}
	var aligned []int // leaders with an anchor, in order
	for i, a := range anchor {
		if a >= 0 {
			aligned = append(aligned, i)
		}
	}
	if len(aligned) == 0 {
		return Suggestion{}, false
	}
	// Back to front: leader i's value, pulled under the group after it;
	// limit[i] is the hundredth the forward pass must stay below.
	out, limit := make([]int, n), make([]int, n)
	ceil, lim := -1, maxStartMS/stampMS+1
	for i, k := n-1, len(aligned); i >= 0; i-- {
		if leader[i] != i {
			continue
		}
		limit[i] = lim
		switch {
		case anchor[i] >= 0:
			k--
			out[i], ceil, lim = anchor[i], anchor[i], anchor[i]/stampMS
		case ceil >= 0:
			ceil = min(carry(starts, anchor, aligned, k, i), max(ceil/stampMS*stampMS-1, 0))
			out[i] = ceil
		default:
			out[i] = carry(starts, anchor, aligned, k, i)
		}
	}
	prev := -1
	for i := range out {
		if leader[i] != i {
			out[i] = out[i-1]
			continue
		}
		if anchor[i] < 0 && prev >= 0 {
			out[i] = max(out[i], prev)
			if c := (prev/stampMS + 1) * stampMS; out[i] < c && c/stampMS < limit[i] {
				out[i] = c
			}
		}
		prev = out[i]
	}

	s := Suggestion{LineMS: out, Words: make([][]SuggestedWord, n)}
	var tokens, matched, merged int
	for i, line := range lines {
		if i > 0 && leader[i] == i && out[i]/stampMS == out[i-1]/stampMS {
			merged++
		}
		hi := math.MaxInt
		for j := i + 1; j < n; j++ {
			if leader[j] != leader[i] {
				hi = out[j]
				break
			}
		}
		u, m, t := units(line, byLine[i])
		tokens, matched = tokens+t, matched+m
		if len(u) >= 2 && bounded(u, hi) {
			s.Words[i] = u
		}
	}
	s.Quality = measure(lines, byLine, r.Transcript, tokens, matched)
	s.Quality.Merged = merged
	return s, true
}

// carry returns unaligned leader i's start before separation; aligned (never
// empty) holds the aligned leaders, aligned[k] the first after i. See Suggest.
func carry(starts, anchor, aligned []int, k, i int) int {
	from := -1
	if k > 0 {
		from = aligned[k-1]
	}
	if k < len(aligned) {
		after := aligned[k]
		if from >= 0 {
			// All within [0, maxStartMS]: no int64 overflow; sp < sn: no zero divisor.
			// ap <= an always: used words are non-decreasing and in line order.
			s, sp, sn := int64(starts[i]), int64(starts[from]), int64(starts[after])
			if ap, an := int64(anchor[from]), int64(anchor[after]); sp < s && s < sn {
				return int(ap + (s-sp)*(an-ap)/(sn-sp))
			}
		}
		// Absolute distances: an unsorted start can lie below or above both.
		if from < 0 || dist(starts[after], starts[i]) < dist(starts[i], starts[from]) {
			from = after
		}
	}
	return min(max(starts[i]+anchor[from]-starts[from], 0), maxStartMS)
}

// dist is the absolute difference of two starts.
func dist(a, b int) int {
	if a < b {
		return b - a
	}
	return a - b
}

// units matches one line's used words to its tokens, in order, and returns the
// word units, the sidecar tokens matched, and the sidecar tokens the line has.
// The sidecar splits with Python str.split(), wider than strings.Fields (see
// pythonExtraSpace), so one field can hold several sidecar tokens; its unit
// starts at the first matched one. A word matching no remaining token: no units.
// Matching is by text alone: had the sidecar skipped only the first of two equal
// tokens, it would take the second's time (equal text is skipped alike today).
func units(line string, ws []Word) (u []SuggestedWord, matched, total int) {
	type token struct {
		text  string
		field int
	}
	var toks []token
	for f, field := range strings.Fields(line) {
		for _, t := range strings.FieldsFunc(field, isPythonSpace) {
			toks = append(toks, token{t, f})
		}
	}
	next, ok := 0, true
	for _, w := range ws {
		j := next
		for j < len(toks) && toks[j].text != w.Text {
			j++
		}
		if j == len(toks) {
			ok = false
			continue
		}
		next, matched = j+1, matched+1
		if len(u) == 0 || u[len(u)-1].Token != toks[j].field {
			u = append(u, SuggestedWord{Token: toks[j].field, StartMS: w.StartMS})
		}
	}
	if !ok || len(u) == 0 {
		return nil, matched, len(toks)
	}
	u[0].Token = 0 // tokens omitted before the first aligned word join it
	return u, matched, len(toks)
}

// bounded reports whether no unit is past hi, the next group's start. None can
// precede its own line: an aligned group starts at its first word, never moved.
// A failing line keeps no words, never a moved time.
func bounded(u []SuggestedWord, hi int) bool {
	for _, w := range u {
		if w.StartMS > hi {
			return false
		}
	}
	return true
}
