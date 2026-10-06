package aligner

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// suggest calls Suggest and fails the test when it returns no suggestion.
func suggest(t *testing.T, lines []string, starts []int, r Result) Suggestion {
	t.Helper()
	s, ok := Suggest(lines, starts, r)
	if !ok {
		t.Fatalf("Suggest(%q, %v) returned no suggestion", lines, starts)
	}
	return s
}

// sw is a valid word on line li starting at ms (100 ms long, confidence 0.9).
func sw(li int, text string, ms int) Word {
	return Word{Text: text, StartMS: ms, EndMS: ms + 100, LineIndex: li, Confidence: 0.9}
}

// TestSuggestLines pins the line mapping; each want is derived by hand in its comment.
func TestSuggestLines(t *testing.T) {
	three := []string{"aa", "", "bb"}
	cases := []struct {
		name   string
		lines  []string
		starts []int
		words  []Word
		want   []int
	}{
		{"first word start, absolute ms, zero is a real time", []string{"aa bb", "cc dd"}, []int{1000, 2000},
			[]Word{sw(0, "aa", 0), sw(0, "bb", 1700), sw(1, "cc", 2601), sw(1, "dd", 2900)}, []int{0, 2601}},
		// Between two aligned lines: 1500 + (2000-1000)*(3200-1500)/(3000-1000) = 1500+850.
		{"interpolated between its aligned neighbors", three, []int{1000, 2000, 3000},
			[]Word{sw(0, "aa", 1500), sw(2, "bb", 3200)}, []int{1500, 2350, 3200}},
		// Trailing, shift 0-1000: 2000-1000. Were 0 no anchor, it would keep 2000.
		{"zero anchor carries a trailing line", []string{"aa", ""}, []int{1000, 2000},
			[]Word{sw(0, "aa", 0)}, []int{0, 1000}},
		// The negative word is not used, so the line starts at bb, not at -5 or 0.
		{"negative word beside a valid one", []string{"aa bb"}, []int{1000},
			[]Word{sw(0, "aa", -5), sw(0, "bb", 1200)}, []int{1200}},
		// bb is one ms past 24 h, so not used; line 1 trails aa's +500: 2500.
		{"word past 24 hours is ignored", []string{"aa", "bb"}, []int{1000, 2000},
			[]Word{sw(0, "aa", 1500), sw(1, "bb", maxStartMS+1)}, []int{1500, 2500}},
		{"same-stamp group aligned only on a follower", []string{"", "aa", "bb"}, []int{1000, 1000, 2000},
			[]Word{sw(1, "aa", 1300), sw(2, "bb", 2500)}, []int{1300, 1300, 2500}},
		// The group's start is its first word, 1400; xx's own 1450 must not split it.
		{"same-stamp group never splits", []string{"aa", "xx", "bb"}, []int{1000, 1000, 2000},
			[]Word{sw(0, "aa", 1400), sw(1, "xx", 1450), sw(2, "bb", 2500)}, []int{1400, 1400, 2500}},
		// Shift 0: 400 is raised to aa's 500, then out of its hundredth; bb trails at 900.
		{"unsorted current start is raised, non-decreasing", three, []int{500, 400, 900},
			[]Word{sw(0, "aa", 500)}, []int{500, 510, 900}},
		// Only the last word is used: shift 1100-1000 carries the blank line to 2100.
		{"unusable words are ignored", []string{"aa", ""}, []int{1000, 2000}, []Word{
			sw(0, "aa", -5),                                     // negative start
			{Text: "aa", StartMS: 700, EndMS: 600},              // ends before it starts
			sw(5, "aa", 100), sw(-1, "aa", 100), sw(1, "zz", 9), // no such line; a blank line
			sw(0, "aa", 1100),
		}, []int{1100, 2100}},
		// Shift +86398000 would put the blank line at 24 h + 500 ms: clamped.
		{"trailing line clamps at the 24 hour bound", []string{"aa", ""}, []int{1000, 2000},
			[]Word{sw(0, "aa", maxStartMS-500)}, []int{maxStartMS - 500, maxStartMS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := suggest(t, tc.lines, tc.starts, Result{Words: tc.words}).LineMS; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LineMS = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSuggestRefuses pins the inputs that get no suggestion at all.
func TestSuggestRefuses(t *testing.T) {
	four, st := []string{"aa", "bb cc", "", "dd"}, []int{10000, 20000, 21000, 60000}
	cases := []struct {
		name   string
		lines  []string
		starts []int
		words  []Word
	}{
		{"lines and starts differ in length", []string{"aa"}, []int{1, 2}, nil},
		{"one stray early word", four, st, []Word{sw(0, "aa", 10100), sw(1, "cc", 0), sw(1, "bb", 20100), sw(3, "dd", 60000)}},
		{"one stray late word", four, st, []Word{sw(0, "aa", 90000), sw(1, "bb", 20100), sw(1, "cc", 20500), sw(3, "dd", 60000)}},
		{"matching words with decreasing times", []string{"aa bb"}, []int{900}, []Word{sw(0, "aa", 1700), sw(0, "bb", 1500)}},
		{"follower's word earlier than its leader's", []string{"aa", "xx"}, []int{1000, 1000}, []Word{sw(0, "aa", 1450), sw(1, "xx", 1400)}},
		{"equal times, lines out of order", []string{"aa", "bb"}, []int{1000, 2000}, []Word{sw(1, "bb", 1500), sw(0, "aa", 1500)}},
		{"current start past 24 hours", []string{"aa"}, []int{maxStartMS + 1}, []Word{sw(0, "aa", 1000)}},
		{"negative current start", []string{"aa"}, []int{-1}, []Word{sw(0, "aa", 1000)}},
		// Nothing aligned: the current starts are not offered back as a suggestion.
		{"no usable word", []string{"aa", ""}, []int{1000, 2000}, []Word{sw(0, "aa", -5), sw(1, "zz", 9)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if s, ok := Suggest(tc.lines, tc.starts, Result{Words: tc.words}); ok || s.LineMS != nil || s.Words != nil {
				t.Errorf("Suggest = %+v, %v; want the zero Suggestion and false", s, ok)
			}
		})
	}
}

// TestSuggestSeparation pins that lines with different current starts keep
// different stamps (hundredths) when there is room, and are counted when not.
func TestSuggestSeparation(t *testing.T) {
	pile := []string{"aa", "", "", "bb"}
	cases := []struct {
		name   string
		lines  []string
		starts []int
		words  []Word
		want   []int
		merged int
	}{
		// 10000 + (20000-10000)*(15000-10000)/(30000-10000) = 10000+2500.
		{"shift change between neighbors", []string{"aa bb", "", "cc dd"}, []int{10000, 20000, 30000},
			[]Word{sw(0, "aa", 10000), sw(0, "bb", 10400), sw(2, "cc", 15000), sw(2, "dd", 15400)}, []int{10000, 12500, 15000}, 0},
		// 1000 + 1999*2005/2000 = 3003, in bb's hundredth (300): pulled back to
		// the last ms of the hundredth before it, 2999.
		{"pulled out of the next aligned hundredth", []string{"aa", "", "bb"}, []int{1000, 2999, 3000},
			[]Word{sw(0, "aa", 1000), sw(2, "bb", 3005)}, []int{1000, 2999, 3005}, 0},
		// Both interpolate to 1000 + {1,2}*100/4000 = 1000, aa's own hundredth.
		// Each moves to the next free one: 1010, then 1020, both below bb's 1100.
		{"integer division collapse, room", pile, []int{1000, 1001, 1002, 5000},
			[]Word{sw(0, "aa", 1000), sw(3, "bb", 1100)}, []int{1000, 1010, 1020, 1100}, 0},
		// 5000 + 1000*25/3000 = 5008 and 5000 + 2000*25/3000 = 5016. Only one
		// hundredth (501) lies between 500 and 502: 5008 moves to 5010, 5016
		// cannot move to bb's 5020 and stays, sharing 501. One pair merged.
		{"no room between the anchors", pile, []int{1000, 2000, 3000, 4000},
			[]Word{sw(0, "aa", 5000), sw(3, "bb", 5025)}, []int{5000, 5010, 5016, 5025}, 1},
		// Aligned starts are never moved; 15001 and 15009 both write as 00:15.00.
		{"anchors in one hundredth", []string{"aa", "bb"}, []int{1000, 2000},
			[]Word{sw(0, "aa", 15001), sw(1, "bb", 15009)}, []int{15001, 15009}, 1},
		// Shift -4700 floors all three at 0; they take hundredths 0, 1, 2.
		{"leading pile floored at zero", []string{"", "", "", "aa"}, []int{100, 500, 900, 5000},
			[]Word{sw(3, "aa", 300)}, []int{0, 10, 20, 300}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := suggest(t, tc.lines, tc.starts, Result{Words: tc.words})
			if !reflect.DeepEqual(s.LineMS, tc.want) || s.Quality.Merged != tc.merged {
				t.Errorf("LineMS = %v merged %d, want %v merged %d", s.LineMS, s.Quality.Merged, tc.want, tc.merged)
			}
		})
	}
}

// TestSuggestProperties checks Suggest's guarantees over seeded random inputs.
// Even cases are sidecar-like (words in line order, a jittering and stepping offset,
// lines left unaligned) and must get one; odd ones are hostile: a valid one or none.
func TestSuggestProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1008, 7))
	vocab := []string{"aa", "bb", "la", "--", "x\x1cy"}
	for c := range 4000 {
		hostile := c%2 == 1
		n := 1 + rng.IntN(8)
		lines, starts := make([]string, n), make([]int, n)
		var words []Word
		at, off, last := rng.IntN(3000), rng.IntN(4000)-1000, 0
		for i := range lines {
			at += []int{0, 1 + rng.IntN(15), 200 + rng.IntN(3000)}[rng.IntN(3)]
			starts[i] = at
			if hostile && rng.IntN(6) == 0 {
				starts[i] = rng.IntN(5000)
			}
			if rng.IntN(4) == 0 {
				continue
			}
			toks := make([]string, 1+rng.IntN(3))
			for j := range toks {
				toks[j] = vocab[rng.IntN(len(vocab))]
			}
			lines[i] = strings.Join(toks, " ")
			if off += rng.IntN(41) - 20; rng.IntN(5) == 0 {
				off += rng.IntN(12000) - 6000
			}
			for j, tok := range toks {
				if tok == "--" || rng.IntN(5) == 0 {
					continue
				}
				last = max(last, at+off+j*(rng.IntN(400)))
				words = append(words, sw(i, strings.TrimSuffix(tok, "\x1cy"), last))
			}
		}
		if hostile {
			for range rng.IntN(4) {
				words = append(words, sw(rng.IntN(n+2)-1, vocab[rng.IntN(3)], []int{-7, 0, rng.IntN(9000), 1 << 62}[rng.IntN(4)]))
				j := rng.IntN(len(words))
				words[j], words[len(words)-1] = words[len(words)-1], words[j]
			}
		}
		s, ok := Suggest(lines, starts, Result{Words: words})
		if again, ok2 := Suggest(lines, starts, Result{Words: words}); ok != ok2 || !reflect.DeepEqual(s, again) {
			t.Fatalf("case %d: not deterministic", c)
		}
		fail := func(format string, a ...any) {
			t.Helper()
			t.Fatalf("case %d: "+format+"\nlines %q starts %v words %+v\ngot %+v", append(append([]any{c}, a...), lines, starts, words, s)...)
		}
		// Independent of Suggest: lead[i] leads i's same-stamp group, first[l] is
		// group l's earliest used word (-1: none), anyUsed whether any word is used.
		lead, first, anyUsed := make([]int, n), make([]int, n), false
		for i := range lead {
			lead[i], first[i] = i, -1
			if i > 0 && starts[i] == starts[i-1] {
				lead[i] = lead[i-1]
			}
		}
		for _, w := range words {
			if li := w.LineIndex; li >= 0 && li < n && lines[li] != "" && w.StartMS >= 0 && w.StartMS <= maxStartMS {
				if l := lead[li]; first[l] < 0 || w.StartMS < first[l] {
					first[l], anyUsed = w.StartMS, true
				}
			}
		}
		if !ok && ((!hostile && anyUsed) || s.LineMS != nil) {
			fail("no suggestion for a sidecar-like result with a used word, or one beside false")
		}
		if ok && !anyUsed {
			fail("a suggestion with no used word")
		}
		if ok && (len(s.LineMS) != n || len(s.Words) != n) {
			fail("%d starts and %d word lists for %d lines", len(s.LineMS), len(s.Words), n)
		}
		// least is the fewest merges any order-keeping placement can reach with the
		// aligned starts fixed: run unaligned groups need run free hundredths below
		// the first anchor, run+1 steps between two, run above the last one.
		merged, least, prevH, run := 0, 0, -1, 0
		for i, ms := range s.LineMS {
			same := i > 0 && starts[i] == starts[i-1]
			switch h := first[i] / 10; {
			case lead[i] != i:
			case first[i] < 0:
				run++
			case ms != first[i]:
				fail("line %d: aligned group starts at %d, not its first used word %d", i, ms, first[i])
			case prevH < 0:
				least, prevH, run = least+max(0, run-h), h, 0
			default:
				least, prevH, run = least+max(0, run+1-(h-prevH)), h, 0
			}
			switch {
			case ms < 0 || ms > maxStartMS:
				fail("line %d out of range", i)
			case i > 0 && ms < s.LineMS[i-1]:
				fail("line %d goes backwards", i)
			case same && ms != s.LineMS[i-1]:
				fail("line %d splits a same-stamp group", i)
			case i > 0 && !same && ms/10 == s.LineMS[i-1]/10:
				merged++
			}
			hi := maxStartMS
			for j := i + 1; j < n; j++ {
				if starts[j] != starts[j-1] {
					hi = s.LineMS[j]
					break
				}
			}
			u := s.Words[i]
			if len(u) == 1 || (len(u) > 0 && u[0].Token != 0) {
				fail("line %d: units %v", i, u)
			}
			for j, w := range u {
				if w.Token >= len(strings.Fields(lines[i])) || w.StartMS < ms || w.StartMS > hi ||
					(j > 0 && (w.Token <= u[j-1].Token || w.StartMS < u[j-1].StartMS)) {
					fail("line %d: unit %d of %v outside [%d, %d] or out of order", i, j, u, ms, hi)
				}
			}
		}
		if ok && merged != s.Quality.Merged {
			fail("Merged = %d, but %d distinct adjacent lines share a hundredth", s.Quality.Merged, merged)
		}
		if least += max(0, run-(maxStartMS/10-prevH)); ok && merged != least {
			fail("Merged = %d, but room exists for only %d merges", merged, least)
		}
	}
}

