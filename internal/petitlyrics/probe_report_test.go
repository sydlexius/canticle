package petitlyrics

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// sampleObservation is one track's worth of survey data. It deliberately carries
// NO title, artist, album, or lyric text: the report generator can only print
// what it is given, so the privacy guarantee is structural rather than a
// redaction pass that could fail open.
type sampleObservation struct {
	Tier          int // classifyPayload result, 0 when the lookup failed
	AvailableTier int // availableLyricsType as reported, 0 when absent
	CueCount      int // number of cues decoded, 0 when none
	// IsOfficial is PRINTED by render(), unlike Copyright which is only counted.
	// It passes through boundedToken first, so a short enumerated token (the
	// observed "0"/"1") prints verbatim while anything longer is replaced by a
	// length-only placeholder. That keeps the measurement intact -- printing the
	// real values is how the inverted isOfficial correlation was found -- without
	// leaving a provider-controlled string able to reach a shared surface.
	IsOfficial    string  // raw isOfficial value, empty when absent
	Copyright     string  // raw copyright value, empty when absent
	DistinctRatio float64 // tier-3 only: fraction of lines with distinct word starts
	Err           string  // sentinel error class name, empty on success
	// Paired and PairErr are tier-2 only, and record whether the companion
	// tier-1 text was captured beside the blob (#602). An UNPAIRED blob is not
	// useless -- the first corpus was four of them -- but it cannot supply the
	// ground truth needed to pin the XOR key's low byte or the timestamp offset,
	// so the report counts the two separately rather than reporting a corpus size
	// that overstates how much of it is actually usable.
	Paired  bool
	PairErr string // why the companion fetch failed, empty when it succeeded
}

type surveyReport struct {
	total          int
	tierCounts     map[int]int
	availTierAgree int
	availTierSeen  int
	officialCounts map[string]int
	officialByTier map[int]map[string]int
	copyrightSeen  int
	cueCounts      []int
	lsyPaired      int
	lsyUnpaired    map[string]int
	distinctRatios []float64
	errCounts      map[string]int
}

func newSurveyReport() *surveyReport {
	return &surveyReport{
		tierCounts:     map[int]int{},
		officialCounts: map[string]int{},
		officialByTier: map[int]map[string]int{},
		errCounts:      map[string]int{},
		lsyUnpaired:    map[string]int{},
	}
}

func (r *surveyReport) add(obs sampleObservation) {
	r.total++
	if obs.Err != "" {
		r.errCounts[obs.Err]++
		// A sample can carry BOTH an error and a known tier: the wsy-decode and
		// lsy-write paths classify the payload before they fail. Those still count
		// toward the tier distribution, because Q2 asks what the provider OFFERS,
		// not what we managed to decode. Returning early here would silently
		// undercount word-sync coverage by exactly the tracks that are hardest to
		// decode.
		if obs.Tier != 0 {
			r.tierCounts[obs.Tier]++
		}
		// A tier-2 sample counted just above is part of the corpus TOTAL, so it
		// must also land in the paired/unpaired breakdown or the two numbers
		// cannot be reconciled ("0 paired / 1 total" with no reason given). It
		// reached here by failing AFTER classifying its payload -- lsy-write --
		// so it is definitionally unpaired, and obs.Err is the reason. Recorded
		// BEFORE the early return, which is the whole bug: the return skips the
		// breakdown at the bottom of this function.
		if obs.Tier == tierLineSync {
			r.lsyUnpaired[boundedSentinel(obs.Err)]++
		}
		return
	}
	r.tierCounts[obs.Tier]++
	if obs.AvailableTier != 0 {
		r.availTierSeen++
		if obs.AvailableTier == obs.Tier {
			r.availTierAgree++
		}
	}
	if obs.IsOfficial != "" {
		r.officialCounts[boundedToken(obs.IsOfficial)]++
		if r.officialByTier[obs.Tier] == nil {
			r.officialByTier[obs.Tier] = map[string]int{}
		}
		r.officialByTier[obs.Tier][boundedToken(obs.IsOfficial)]++
	}
	if obs.Copyright != "" {
		r.copyrightSeen++
	}
	// A cue count is aggregate-safe (a number, never a line of text) and sizes the
	// future writer work. Zero means nothing was decoded, so it carries no
	// distribution information and is left out.
	if obs.CueCount > 0 {
		r.cueCounts = append(r.cueCounts, obs.CueCount)
	}
	if obs.Tier == tierWordSync {
		r.distinctRatios = append(r.distinctRatios, obs.DistinctRatio)
	}
	// The #602 corpus is sized by PAIRED blobs, not by blobs. An unpaired one
	// cannot supply ground truth, so counting only the total would report a
	// corpus that looks adequate while leaving the key's low byte and the
	// timestamp offset exactly as unresolvable as before.
	if obs.Tier == tierLineSync {
		if obs.Paired {
			r.lsyPaired++
		} else {
			reason := obs.PairErr
			if reason == "" {
				reason = "unknown"
			}
			r.lsyUnpaired[boundedSentinel(reason)]++
		}
	}
}

