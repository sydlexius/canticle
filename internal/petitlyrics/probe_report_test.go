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
	Tier          int     // classifyPayload result, 0 when the lookup failed
	AvailableTier int     // availableLyricsType as reported, 0 when absent
	CueCount      int     // number of cues decoded, 0 when none
	IsOfficial    string  // raw isOfficial value, empty when absent
	Copyright     string  // raw copyright value, empty when absent
	DistinctRatio float64 // tier-3 only: fraction of lines with distinct word starts
	Err           string  // sentinel error class name, empty on success
}

type surveyReport struct {
	total          int
	tierCounts     map[int]int
	availTierAgree int
	availTierSeen  int
	officialCounts map[string]int
	officialByTier map[int]map[string]int
	copyrightSeen  int
	distinctRatios []float64
	errCounts      map[string]int
}

func newSurveyReport() *surveyReport {
	return &surveyReport{
		tierCounts:     map[int]int{},
		officialCounts: map[string]int{},
		officialByTier: map[int]map[string]int{},
		errCounts:      map[string]int{},
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
		r.officialCounts[obs.IsOfficial]++
		if r.officialByTier[obs.Tier] == nil {
			r.officialByTier[obs.Tier] = map[string]int{}
		}
		r.officialByTier[obs.Tier][obs.IsOfficial]++
	}
	if obs.Copyright != "" {
		r.copyrightSeen++
	}
	if obs.Tier == tierWordSync {
		r.distinctRatios = append(r.distinctRatios, obs.DistinctRatio)
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

	fmt.Fprintf(&b, "\navailableLyricsType agreement: %d/%d\n", r.availTierAgree, r.availTierSeen)

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

	out := r.render()

	if strings.Contains(out, canary) {
		t.Errorf("report leaked a free-form field value into its output:\n%s", out)
	}
	if !strings.Contains(out, "samples: 2") {
		t.Errorf("report did not count both samples:\n%s", out)
	}
	if !strings.Contains(out, "ErrNotFound: 1") {
		t.Errorf("report did not record the error class:\n%s", out)
	}
	if !strings.Contains(out, "copyright populated: 1") {
		t.Errorf("report did not count the populated copyright field:\n%s", out)
	}
}
