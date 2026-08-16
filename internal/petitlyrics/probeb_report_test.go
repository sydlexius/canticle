package petitlyrics

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/sydlexius/canticle/internal/langguard"
	"github.com/sydlexius/canticle/internal/normalize"
)

// This file implements the aggregation and pure logic for Probe B (issue
// #614): does petitlyrics isOfficial=1 agree with the already-trusted sidecar
// text at a materially higher rate than isOfficial=0, WITHIN a popularity
// band. It is throwaway measurement code, never linked into the shipped
// binary, so it lives entirely in _test.go files -- the same discipline
// probe_report_test.go and probe_survey_test.go already establish for Probe
// A / the original survey probe.
//
// The live network-touching half lives in probeb_survey_test.go, gated on
// PLPROBEB=1. Everything in THIS file is pure and runs on every `go test`.

// bandCount is the number of popularity terciles the sample is stratified
// into. Fixed at 3 (bottom/middle/top) per the issue's explicit design: the
// comparison must never be pooled across the whole sample, because isOfficial
// may correlate with popularity rather than with care.
const bandCount = 3

// bandTracksByArtist assigns each artist_key to a popularity band (0 = bottom
// third, 1 = middle third, 2 = top third) based on how many tracks that artist
// has in THIS library, per artistTrackCounts.
//
// This is a proxy for real-world popularity, not a measurement of it: it
// measures this library's collecting pattern (how many tracks by an artist
// the collector happened to acquire), which correlates with but is not the
// same as mainstream popularity. State that limitation wherever the bands are
// reported, not just here.
//
// Banding is done by ARTIST, not by track: the sorted list of distinct
// artist_keys is split into three equal-sized groups by count. Every track by
// a given artist inherits that artist's band, so a prolific artist's tracks
// do not each get an independent bucket assignment.
func bandTracksByArtist(artistTrackCounts map[string]int) map[string]int {
	type kv struct {
		key   string
		count int
	}
	sorted := make([]kv, 0, len(artistTrackCounts))
	for k, c := range artistTrackCounts {
		sorted = append(sorted, kv{k, c})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count < sorted[j].count
		}
		// Stable tie-break on the key itself so the assignment is
		// deterministic across runs for a fixed input, independent of map
		// iteration order.
		return sorted[i].key < sorted[j].key
	})

	band := make(map[string]int, len(sorted))
	n := len(sorted)
	if n == 0 {
		return band
	}
	for i, e := range sorted {
		// Split index range [0,n) into bandCount groups by position, not by
		// value: this guarantees every band gets floor(n/3) or ceil(n/3)
		// artists even when many artists share the same track count (a
		// library dominated by 1-track artists would otherwise pile the
		// whole population into one band under a value-based split).
		b := i * bandCount / n
		if b >= bandCount {
			b = bandCount - 1
		}
		band[e.key] = b
	}
	return band
}

// bandLabel renders a band index as the human-readable name used in reports.
func bandLabel(band int) string {
	switch band {
	case 0:
		return "bottom third"
	case 1:
		return "middle third"
	case 2:
		return "top third"
	default:
		return fmt.Sprintf("band %d", band)
	}
}

// scoreAgreement measures textual agreement between two lyric bodies as a
// token-multiset Dice coefficient in [0.0, 1.0]: 1.0 means the same
// (normalized) words in the same proportions, 0.0 means no shared tokens.
//
// This is a TEXT-agreement score, not a correctness score -- per the issue,
// "it measures agreement with the trusted source, not correctness. If both
// sources carry the same error it scores as perfect agreement." That is
// accepted, because the accept rule Probe B is evaluating is exactly "does
// this match the text I already trust".
//
// Token-bag Dice rather than a line-by-line or edit-distance comparison,
// because the two sides can come from different tiers with different line
// segmentation (an unsynced .txt trusted sidecar vs a word-synced petitlyrics
// result whose cue boundaries need not match): comparing the multiset of
// normalized words is robust to re-wrapping and re-punctuation that a
// line-oriented comparison would misread as disagreement.
func scoreAgreement(trusted, candidate string) float64 {
	a := tokenCounts(trusted)
	b := tokenCounts(candidate)
	lenA, lenB := 0, 0
	for _, n := range a {
		lenA += n
	}
	for _, n := range b {
		lenB += n
	}
	if lenA == 0 || lenB == 0 {
		return 0
	}
	overlap := 0
	for tok, na := range a {
		if nb := b[tok]; nb > 0 {
			overlap += min(na, nb)
		}
	}
	return 2 * float64(overlap) / float64(lenA+lenB)
}

