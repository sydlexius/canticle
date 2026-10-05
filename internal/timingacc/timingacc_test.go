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
