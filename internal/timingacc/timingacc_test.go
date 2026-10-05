package timingacc

import (
	"strings"
	"testing"
)

func cues(startsMS []int, texts ...string) []Cue {
	out := make([]Cue, len(texts))
	for i, t := range texts {
		out[i] = Cue{StartMS: startsMS[i], Text: t}
	}
	return out
}

func TestExactMatchHasZeroError(t *testing.T) {
	ref := cues([]int{1000, 5000, 9000}, "alpha one", "beta two", "gamma three")
	var s LineStats
	s.AddTrack(ref, ref, 20)
	if mae, ok := s.MAE(); !ok || mae != 0 {
		t.Fatalf("MAE = %v,%v; want 0,true", mae, ok)
	}
	if w, _ := s.Within(); w != 1 {
		t.Errorf("within = %v; want 1", w)
	}
}

func TestUniformShift(t *testing.T) {
	ref := cues([]int{1000, 5000, 9000}, "alpha", "beta", "gamma")
	prov := cues([]int{1500, 5500, 9500}, "Alpha", "BETA", "gamma")
	var s LineStats
	s.AddTrack(ref, prov, 0)
	if mae, _ := s.MAE(); mae != 500 {
		t.Errorf("MAE = %v; want 500", mae)
	}
	if w, _ := s.Within(); w != 0 {
		t.Errorf("within = %v; want 0", w)
	}
}

func TestCountMismatch(t *testing.T) {
	ref := cues([]int{1000, 5000, 9000}, "alpha", "beta", "gamma")
	prov := cues([]int{1000, 7000, 9000, 12000}, "alpha", "extra", "gamma", "outro")
	pairs, refU, provX := matchLines(ref, prov)
	if len(pairs) != 2 || len(refU) != 1 || refU[0] != 1 || len(provX) != 2 {
		t.Fatalf("pairs=%v refU=%v provX=%v", pairs, refU, provX)
	}
}

func TestRepeatedLinesPairMonotonically(t *testing.T) {
	ref := cues([]int{1000, 5000, 9000}, "chorus", "chorus", "chorus")
	prov := cues([]int{5050, 9050}, "chorus", "chorus")
	pairs, refU, _ := matchLines(ref, prov)
	if len(pairs) != 2 || pairs[0] != (pair{1, 0}) || pairs[1] != (pair{2, 1}) {
		t.Fatalf("pairs = %v; want the two nearest in order", pairs)
	}
	if len(refU) != 1 || refU[0] != 0 {
		t.Errorf("refUnmatched = %v", refU)
	}
	// Crossing is never allowed: reversed text cannot pair both.
	p2, _, _ := matchLines(cues([]int{0, 1000}, "a", "b"), cues([]int{0, 1000}, "b", "a"))
	if len(p2) != 1 {
		t.Errorf("crossing pairs = %v; want exactly 1", p2)
	}
}

func TestNoMatchesPrintsNoMAE(t *testing.T) {
	var s LineStats
	s.AddTrack(cues([]int{0}, "alpha"), cues([]int{0}, "zzz"), 10)
	if _, ok := s.MAE(); ok {
		t.Error("MAE ok with zero matches")
	}
	if _, ok := s.Within(); ok {
		t.Error("Within ok with zero matches")
	}
	if row := FormatRow("lane", s); !strings.Contains(row, "line-MAE=n/a") || strings.Contains(row, "0ms") {
		t.Errorf("row = %q", row)
	}
}

func TestBlankCuesNeverPair(t *testing.T) {
	ref := cues([]int{0, 100}, "", "  ")
	prov := cues([]int{0, 100, 200}, " ", "", "   ")
	pairs, refU, provX := matchLines(ref, prov)
	if len(pairs) != 0 || len(refU) != 2 || len(provX) != 3 {
		t.Fatalf("pairs=%v refU=%v provX=%v; want none paired", pairs, refU, provX)
	}
}