// tokenCounts reduces s to a frequency map of comparable units.
//
// TWO TOKENIZERS, chosen per text, because one does not fit both scripts. This
// is the correction CodeRabbit caught on PR #772: the original whitespace-only
// split made each Japanese LINE a single token, so two texts differing only by
// where the line breaks fall scored 0.000 rather than 1.000. Measured, with a
// Latin control at 1.000 on the same experiment -- so the defect was specific to
// scripts with no whitespace word boundaries.
//
// That is not a corner case here. petitlyrics is a Japanese provider, and CJK
// material is a large part of what this lane serves, so Probe B would have been
// measuring line-wrapping differences instead of isOfficial agreement on exactly
// the population it exists to say something about.
//
//   - Whitespace-delimited text (Latin and friends): word tokens, as before.
//     Words are the meaningful unit and keep the score interpretable.
//   - Scripts without whitespace word boundaries (Han, Kana, Hangul): character
//     BIGRAMS over the whitespace-stripped body. Bigrams are segmentation-
//     invariant -- re-wrapping a line cannot change which adjacent character
//     pairs exist -- without needing a morphological analyzer, which would be a
//     large dependency for a throwaway measurement harness.
//
// Each unit is normalized through normalize.NormalizeKey (NFKD fold, strip
// combining marks, NFC, lowercase) so case and diacritic differences do not read
// as disagreement.
func tokenCounts(s string) map[string]int {
	if usesCharacterSegmentation(s) {
		return bigramCounts(s)
	}
	counts := map[string]int{}
	for tok := range strings.FieldsSeq(s) {
		key := normalize.NormalizeKey(tok)
		if key == "" {
			continue
		}
		counts[key]++
	}
	return counts
}

// usesCharacterSegmentation reports whether s is predominantly written in a
// script that does not delimit words with whitespace.
//
// Threshold rather than "any": a CJK lyric commonly carries a Latin title line
// or a romanized credit, and a single Latin word must not flip the whole body to
// word tokens. Judged over LETTERS only -- ScriptOf returns "" for punctuation,
// digits and whitespace, which are not evidence either way.
func usesCharacterSegmentation(s string) bool {
	var letters, charSeg int
	for _, r := range s {
		switch langguard.ScriptOf(r) {
		case "":
			continue
		case langguard.Han, langguard.Kana, langguard.Hangul:
			charSeg++
		}
		letters++
	}
	if letters == 0 {
		return false
	}
	return float64(charSeg)/float64(letters) >= charSegmentationThreshold
}

// charSegmentationThreshold is the share of letters that must belong to a
// character-segmented script before the bigram tokenizer is used. Half is
// deliberately permissive: a mixed body is better served by bigrams (which still
// capture Latin runs, just more finely) than by word tokens (which collapse an
// entire CJK line into one unmatched unit).
const charSegmentationThreshold = 0.5

// bigramCounts returns a frequency map of adjacent character pairs over the
// whitespace-stripped, normalized body.
//
// Whitespace is removed BEFORE pairing, which is what makes the result
// segmentation-invariant: the same characters in the same order yield the same
// bigrams no matter where lines break. A single-character body has no pairs, so
// it falls back to that one character, which keeps a one-word line comparable
// rather than scoring 0 against everything.
func bigramCounts(s string) map[string]int {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	runes := []rune(normalize.NormalizeKey(b.String()))
	counts := map[string]int{}
	if len(runes) == 0 {
		return counts
	}
	if len(runes) == 1 {
		counts[string(runes)]++
		return counts
	}
	for i := 0; i+1 < len(runes); i++ {
		counts[string(runes[i:i+2])]++
	}
	return counts
}

// scoreBucket classifies a score into a coarse, aggregate-safe label. Reported
// alongside the mean/median so a reader can see the shape of the distribution
// without needing every raw float.
func scoreBucket(score float64) string {
	switch {
	case score >= 0.8:
		return "high (>=0.8)"
	case score >= 0.5:
		return "medium (>=0.5)"
	default:
		return "low (<0.5)"
	}
}

