package vocalcalib

import (
	"fmt"
	"sort"

	"github.com/sydlexius/canticle/internal/detector"
)

// Gates holds the music and speech thresholds kept fixed while the vocal-peak
// threshold is swept.
type Gates struct {
	MinConfidence float64
	SpeechMax     float64
}

// CurvePoint is one swept vocal-peak threshold and its measured rates.
type CurvePoint struct {
	Threshold   float64
	PosErrRate  float64 // fraction of positives wrongly marked instrumental
	NegRecovery float64 // fraction of negatives correctly marked instrumental
}

// Selection is the chosen operating point plus the full swept curve.
type Selection struct {
	Threshold   float64
	PosErrRate  float64
	NegRecovery float64
	PosN        int
	NegN        int
	Curve       []CurvePoint
}

// SelectThreshold sweeps the vocal-peak threshold over every distinct positive
// vocal_peak value and returns the HIGHEST threshold whose positive-error-rate
// stays <= maxPosErrRate. Music and speech gates are held at gates; a track is
// "marked instrumental" per detector.Instrumental. Positives set the ceiling;
// negatives measure recovery only.
func SelectThreshold(scores []LabeledScore, gates Gates, maxPosErrRate float64) (Selection, error) {
	var pos, neg []LabeledScore
	for _, s := range scores {
		switch s.Label {
		case "vocal":
			pos = append(pos, s)
		case "instrumental":
			neg = append(neg, s)
		}
	}
	if len(pos) == 0 {
		return Selection{}, fmt.Errorf("no positive (vocal) samples")
	}

	// Candidate thresholds: each distinct positive vocal_peak, plus a hair above
	// the max so the top of the range is reachable. Sorted ascending.
	cand := map[float64]struct{}{}
	for _, p := range pos {
		cand[p.VocalPeak] = struct{}{}
	}
	thresholds := make([]float64, 0, len(cand)+1)
	for t := range cand {
		thresholds = append(thresholds, t)
	}
	sort.Float64s(thresholds)
	if n := len(thresholds); n > 0 {
		thresholds = append(thresholds, thresholds[n-1]*1.0001+1e-9)
	}

	markInstrumental := func(s LabeledScore, vocalMax float64) bool {
		return detector.Instrumental(s.MusicSum, s.VocalPeak, s.SpeechMean, gates.MinConfidence, vocalMax, gates.SpeechMax)
	}

	sel := Selection{PosN: len(pos), NegN: len(neg), Threshold: 0}
	for _, th := range thresholds {
		posErr := 0
		for _, p := range pos {
			if markInstrumental(p, th) {
				posErr++
			}
		}
		negRec := 0
		for _, n := range neg {
			if markInstrumental(n, th) {
				negRec++
			}
		}
		pr := float64(posErr) / float64(len(pos))
		nr := 0.0
		if len(neg) > 0 {
			nr = float64(negRec) / float64(len(neg))
		}
		sel.Curve = append(sel.Curve, CurvePoint{Threshold: th, PosErrRate: pr, NegRecovery: nr})
		if pr <= maxPosErrRate && th > sel.Threshold {
			sel.Threshold = th
			sel.PosErrRate = pr
			sel.NegRecovery = nr
		}
	}
	return sel, nil
}
