// Package timingacc measures how far a provider's synced-lyric line starts sit
// from a hand-verified reference (#1117). It is pure: no I/O, no clock, no
// logging. Callers parse both sides into Cue values and decide what to print.
package timingacc

import (
	"fmt"

	"github.com/sydlexius/canticle/internal/normalize"
)

// WithinMS is the line-start tolerance behind the "share within" figure. It is
// a constant, not a flag, so reports stay comparable across runs.
const WithinMS = 300

// Word is one timed word of a cue (reserved for word-start accuracy).
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
	rk := make([]string, len(ref))
	for i, c := range ref {
		rk[i] = normalize.NormalizeKey(c.Text)
	}
	pk := make([]string, len(prov))
	for j, c := range prov {
		pk[j] = normalize.NormalizeKey(c.Text)
	}
	best := make([][]score, len(ref)+1)
	for i := range best {
		best[i] = make([]score, len(prov)+1)
	}
	for i := 1; i <= len(ref); i++ {
		for j := 1; j <= len(prov); j++ {
			cur := best[i-1][j]
			if s := best[i][j-1]; s.better(cur) {
				cur = s
			}
			if rk[i-1] != "" && rk[i-1] == pk[j-1] {
				d := best[i-1][j-1]
				s := score{d.n + 1, d.cost + absInt(prov[j-1].StartMS-ref[i-1].StartMS)}
				if s.better(cur) {
					cur = s
				}
			}
			best[i][j] = cur
		}
	}
	matchedRef := make([]bool, len(ref))
	matchedProv := make([]bool, len(prov))
	for i, j := len(ref), len(prov); i > 0 && j > 0; {
		if rk[i-1] != "" && rk[i-1] == pk[j-1] {
			d := best[i-1][j-1]
			if best[i][j] == (score{d.n + 1, d.cost + absInt(prov[j-1].StartMS-ref[i-1].StartMS)}) {
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