// probeBObservation is one sampled track's worth of data. Like
// sampleObservation in probe_report_test.go, it deliberately carries no
// title, artist, album, or lyric text -- the report generator can only print
// what it is given, so the privacy guarantee is structural.
type probeBObservation struct {
	Band int // popularity tercile, 0 (bottom) to bandCount-1 (top)
	// IsOfficial is PRINTED by render() after passing through boundedToken,
	// exactly like the original survey probe's field of the same name and for
	// the same reason: a short enumerated token prints verbatim, anything
	// longer becomes a length-only placeholder.
	IsOfficial string
	// Score is the token-agreement Dice coefficient in [0,1], valid only when
	// Scored is true.
	Score  float64
	Scored bool
	// Err is the sentinel error class for a sample that could not be scored:
	// no sidecar, an unreadable sidecar, a provider miss, or a transport
	// error. Empty means Score is valid.
	Err string
}

// probeBReport aggregates observations into the shareable, aggregate-only
// artifact.
type probeBReport struct {
	total     int
	errCounts map[string]int

	// cells is keyed by (band, boundedToken(isOfficial)). Each cell tracks a
	// count and the running scores so render() can compute mean/median and a
	// bucket histogram per cell.
	cells map[cellKey]*cell
}

type cellKey struct {
	band       int
	isOfficial string
}

type cell struct {
	n       int
	scores  []float64
	buckets map[string]int
}

func newProbeBReport() *probeBReport {
	return &probeBReport{
		errCounts: map[string]int{},
		cells:     map[cellKey]*cell{},
	}
}

func (r *probeBReport) add(obs probeBObservation) {
	r.total++
	if obs.Err != "" {
		r.errCounts[boundedSentinel(obs.Err)]++
		return
	}
	if !obs.Scored {
		return
	}
	key := cellKey{band: obs.Band, isOfficial: boundedToken(obs.IsOfficial)}
	c := r.cells[key]
	if c == nil {
		c = &cell{buckets: map[string]int{}}
		r.cells[key] = c
	}
	c.n++
	c.scores = append(c.scores, obs.Score)
	c.buckets[scoreBucket(obs.Score)]++
}

func (r *probeBReport) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "probe B: petitlyrics isOfficial agreement, by popularity band\n")
	fmt.Fprintf(&b, "samples: %d\n", r.total)

	fmt.Fprintf(&b, "\nerrors:\n")
	if len(r.errCounts) == 0 {
		fmt.Fprintf(&b, "  none\n")
	}
	for _, k := range sortedKeys(r.errCounts) {
		fmt.Fprintf(&b, "  %s: %d\n", k, r.errCounts[k])
	}

	fmt.Fprintf(&b, "\nNOTE: band is a proxy for popularity (track count per artist_key\n"+
		"in THIS library), not a measurement of real-world popularity. It reflects\n"+
		"this library's collecting pattern.\n")

	for band := range bandCount {
		fmt.Fprintf(&b, "\n%s:\n", bandLabel(band))
		officials := cellsForBand(r.cells, band)
		if len(officials) == 0 {
			fmt.Fprintf(&b, "  no scored samples\n")
			continue
		}
		for _, official := range officials {
			c := r.cells[cellKey{band: band, isOfficial: official}]
			mean := meanOf(c.scores)
			median := medianOf(c.scores)
			fmt.Fprintf(&b, "  isOfficial=%q: n=%d  mean=%.3f  median=%.3f\n", official, c.n, mean, median)
			for _, bucket := range []string{"high (>=0.8)", "medium (>=0.5)", "low (<0.5)"} {
				if n := c.buckets[bucket]; n > 0 {
					fmt.Fprintf(&b, "    %s: %d\n", bucket, n)
				}
			}
		}
	}

	return b.String()
}

func cellsForBand(cells map[cellKey]*cell, band int) []string {
	var out []string
	for k := range cells {
		if k.band == band {
			out = append(out, k.isOfficial)
		}
	}
	sort.Strings(out)
	return out
}