func (r *surveyReport) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "samples: %d\n", r.total)

	fmt.Fprintf(&b, "\ntier distribution:\n")
	for _, tier := range []int{tierUnsynced, tierLineSync, tierWordSync} {
		fmt.Fprintf(&b, "  tier %d: %d\n", tier, r.tierCounts[tier])
	}

	fmt.Fprintf(&b, "\nerrors:\n")
	if len(r.errCounts) == 0 {
		fmt.Fprintf(&b, "  none\n")
	}
	for _, k := range sortedKeys(r.errCounts) {
		fmt.Fprintf(&b, "  %s: %d\n", k, r.errCounts[k])
	}

	// The denominator here is deliberately SMALLER than the tier-distribution
	// total, and saying so in the output stops a reader treating the gap as a
	// discrepancy. A sample that classified a tier but then failed to decode
	// counts toward the tier counts (see add()) yet never reaches these
	// accumulators, so it is present above and absent here.
	fmt.Fprintf(&b, "\navailableLyricsType agreement: %d/%d"+
		" (denominator counts only samples that decoded fully; see tier distribution for the full count)\n",
		r.availTierAgree, r.availTierSeen)

	fmt.Fprintf(&b, "\nisOfficial values:\n")
	if len(r.officialCounts) == 0 {
		fmt.Fprintf(&b, "  field absent on every sample\n")
	}
	for _, k := range sortedKeys(r.officialCounts) {
		fmt.Fprintf(&b, "  %q: %d\n", k, r.officialCounts[k])
	}

	fmt.Fprintf(&b, "\nisOfficial by tier:\n")
	for _, tier := range []int{tierUnsynced, tierLineSync, tierWordSync} {
		if len(r.officialByTier[tier]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  tier %d:\n", tier)
		for _, k := range sortedKeys(r.officialByTier[tier]) {
			fmt.Fprintf(&b, "    %q: %d\n", k, r.officialByTier[tier][k])
		}
	}

	fmt.Fprintf(&b, "\ncopyright populated: %d\n", r.copyrightSeen)

	fmt.Fprintf(&b, "\ncue counts (n=%d):\n", len(r.cueCounts))
	if len(r.cueCounts) > 0 {
		sorted := append([]int(nil), r.cueCounts...)
		sort.Ints(sorted)
		fmt.Fprintf(&b, "  min %d  median %d  max %d\n",
			sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1])
	}

	fmt.Fprintf(&b, "\nline-sync corpus: %d paired / %d total\n",
		r.lsyPaired, r.tierCounts[tierLineSync])
	for _, k := range sortedKeys(r.lsyUnpaired) {
		fmt.Fprintf(&b, "  unpaired (%s): %d\n", k, r.lsyUnpaired[k])
	}

	fmt.Fprintf(&b, "\nword-sync distinct-start ratios (n=%d):\n", len(r.distinctRatios))
	if len(r.distinctRatios) > 0 {
		sorted := append([]float64(nil), r.distinctRatios...)
		sort.Float64s(sorted)
		fmt.Fprintf(&b, "  min %.2f  median %.2f  max %.2f\n",
			sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1])
	}

	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSurveyReport_EmitsNoIdentifyingContent is the privacy guarantee made
