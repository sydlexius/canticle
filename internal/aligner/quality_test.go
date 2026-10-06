package aligner

import (
	"math"
	"reflect"
	"testing"
)

// TestSuggestQuality works one result by hand:
//   - tokens: line 0 has 4, the blank line 0, line 2 has 4 = 8; aligned 4,
//     so coverage is 4/8 = 0.5.
//   - confidences 1.5, -2, NaN, 0.5 count as 1, 0, 0, 0.5: 1.5/4 = 0.375.
//     The word on the blank line (confidence 1) is not used, so not counted.
//   - the transcript's distinct tokens are aa bb zz qq, and aa and bb are in
//     the sent lines: 2/4 = 0.5.
func TestSuggestQuality(t *testing.T) {
	lines := []string{"aa bb cc dd", "", "ee ff gg hh"}
	words := []Word{
		{Text: "aa", StartMS: 100, EndMS: 200, Confidence: 1.5},
		{Text: "bb", StartMS: 300, EndMS: 400, Confidence: -2},
		{Text: "cc", StartMS: 500, EndMS: 600, Confidence: math.NaN()},
		{Text: "dd", StartMS: 700, EndMS: 800, Confidence: 0.5},
		{Text: "zz", StartMS: 900, EndMS: 950, LineIndex: 1, Confidence: 1},
	}
	got := suggest(t, lines, []int{0, 1000, 2000}, Result{Words: words, Transcript: "aa bb zz qq"}).Quality
	want := Quality{Similarity: 0.5, HasTranscript: true, MeanConfidence: 0.375, Coverage: 0.5, Tokens: 8, AlignedTokens: 4}
	if got != want {
		t.Errorf("Quality = %+v, want %+v", got, want)
	}
	if w := got.Warnings(); !reflect.DeepEqual(w, []string{"confidence", "coverage"}) {
		t.Errorf("Warnings = %v, want [confidence coverage]", w)
	}

	// One used word matching no token at confidence 0, no transcript: every
	// measure is 0, never NaN. (No used word at all is no suggestion.)
	empty := suggest(t, lines, []int{0, 1000, 2000}, Result{Words: []Word{{Text: "zz", StartMS: 100, EndMS: 200}}}).Quality
	if want := (Quality{Tokens: 8}); empty != want {
		t.Errorf("empty Quality = %+v, want %+v", empty, want)
	}

	// The sidecar splits "bb\x1ccc ee" into 3 tokens (strings.Fields sees 2):
	// 5 of 5 aligned. Lines are joined with a space, so the transcript's "aabb"
	// spans the two lines and matches nothing while dd matches: 1/2.
	got = suggest(t, []string{"dd aa", "bb\x1ccc ee"}, []int{0, 1000}, Result{Transcript: "aabb dd", Words: []Word{
		sw(0, "dd", 100), sw(0, "aa", 300), sw(1, "bb", 1000), sw(1, "cc", 1200), sw(1, "ee", 1400),
	}}).Quality
	if want := (Quality{Similarity: 0.5, HasTranscript: true, MeanConfidence: 0.9, Coverage: 1, Tokens: 5, AlignedTokens: 5}); got != want {
		t.Errorf("Quality = %+v, want %+v", got, want)
	}

	// A good result whose transcript is empty (or has no letter or digit):
	// similarity is undefined, reported as such, and not warned on.
	for _, tr := range []string{"", " ... "} {
		q := suggest(t, []string{"aa"}, []int{0}, Result{Words: []Word{sw(0, "aa", 100)}, Transcript: tr}).Quality
		if q.HasTranscript || q.Similarity != 0 || len(q.Warnings()) != 0 {
			t.Errorf("transcript %q: Quality = %+v, Warnings %v; want no transcript and no warning", tr, q, q.Warnings())
		}
	}
}

func TestQualityWarnings(t *testing.T) {
	all := []string{"similarity", "confidence", "coverage", "merged"}
	cases := []struct {
		name string
		q    Quality
		want []string
	}{
		{"exactly at each threshold does not warn", Quality{Similarity: 0.35, HasTranscript: true, MeanConfidence: 0.50, Coverage: 0.80}, []string{}},
		{"just under each threshold warns, in order", Quality{Similarity: 0.349, HasTranscript: true, MeanConfidence: 0.499, Coverage: 0.799, Merged: 1}, all},
		{"zero value: similarity is undefined, the rest warn", Quality{}, all[1:3]},
		{"NaN warns", Quality{Similarity: math.NaN(), HasTranscript: true, MeanConfidence: 1, Coverage: 1}, all[:1]},
		{"only merged", Quality{MeanConfidence: 1, Coverage: 1, Merged: 2}, all[3:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.Warnings(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Warnings = %v, want %v", got, tc.want)
			}
		})
	}
}
