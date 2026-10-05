// Package timingacc measures how far a provider's synced-lyric line starts sit
// from a hand-verified reference (#1117). It is pure: no I/O, no clock, no
// logging. Callers parse both sides into Cue values and decide what to print.
package timingacc

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/normalize"
)

// WithinMS is the line-start tolerance behind the "share within" figure. It is
// a constant, not a flag, so reports stay comparable across runs.
const WithinMS = 300

// Word is one timed word of a cue.
type Word struct {
	StartMS int
	Text    string
}

// Cue is one synced line: start in milliseconds, its text, optional words.
type Cue struct {
	StartMS int
	Text    string
	Words   []Word
}

// pair is one matched cue: indexes into the reference and provider slices.
type pair struct{ Ref, Prov int }

type score struct{ n, cost int }

// better reports whether a beats b: longer alignment first, then lower cost.
func (a score) better(b score) bool {
	return a.n > b.n || (a.n == b.n && a.cost < b.cost)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// matchLines pairs reference cues with provider cues by normalized text using
// a monotone longest-common-subsequence, so pairs never cross. Among
// equal-length alignments the one with the smallest total absolute start
// difference wins. When a repeated line (a chorus) is missing on one side it is
// therefore paired with its nearest occurrence, so for tracks with repeated
// lines and a count mismatch the reported error is a lower bound. Cues pair
// only when their normalized text is equal, so punctuation or wording variants
// stay unmatched and reduce n rather than bias the error. Cues whose normalized
// text is empty never pair; they count as ref-unmatched or provider-extra like
// any other unpaired cue. It returns the pairs plus the indexes left unmatched
// on each side.
func matchLines(ref, prov []Cue) (pairs []pair, refUnmatched, provExtra []int) {
	rk, rs := make([]string, len(ref)), make([]int, len(ref))
	for i, c := range ref {
		rk[i], rs[i] = normalize.NormalizeKey(c.Text), c.StartMS
	}
	pk, ps := make([]string, len(prov)), make([]int, len(prov))
	for j, c := range prov {
		pk[j], ps[j] = normalize.NormalizeKey(c.Text), c.StartMS
	}
	return matchSeq(rk, rs, pk, ps)
}

// wordKey normalizes a word for pairing: leading and trailing punctuation and
// symbols are trimmed first so "Don't," pairs with "Don't". A key that ends up
// empty never pairs. Line keys are not trimmed this way.
func wordKey(text string) string {
	return normalize.NormalizeKey(strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSymbol(r)
	}))
}

// sortedWords returns ws ordered by start (stable), leaving ws untouched.
func sortedWords(ws []Word) []Word {
	out := slices.Clone(ws)
	slices.SortStableFunc(out, func(a, b Word) int { return a.StartMS - b.StartMS })
	return out
}

// uniformWordStarts mirrors lyrics.uniformStarts (internal/lyrics/a2.go): two
// or more words that all share one start are line-level data, which the writer
// itself refuses as word timing, so they are not measured as words.
func uniformWordStarts(ws []Word) bool {
	if len(ws) < 2 {
		return false
	}
	for _, w := range ws[1:] {
		if w.StartMS != ws[0].StartMS {
			return false
		}
	}
	return true
}

// matchWords is matchLines for the words of one matched line pair.
func matchWords(ref, prov []Word) (pairs []pair, refUnmatched, provExtra []int) {
	rk, rs := make([]string, len(ref)), make([]int, len(ref))
	for i, w := range ref {
		rk[i], rs[i] = wordKey(w.Text), w.StartMS
	}
	pk, ps := make([]string, len(prov)), make([]int, len(prov))
	for j, w := range prov {
		pk[j], ps[j] = wordKey(w.Text), w.StartMS
	}
	return matchSeq(rk, rs, pk, ps)
}

// matchSeq is the monotone matcher over normalized keys and start times.
func matchSeq(rk []string, rs []int, pk []string, ps []int) (pairs []pair, refUnmatched, provExtra []int) {
	nr, np := len(rk), len(pk)
	best := make([][]score, nr+1)
	for i := range best {
		best[i] = make([]score, np+1)
	}
	for i := 1; i <= nr; i++ {
		for j := 1; j <= np; j++ {
			cur := best[i-1][j]
			if s := best[i][j-1]; s.better(cur) {
				cur = s
			}
			if rk[i-1] != "" && rk[i-1] == pk[j-1] {
				d := best[i-1][j-1]
				s := score{d.n + 1, d.cost + absInt(ps[j-1]-rs[i-1])}
				if s.better(cur) {
					cur = s
				}
			}
			best[i][j] = cur
		}
	}
	matchedRef := make([]bool, nr)
	matchedProv := make([]bool, np)
	for i, j := nr, np; i > 0 && j > 0; {
		if rk[i-1] != "" && rk[i-1] == pk[j-1] {
			d := best[i-1][j-1]
			if best[i][j] == (score{d.n + 1, d.cost + absInt(ps[j-1]-rs[i-1])}) {
				pairs = append(pairs, pair{i - 1, j - 1})
				matchedRef[i-1], matchedProv[j-1] = true, true
				i, j = i-1, j-1
				continue
			}
		}
		if best[i][j] == best[i-1][j] {
			i--
		} else {
			j--
		}
	}
	for l, r := 0, len(pairs)-1; l < r; l, r = l+1, r-1 {
		pairs[l], pairs[r] = pairs[r], pairs[l]
	}
	for i, m := range matchedRef {
		if !m {
			refUnmatched = append(refUnmatched, i)
		}
	}
	for j, m := range matchedProv {
		if !m {
			provExtra = append(provExtra, j)
		}
	}
	return pairs, refUnmatched, provExtra
}