// testable. The report is the only probe output permitted on a shared surface
// (an issue, a PR body, a commit message), so it must be impossible for a track
// title or a lyric to reach it.
//
// The guarantee is structural: sampleObservation has no field that could carry
// one. This test defends that structure by feeding obviously-identifiable
// strings through every field that IS free-form and asserting none of the
// canary values appear.
func TestSurveyReport_EmitsNoIdentifyingContent(t *testing.T) {
	const canary = "CANARY-IDENTIFYING-CONTENT"

	r := newSurveyReport()
	r.add(sampleObservation{
		Tier:          tierWordSync,
		AvailableTier: tierWordSync,
		CueCount:      42,
		IsOfficial:    "1",
		Copyright:     canary + "-COPYRIGHT",
		DistinctRatio: 1.0,
	})
	r.add(sampleObservation{Err: "ErrNotFound"})
	// The canary MUST also travel through IsOfficial, because that is the one
	// free-form field render() prints VERBATIM (Copyright is only counted). An
	// earlier version of this test fed the canary through Copyright alone and set
	// IsOfficial to a benign "1" -- so it asserted over the path that cannot leak
	// while leaving the path that can entirely unexercised.
	r.add(sampleObservation{
		Tier:       tierUnsynced,
		IsOfficial: canary + "-OFFICIAL",
	})

	out := r.render()

	if strings.Contains(out, canary) {
		t.Errorf("report leaked a free-form field value into its output:\n%s", out)
	}
	if !strings.Contains(out, "samples: 3") {
		t.Errorf("report did not count every sample:\n%s", out)
	}
	// The bound must not have swallowed the ordinary enumerated token: the whole
	// point of bounding rather than bucketing is that the real values still print,
	// since printing them is how the sweep found the isOfficial correlation.
	if !strings.Contains(out, `"1": 1`) {
		t.Errorf("a short enumerated token was not printed verbatim; bounding must "+
			"not degrade the measurement:\n%s", out)
	}
	if !strings.Contains(out, "ErrNotFound: 1") {
		t.Errorf("report did not record the error class:\n%s", out)
	}
	if !strings.Contains(out, "copyright populated: 1") {
		t.Errorf("report did not count the populated copyright field:\n%s", out)
	}
}

// TestSurveyReport_AvailableTierAgreement pins that the availableLyricsType
// statistic is REAL rather than a permanent 0/0. The field is decoded on apiSong
// and stamped by surveySample purely so this line can be computed; a reader who
// sees 0/0 would read it as "absent on every sample", the opposite of the
// established finding.
func TestSurveyReport_AvailableTierAgreement(t *testing.T) {
	r := newSurveyReport()
	// Agrees with the payload-derived tier.
	r.add(sampleObservation{Tier: tierWordSync, AvailableTier: tierWordSync})
	r.add(sampleObservation{Tier: tierUnsynced, AvailableTier: tierUnsynced})
	// Seen but disagrees, so it counts in the denominator only.
	r.add(sampleObservation{Tier: tierLineSync, AvailableTier: tierWordSync})
	// Absent, so it counts in neither.
	r.add(sampleObservation{Tier: tierLineSync})

	if r.availTierSeen != 3 {
		t.Errorf("availTierSeen = %d, want 3 (an absent field is not a sample)", r.availTierSeen)
	}
	if r.availTierAgree != 2 {
		t.Errorf("availTierAgree = %d, want 2", r.availTierAgree)
	}
	if got := r.render(); !strings.Contains(got, "availableLyricsType agreement: 2/3") {
		t.Errorf("report did not render the agreement statistic:\n%s", got)
	}

	// The empty case must still render, and must not claim agreement.
	empty := newSurveyReport().render()
	if !strings.Contains(empty, "availableLyricsType agreement: 0/0") {
		t.Errorf("empty report did not render the agreement line:\n%s", empty)
	}
}