func meanOf(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func medianOf(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vs...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

// TestProbeBReport_EmitsNoIdentifyingContent is the privacy guarantee made
// testable, mirroring TestSurveyReport_EmitsNoIdentifyingContent exactly: the
// only free-form, provider-controlled field is IsOfficial, and it must pass
// through boundedToken before it can reach render().
func TestProbeBReport_EmitsNoIdentifyingContent(t *testing.T) {
	const canary = "CANARY-IDENTIFYING-CONTENT"

	r := newProbeBReport()
	r.add(probeBObservation{Band: 0, IsOfficial: "1", Score: 0.9, Scored: true})
	r.add(probeBObservation{Band: 1, IsOfficial: canary + "-OFFICIAL", Score: 0.5, Scored: true})
	r.add(probeBObservation{Err: "ErrNotFound"})

	out := r.render()

	if strings.Contains(out, canary) {
		t.Errorf("report leaked a free-form field value into its output:\n%s", out)
	}
	if !strings.Contains(out, "samples: 3") {
		t.Errorf("report did not count every sample:\n%s", out)
	}
	if !strings.Contains(out, `isOfficial="1"`) {
		t.Errorf("a short enumerated token was not printed verbatim:\n%s", out)
	}
	if !strings.Contains(out, "ErrNotFound: 1") {
		t.Errorf("report did not record the error class:\n%s", out)
	}
}

// TestProbeBReport_BandsAreNeverPooled pins the one design property the
// control depends on: the report must present bottom/middle/top separately,
// never a single pooled isOfficial=0-vs-1 number across the whole sample. A
// pooled number is exactly what would make isOfficial look discriminating
// even when it is merely tracking popularity.
func TestProbeBReport_BandsAreNeverPooled(t *testing.T) {
	r := newProbeBReport()
	r.add(probeBObservation{Band: 0, IsOfficial: "0", Score: 0.9, Scored: true})
	r.add(probeBObservation{Band: 2, IsOfficial: "1", Score: 0.9, Scored: true})

	out := r.render()
	for _, want := range []string{"bottom third:", "middle third:", "top third:"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not render a separate section for %q:\n%s", want, out)
		}
	}
	// The middle band has no samples and must say so rather than silently
	// omitting the section, which would look like the band was never
	// considered instead of legitimately empty.
	if !strings.Contains(out, "middle third:\n  no scored samples") {
		t.Errorf("empty band did not render its own no-data line:\n%s", out)
	}
}

// TestBandTracksByArtist_SplitsIntoEqualThirdsByPosition pins the banding
// rule: artists sorted by track count, split by POSITION in that sorted list
// into three groups, not by count value. This matters when many artists share
// the same count (e.g. many 1-track artists), which a value-based split would
// dump entirely into one band.
func TestBandTracksByArtist_SplitsIntoEqualThirdsByPosition(t *testing.T) {
	counts := map[string]int{
		"a1": 1, "a2": 1, "a3": 1, // 3 artists tied at 1 track
		"b1": 5, "b2": 5, "b3": 5, // 3 tied at 5
		"c1": 20, "c2": 20, "c3": 20, // 3 tied at 20
	}
	bands := bandTracksByArtist(counts)
	if len(bands) != 9 {
		t.Fatalf("bands has %d entries, want 9", len(bands))
	}

	counted := map[int]int{}
	for _, b := range bands {
		counted[b]++
	}
	for b := range bandCount {
		if counted[b] != 3 {
			t.Errorf("band %d has %d artists, want 3 (positional split): %v", b, counted[b], bands)
		}
	}

	// The lowest-count artists must land in band 0, the highest in the last
	// band; the tie-break within a count is deterministic on the key.
	if bands["a1"] != 0 || bands["a2"] != 0 || bands["a3"] != 0 {
		t.Errorf("lowest-count artists not in band 0: %v", bands)
	}
	if bands["c1"] != bandCount-1 || bands["c2"] != bandCount-1 || bands["c3"] != bandCount-1 {
		t.Errorf("highest-count artists not in the top band: %v", bands)
	}
}

// TestBandTracksByArtist_Empty pins the zero-artist edge case: no division by
// zero, empty result.
func TestBandTracksByArtist_Empty(t *testing.T) {
	if got := bandTracksByArtist(nil); len(got) != 0 {
		t.Errorf("bandTracksByArtist(nil) = %v, want empty", got)
	}
}

// TestScoreAgreement pins the arithmetic the whole measurement rests on: a
// Dice coefficient over normalized token multisets. Fabricated Latin filler
// text throughout -- no real lyric content anywhere in this test.
func TestScoreAgreement(t *testing.T) {
	cases := []struct {
		name               string
		trusted, candidate string
		want               float64
	}{
		{"identical", "lorem ipsum dolor sit amet", "lorem ipsum dolor sit amet", 1.0},
		{"disjoint", "lorem ipsum dolor", "consectetur adipiscing elit", 0.0},
		{"both empty", "", "", 0.0},
		{"one empty", "lorem ipsum", "", 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreAgreement(tc.trusted, tc.candidate); got != tc.want {
				t.Errorf("scoreAgreement(%q, %q) = %v, want %v", tc.trusted, tc.candidate, got, tc.want)
			}
		})
	}

	// Partial overlap: 2 of 4 tokens shared each side.
	// overlap = 2 (lorem, ipsum); lenA=4, lenB=4; dice = 2*2/8 = 0.5
	partial := scoreAgreement("lorem ipsum dolor sit", "lorem ipsum consectetur adipiscing")
	if partial != 0.5 {
		t.Errorf("scoreAgreement partial overlap = %v, want 0.5", partial)
	}

	// Case folding: NormalizeKey must make these agree.
	folded := scoreAgreement("LOREM IPSUM", "lorem ipsum")
	if folded != 1.0 {
		t.Errorf("scoreAgreement case-folding = %v, want 1.0", folded)
	}

	// DIACRITIC folding, which the case above does not exercise despite its
	// original label -- caught in review on PR #772. NormalizeKey runs an NFKD
	// fold and strips combining marks, so an accented and unaccented spelling of
	// the same word must agree. Without this, removing that normalization step
	// passes the whole suite.
	diacritic := scoreAgreement("café", "cafe")
	if diacritic != 1.0 {
		t.Errorf("scoreAgreement diacritic folding = %v, want 1.0", diacritic)
	}

	// TOKEN MULTIPLICITY. Every other overlap case uses distinct tokens, so a
	// regression turning the multiset overlap into a set overlap would pass them
	// all.
	//
	// The shared token must REPEAT ON BOTH SIDES to discriminate, which is easy
	// to get wrong: an earlier version of this case used "lorem lorem" vs
	// "lorem ipsum", where min(2,1) and count-the-type-once both equal 1, so the
	// mutation scored identically and the test passed it. Verified by actually
	// applying the mutation rather than by reasoning about it.
	//
	// Here the multiset overlap is min(2,2)=2 over lengths 2 and 2, giving
	// exactly 1.0; counting the shared type once gives 2*1/4 = 0.5. Both are
	// exact binary floats, so the comparison needs no tolerance.
	multiplicity := scoreAgreement("lorem lorem", "lorem lorem")
	if multiplicity != 1.0 {
		t.Errorf("scoreAgreement token multiplicity = %v, want 1.0 "+
			"(a set-based overlap would give 0.5)", multiplicity)
	}
}