// LineStats accumulates line-start accuracy for one report group.
type LineStats struct {
	Tracks       int
	RefCues      int
	Matched      int
	RefUnmatched int
	ProvExtra    int
	SumAbsErrMS  int
	WithinCount  int
	// ResidualSumMS is the cue-weighted sum of each track's declared reference
	// residual; ResidualMaxMS the largest residual among tracks with matches.
	ResidualSumMS int
	ResidualMaxMS int
}

// AddTrack matches one served track against its reference and folds the result
// in. residualMS is the reference track's declared line residual.
func (s *LineStats) AddTrack(ref, prov []Cue, residualMS int) {
	pairs, refU, provX := matchLines(ref, prov)
	s.Tracks++
	s.RefCues += len(ref)
	s.Matched += len(pairs)
	s.RefUnmatched += len(refU)
	s.ProvExtra += len(provX)
	for _, p := range pairs {
		e := absInt(prov[p.Prov].StartMS - ref[p.Ref].StartMS)
		s.SumAbsErrMS += e
		if e <= WithinMS {
			s.WithinCount++
		}
	}
	if len(pairs) > 0 {
		s.ResidualSumMS += residualMS * len(pairs)
		s.ResidualMaxMS = max(s.ResidualMaxMS, residualMS)
	}
}

// MAE is the mean absolute line-start error in ms; ok is false with no matches.
func (s LineStats) MAE() (float64, bool) {
	if s.Matched == 0 {
		return 0, false
	}
	return float64(s.SumAbsErrMS) / float64(s.Matched), true
}

// Within is the share of matched cues within WithinMS, in [0,1]; ok is false
// with no matches.
func (s LineStats) Within() (float64, bool) {
	if s.Matched == 0 {
		return 0, false
	}
	return float64(s.WithinCount) / float64(s.Matched), true
}

// Residual is the cue-weighted mean declared reference residual and its max.
func (s LineStats) Residual() (mean float64, maxMS int, ok bool) {
	if s.Matched == 0 {
		return 0, 0, false
	}
	return float64(s.ResidualSumMS) / float64(s.Matched), s.ResidualMaxMS, true
}

// FormatRow renders one aggregate report row. key must be a lane or
// lane/upstream token, never track identity. With no matched cues no error
// figure is printed (a zero would read as perfect).
func FormatRow(key string, s LineStats) string {
	row := fmt.Sprintf("%s: tracks=%d cues(ref=%d matched=%d ref-unmatched=%d provider-extra=%d)",
		key, s.Tracks, s.RefCues, s.Matched, s.RefUnmatched, s.ProvExtra)
	mae, ok := s.MAE()
	if !ok {
		return row + " line-MAE=n/a"
	}
	w, _ := s.Within()
	mean, mx, _ := s.Residual()
	return row + fmt.Sprintf(" line-MAE=%.0fms within-%dms=%.1f%% ref-residual(mean=%.0fms max=%dms) n=%d",
		mae, WithinMS, w*100, mean, mx, s.Matched)
}

// WordStats accumulates word-start accuracy for one report group. Words pair
// only inside line pairs the line matcher already matched, and only when both
// sides carry word timings for that line (a provider line whose words all share
// one start counts as having none). RefWords counts the reference words of every
// matched line that has them. With a repeated word the error is a lower bound.
type WordStats struct {
	// Tracks counts tracks where at least one matched line had words on both sides.
	Tracks        int
	RefWords      int
	Matched       int
	SumAbsErrMS   int
	WithinCount   int
	ResidualSumMS int
	ResidualMaxMS int
}

// AddTrack folds one served track's words in. residualMS is the reference
// track's declared word residual.
func (s *WordStats) AddTrack(ref, prov []Cue, residualMS int) {
	pairs, _, _ := matchLines(ref, prov)
	matched, seen := 0, false
	for _, p := range pairs {
		rw, pw := sortedWords(ref[p.Ref].Words), sortedWords(prov[p.Prov].Words)
		if len(rw) == 0 {
			continue
		}
		// Reference words count on every matched line that has them, so the row
		// shows partial coverage when the lane served fewer.
		s.RefWords += len(rw)
		if len(pw) == 0 || uniformWordStarts(pw) {
			continue
		}
		seen = true
		wp, _, _ := matchWords(rw, pw)
		for _, q := range wp {
			e := absInt(pw[q.Prov].StartMS - rw[q.Ref].StartMS)
			s.SumAbsErrMS += e
			if e <= WithinMS {
				s.WithinCount++
			}
		}
		matched += len(wp)
	}
	if seen {
		s.Tracks++
	}
	s.Matched += matched
	if matched > 0 {
		s.ResidualSumMS += residualMS * matched
		s.ResidualMaxMS = max(s.ResidualMaxMS, residualMS)
	}
}

// FormatWordRow renders the word-start row for a report group. With no matched
// words no error figure is printed (a zero would read as perfect).
func FormatWordRow(key string, s WordStats) string {
	row := fmt.Sprintf("%s: word-tracks=%d words(ref=%d matched=%d)", key, s.Tracks, s.RefWords, s.Matched)
	if s.Matched == 0 {
		return row + " word-MAE=n/a"
	}
	n := float64(s.Matched)
	return row + fmt.Sprintf(" word-MAE=%.0fms within-%dms=%.1f%% ref-residual(mean=%.0fms max=%dms) n=%d",
		float64(s.SumAbsErrMS)/n, WithinMS, float64(s.WithinCount)/n*100, float64(s.ResidualSumMS)/n, s.ResidualMaxMS, s.Matched)
}