// TestSurveyReport_CueCountDistribution pins the cue-count aggregation, which
// exists to size the future writer work. A count is aggregate-safe; the empty
// case is covered because render() indexes the sorted slice.
func TestSurveyReport_CueCountDistribution(t *testing.T) {
	r := newSurveyReport()
	r.add(sampleObservation{Tier: tierWordSync, CueCount: 30})
	r.add(sampleObservation{Tier: tierWordSync, CueCount: 10})
	r.add(sampleObservation{Tier: tierWordSync, CueCount: 50})
	// Zero carries no distribution information and must be left out.
	r.add(sampleObservation{Tier: tierUnsynced, CueCount: 0})
	// An errored sample never reaches the success-path accumulators.
	r.add(sampleObservation{Err: "wsy-decode", Tier: tierWordSync, CueCount: 99})

	if len(r.cueCounts) != 3 {
		t.Fatalf("cueCounts = %v, want 3 entries (zero and errored samples excluded)", r.cueCounts)
	}
	out := r.render()
	if !strings.Contains(out, "cue counts (n=3)") {
		t.Errorf("report did not render the cue-count sample size:\n%s", out)
	}
	if !strings.Contains(out, "min 10  median 30  max 50") {
		t.Errorf("report did not render the cue-count distribution:\n%s", out)
	}

	// No samples with cues: render() must not index an empty slice.
	emptyReport := newSurveyReport()
	emptyReport.add(sampleObservation{Tier: tierUnsynced})
	empty := emptyReport.render()
	if !strings.Contains(empty, "cue counts (n=0)") {
		t.Errorf("empty report did not render the cue-count header:\n%s", empty)
	}
	if strings.Contains(empty, "min ") {
		t.Errorf("empty report rendered a distribution line with no data:\n%s", empty)
	}
}

// maxTokenLen bounds a provider value that render() prints VERBATIM. The two
// values observed across a 100-track live sweep were "0" and "1", so anything
// past a handful of characters is not the enumerated flag this report is
// measuring.
const maxTokenLen = 8

// maxSentinelLen bounds an INTERNAL error sentinel -- a literal written in this
// package, never provider data. It is separate from maxTokenLen on purpose: that
// bound is 8 because a provider-controlled flag past a handful of characters is
// not the enumerated value being measured, whereas these sentinels are our own
// and several legitimately exceed 8 ("no-candidate", "empty-payload",
// "ErrProviderUnavailable"). Bounding them with maxTokenLen redacted the very
// attribution the unpaired breakdown exists to print. The bound is kept -- one
// sentinel is computed (fmt.Sprintf("tier-%d", ...)) and future code could put
// something unexpected here -- but sized so an ordinary sentinel survives it.
const maxSentinelLen = 32

// boundedSentinel bounds an internal sentinel for display. Same shape as
// boundedToken, different threat model: this one guards against a surprising
// value, not against provider-controlled content reaching a shared surface.
func boundedSentinel(v string) string {
	if len(v) <= maxSentinelLen {
		return v
	}
	return fmt.Sprintf("<non-sentinel value, %d bytes>", len(v))
}

// boundedToken passes through a short enumerated token and replaces anything
// longer with a length-only placeholder.
//
// This closes the one path by which the report could leak. sampleObservation
// carries no title/artist/album field, so the privacy guarantee is otherwise
// structural -- but IsOfficial is a provider-controlled string that render()
// prints verbatim, and "provider-controlled" plus "printed verbatim" is a leak
// waiting for the provider to change shape.
//
// It BOUNDS rather than buckets. Bucketing (mapping every value to
// official/not-official) was the reviewer's suggestion and would have destroyed
// the measurement: printing the raw values is exactly how the sweep found that
// isOfficial discriminates INVERTED (every word-synced result carried "0", every
// unsynced one "1"), which is the finding that redirected #480 and #615. A
// length bound keeps that visible while making an unexpected long value
// unprintable.
func boundedToken(v string) string {
	if len(v) <= maxTokenLen {
		return v
	}
	return fmt.Sprintf("<non-token value, %d bytes>", len(v))
}