// TestSuggestWords pins which lines keep word units and what the units are.
func TestSuggestWords(t *testing.T) {
	type sws = []SuggestedWord
	cases := []struct {
		name   string
		lines  []string
		starts []int
		words  []Word
		want   [][]SuggestedWord
	}{
		// "--" has no alignable characters: it stays in the unit before it.
		{"omitted token merges into the previous unit", []string{"aa -- bb cc"}, []int{900},
			[]Word{sw(0, "aa", 1000), sw(0, "bb", 1400), sw(0, "cc", 1800)}, [][]SuggestedWord{sws{{0, 1000}, {2, 1400}, {3, 1800}}}},
		// U+001C splits a token for the sidecar but not for strings.Fields:
		// aa and bb are one field, so one unit, starting at aa.
		{"Python-only whitespace inside a field", []string{"aa\x1cbb cc"}, []int{900},
			[]Word{sw(0, "aa", 1000), sw(0, "bb", 1200), sw(0, "cc", 1500)}, [][]SuggestedWord{sws{{0, 1000}, {1, 1500}}}},
		// Two units matched (aa, cc), but zz matches nothing: no words at all.
		{"one unmatched word among three drops them all", []string{"aa bb cc"}, []int{900},
			[]Word{sw(0, "aa", 1000), sw(0, "zz", 1200), sw(0, "cc", 1400)}, [][]SuggestedWord{nil}},
		{"duplicate text binds in order", []string{"la la"}, []int{900},
			[]Word{sw(0, "la", 1100), sw(0, "la", 1300)}, [][]SuggestedWord{sws{{0, 1100}, {1, 1300}}}},
		// The blank line trails aa's shift of 0 and stays at 2000; bb at 2600 is past it.
		{"a word past the next line's start drops the line's words", []string{"aa bb", ""}, []int{1000, 2000},
			[]Word{sw(0, "aa", 1000), sw(0, "bb", 2600)}, [][]SuggestedWord{nil, nil}},
		{"a word exactly at the next line's start is kept", []string{"aa bb", "cc"}, []int{1000, 2000},
			[]Word{sw(0, "aa", 1000), sw(0, "bb", 2500), sw(1, "cc", 2500)}, [][]SuggestedWord{sws{{0, 1000}, {1, 2500}}, nil}},
		// The bound is the next DIFFERENT group's start (2500), not the follower's own 1000.
		{"bound skips same-stamp followers", []string{"aa bb", "", "cc"}, []int{1000, 1000, 2000},
			[]Word{sw(0, "aa", 1000), sw(0, "bb", 1400), sw(2, "cc", 2500)}, [][]SuggestedWord{sws{{0, 1000}, {1, 1400}}, nil, nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := suggest(t, tc.lines, tc.starts, Result{Words: tc.words}).Words; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Words = %v, want %v", got, tc.want)
			}
		})
	}
}
