package vocalcalib

import (
	"math"
	"testing"
)

// Positives (vocal) peak high; negatives (instrumental) peak low, with one
// positive outlier at 0.04 that the 1% budget must tolerate rather than let it
// drag the threshold down to ~0.04.
func sample() []LabeledScore {
	var s []LabeledScore
	for i := 0; i < 199; i++ { // 199 clean positives at 0.50
		s = append(s, LabeledScore{MusicSum: 0.98, VocalPeak: 0.50, SpeechMean: 0.001, Label: "vocal"})
	}
	s = append(s, LabeledScore{MusicSum: 0.98, VocalPeak: 0.04, SpeechMean: 0.001, Label: "vocal"}) // 1 outlier
	for i := 0; i < 100; i++ {                                                                      // negatives at 0.02
		s = append(s, LabeledScore{MusicSum: 0.98, VocalPeak: 0.02, SpeechMean: 0.001, Label: "instrumental"})
	}
	return s
}

func TestSelectThreshold_ToleratesSingleOutlierUnder1Percent(t *testing.T) {
	gates := Gates{MinConfidence: 0.90, SpeechMax: 0.20}
	sel, err := SelectThreshold(sample(), gates, 0.01)
	if err != nil {
		t.Fatalf("SelectThreshold: %v", err)
	}
	// With 1/200 positives = 0.5% < 1%, the threshold may rise above the 0.04
	// outlier (up to just below the clean cluster at 0.50), recovering all
	// negatives (all 100 have vocalPeak 0.02 < threshold).
	if sel.Threshold <= 0.04 {
		t.Fatalf("threshold=%.4f should clear the single sub-1%% outlier", sel.Threshold)
	}
	if sel.PosErrRate >= 0.01 {
		t.Fatalf("PosErrRate=%.4f must stay < 1%%", sel.PosErrRate)
	}
	if math.Abs(sel.NegRecovery-1.0) > 1e-9 {
		t.Fatalf("NegRecovery=%.4f want 1.0", sel.NegRecovery)
	}
}

func TestSelectThreshold_ZeroBudgetPinsBelowLowestPositive(t *testing.T) {
	gates := Gates{MinConfidence: 0.90, SpeechMax: 0.20}
	sel, err := SelectThreshold(sample(), gates, 0.0)
	if err != nil {
		t.Fatalf("SelectThreshold: %v", err)
	}
	// Zero budget: threshold must sit at/below the outlier 0.04 so NO positive
	// is misclassified.
	if sel.Threshold > 0.04 {
		t.Fatalf("threshold=%.4f must not exceed the lowest positive under a 0 budget", sel.Threshold)
	}
	if sel.PosErrRate != 0 {
		t.Fatalf("PosErrRate=%.4f want 0", sel.PosErrRate)
	}
}