// TestScoreAgreement_SegmentationInvariance is the regression test for the
// scoring defect CodeRabbit caught on PR #772.
//
// Whitespace tokenization made each Japanese LINE a single token, so two texts
// differing only by where the line breaks fall scored 0.000 instead of 1.000.
// petitlyrics is a Japanese provider, so Probe B would have measured line
// wrapping rather than isOfficial agreement on much of what this lane serves.
//
// The Latin cases are the CONTROL, and they are what make the CJK results
// meaningful: they prove the word tokenizer is still in use where whitespace
// genuinely delimits words, so the fix is script-aware rather than a blanket
// change that scores everything as agreeing.
//
// All fixtures are placeholder text -- kana syllabary and lorem ipsum -- never
// real lyric content.
func TestScoreAgreement_SegmentationInvariance(t *testing.T) {
	cases := []struct {
		name               string
		trusted, candidate string
		want               float64
	}{
		// THE REGRESSION: identical characters, different line breaks.
		{"kana rewrapped", "あいう\nえお", "あいうえお", 1.0},
		{"kana rewrapped, more breaks", "あ\nいう\nえお", "あいうえお", 1.0},
		// Genuine disagreement must still score 0, or the fix would be
		// "everything agrees", which measures nothing.
		{"kana disjoint", "あいうえお", "かきくけこ", 0.0},
		// CONTROL: whitespace-delimited text keeps word tokenization.
		{"latin rewrapped", "lorem ipsum\ndolor", "lorem ipsum dolor", 1.0},
		{"latin disjoint", "lorem ipsum", "consectetur adipiscing", 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreAgreement(tc.trusted, tc.candidate); got != tc.want {
				t.Errorf("scoreAgreement = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUsesCharacterSegmentation pins the tokenizer-selection rule, including the
// mixed-script case the threshold exists for: a CJK lyric commonly carries a
// Latin title or romanized credit, and a few Latin words must not flip the whole
// body back to word tokens.
func TestUsesCharacterSegmentation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"pure latin", "lorem ipsum dolor", false},
		{"pure kana", "あいうえお", true},
		{"kana with a latin credit line", "あいうえお\nlorem", true},
		{"latin with one kana word", "lorem ipsum dolor sit amet あ", false},
		{"empty", "", false},
		{"punctuation and digits only", "12:34 -- !!", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesCharacterSegmentation(tc.in); got != tc.want {
				t.Errorf("usesCharacterSegmentation(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestScoreAgreement_MutationGuard is the mutation-verification case: it pins
// a specific nonzero, non-one value (0.5) so a mutant that collapses the
// scorer to always return 0.0 or always return 1.0 is caught, which neither
// the "identical" nor the "disjoint" case alone would necessarily catch if
// the two failure modes happened to coincide with those particular inputs.
func TestScoreAgreement_MutationGuard(t *testing.T) {
	got := scoreAgreement("lorem ipsum dolor sit amet consectetur", "lorem ipsum dolor adipiscing elit sed")
	// overlap = 3 (lorem, ipsum, dolor); lenA=6, lenB=6; dice = 2*3/12 = 0.5
	if got != 0.5 {
		t.Errorf("scoreAgreement = %v, want 0.5", got)
	}
}

// TestQuotaByBand is the regression test for the sampling bias CodeRabbit caught
// on PR #772.
//
// Rows are per-TRACK, so a global shuffle lets an artist with more completed
// tracks contribute proportionally more of them -- and the top band is DEFINED by
// having more tracks, so it crowds out the bottom. The bottom band is exactly the
// population this probe must speak about, since the question is whether
// isOfficial discriminates WITHIN a band rather than merely tracking popularity.
func TestQuotaByBand(t *testing.T) {
	// A deliberately lopsided pool: the top band has far more rows than the
	// bottom, which is what a real library looks like and what biases a global
	// shuffle.
	var all []probeBRow
	bands := map[string]int{}
	add := func(artist string, band, count int) {
		bands[artist] = band
		for i := 0; i < count; i++ {
			all = append(all, probeBRow{artistKey: artist})
		}
	}
	add("bottom", 0, 5)
	add("middle", 1, 20)
	add("top", 2, 100)

	got := quotaByBand(all, bands, 12)
	if len(got) != 12 {
		t.Fatalf("sampled %d rows, want 12", len(got))
	}

	counts := map[int]int{}
	for _, r := range got {
		counts[bands[r.artistKey]]++
	}
	// Even quota: 12 across 3 bands is 4 each, and every band has at least 4
	// available.
	for b := range bandCount {
		if counts[b] != 4 {
			t.Errorf("band %d got %d rows, want 4 (an even quota); counts=%v", b, counts[b], counts)
		}
	}
}

// TestQuotaByBandRedistributesShortage covers the case a naive fixed quota gets
// wrong: a band with fewer rows than its share must not leave the sample short,
// and must not silently drop the request either.
func TestQuotaByBandRedistributesShortage(t *testing.T) {
	var all []probeBRow
	bands := map[string]int{}
	add := func(artist string, band, count int) {
		bands[artist] = band
		for i := 0; i < count; i++ {
			all = append(all, probeBRow{artistKey: artist})
		}
	}
	// The bottom band can supply only 2 of its 5-row share.
	add("bottom", 0, 2)
	add("middle", 1, 50)
	add("top", 2, 50)

	got := quotaByBand(all, bands, 15)
	if len(got) != 15 {
		t.Fatalf("sampled %d rows, want 15 -- a short band must be redistributed, not left as a gap", len(got))
	}
	counts := map[int]int{}
	for _, r := range got {
		counts[bands[r.artistKey]]++
	}
	if counts[0] != 2 {
		t.Errorf("bottom band got %d rows, want all 2 it had; counts=%v", counts[0], counts)
	}
	if counts[1]+counts[2] != 13 {
		t.Errorf("the remaining bands got %d rows, want 13; counts=%v", counts[1]+counts[2], counts)
	}
}

// TestQuotaByBandSmallPool asserts the degenerate cases return everything rather
// than erroring or looping: a pool at or below the requested size needs no
// quota at all.
func TestQuotaByBandSmallPool(t *testing.T) {
	bands := map[string]int{"a": 0, "b": 2}
	all := []probeBRow{{artistKey: "a"}, {artistKey: "b"}}

	if got := quotaByBand(all, bands, 10); len(got) != 2 {
		t.Errorf("pool smaller than n returned %d rows, want all 2", len(got))
	}
	if got := quotaByBand(nil, bands, 10); got != nil {
		t.Errorf("empty pool returned %v, want nil", got)
	}
	if got := quotaByBand(all, bands, 0); got != nil {
		t.Errorf("n=0 returned %v, want nil", got)
	}
}
