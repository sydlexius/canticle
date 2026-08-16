package petitlyrics

import (
	"fmt"
	"sort"
	"strings"
	"testing"

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

// tokenCounts splits s into whitespace-delimited tokens after running each
// through normalize.NormalizeKey (NFKD fold, strip combining marks, NFC,
// lowercase), and returns a frequency map. Normalizing per-token rather than
// the whole body first keeps punctuation-adjacent words comparable (e.g. a
// trailing comma is part of the pre-split token either way, so both sides are
// affected identically).
func tokenCounts(s string) map[string]int {
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

	// Case and diacritic folding: NormalizeKey must make these agree.
	folded := scoreAgreement("LOREM IPSUM", "lorem ipsum")
	if folded != 1.0 {
		t.Errorf("scoreAgreement case-folding = %v, want 1.0", folded)
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