func TestAccumulatesAcrossTracks(t *testing.T) {
	var s LineStats
	s.AddTrack(cues([]int{1000}, "Alpha"), cues([]int{700}, "alpha"), 10) // early, exactly 300
	s.AddTrack(cues([]int{0, 1000, 2000, 3000}, "a", "b", "c", "d"),
		cues([]int{301, 1000, 2000}, "a", "b", "c"), 50) // 301, 0, 0; one ref unmatched
	s.AddTrack(cues([]int{0}, "a"), cues([]int{0}, "q"), 999) // no match: residual ignored
	if s.Tracks != 3 || s.RefCues != 6 || s.Matched != 4 || s.RefUnmatched != 2 || s.ProvExtra != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if mae, _ := s.MAE(); mae != 150.25 {
		t.Errorf("MAE = %v; want 150.25", mae)
	}
	if w, _ := s.Within(); w != 0.75 {
		t.Errorf("within = %v; want 0.75", w)
	}
	if mean, mx, ok := s.Residual(); !ok || mean != 40 || mx != 50 {
		t.Errorf("residual = %v,%v,%v; want 40,50,true", mean, mx, ok)
	}
	want := "lane: tracks=3 cues(ref=6 matched=4 ref-unmatched=2 provider-extra=1) line-MAE=150ms within-300ms=75.0% ref-residual(mean=40ms max=50ms) n=4"
	if got := FormatRow("lane", s); got != want {
		t.Errorf("row = %q", got)
	}
}

func words(starts []int, texts ...string) []Word {
	w := make([]Word, len(texts))
	for i, t := range texts {
		w[i] = Word{StartMS: starts[i], Text: t}
	}
	return w
}

func TestWordStarts(t *testing.T) {
	line := func(ws []Word) []Cue { return []Cue{{StartMS: 0, Text: "x", Words: ws}} }
	var s WordStats
	// Reworded word stays unmatched; the repeated word pairs with its nearest occurrence.
	s.AddTrack(line(words([]int{0, 500, 1000, 1500}, "a", "b", "a", "c")),
		line(words([]int{0, 520, 1700, 1990}, "a", "x", "a", "c")), 40)
	if s.Tracks != 1 || s.RefWords != 4 || s.Matched != 3 || s.SumAbsErrMS != 700+490 || s.WithinCount != 1 {
		t.Fatalf("stats = %+v", s)
	}
	want := "k: word-tracks=1 words(ref=4 matched=3) word-MAE=397ms within-300ms=33.3% ref-residual(mean=40ms max=40ms) n=3"
	if got := FormatWordRow("k", s); got != want {
		t.Errorf("row = %q; want %q", got, want)
	}
}

func TestWordStartsNeedWordsOnBothSides(t *testing.T) {
	var s WordStats
	ref := []Cue{{Text: "x", Words: words([]int{0}, "a")}}
	s.AddTrack(ref, []Cue{{Text: "x"}}, 10)
	s.AddTrack(ref, []Cue{{Text: "other", Words: words([]int{0}, "a")}}, 10) // line unmatched
	if s.Tracks != 0 || s.Matched != 0 || s.RefWords != 1 {
		t.Errorf("stats = %+v; want ref words counted on the matched line only", s)
	}
	if got := FormatWordRow("k", s); got != "k: word-tracks=0 words(ref=1 matched=0) word-MAE=n/a" {
		t.Errorf("row = %q", got)
	}
}

func TestWordKeysIgnoreEdgePunctuation(t *testing.T) {
	var s WordStats
	ref := []Cue{{Text: "x", Words: words([]int{0, 500, 900}, "Don't,", "(go)", "...")}}
	prov := []Cue{{Text: "x", Words: words([]int{300, 800, 1200}, "Don't", "go", "!!")}}
	s.AddTrack(ref, prov, 0)
	// "Don't," and "(go)" pair; a word that is all punctuation never pairs.
	if s.Matched != 2 || s.WithinCount != 2 {
		t.Errorf("stats = %+v; want 2 matched, both exactly 300ms off (within)", s)
	}
}

func TestUniformProviderWordsAreNotMeasured(t *testing.T) {
	var s WordStats
	ref := []Cue{{Text: "x", Words: words([]int{0, 500}, "a", "b")}}
	s.AddTrack(ref, []Cue{{Text: "x", Words: words([]int{0, 0}, "a", "b")}}, 0)
	if s.Matched != 0 || s.Tracks != 0 || s.RefWords != 2 {
		t.Errorf("stats = %+v; want line-level provider words skipped", s)
	}
	s.AddTrack(ref, []Cue{{Text: "x", Words: words([]int{0}, "a")}}, 0) // one word is honest
	if s.Matched != 1 {
		t.Errorf("stats = %+v; want a single word measured", s)
	}
}

func TestWordsSortedByStartBeforeMatching(t *testing.T) {
	var s WordStats
	ref := []Cue{{Text: "x", Words: words([]int{0, 500}, "a", "b")}}
	prov := []Cue{{Text: "x", Words: words([]int{500, 0}, "b", "a")}}
	s.AddTrack(ref, prov, 0)
	if s.Matched != 2 || s.SumAbsErrMS != 0 {
		t.Errorf("stats = %+v; want both words paired with zero error", s)
	}
}
