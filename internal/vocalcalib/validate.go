package vocalcalib

import "github.com/sydlexius/canticle/internal/detector"

// Report is the outcome of validating a fixed threshold against a labeled set.
type Report struct {
	Threshold     float64
	PosN          int
	PosErrN       int
	PosErrRate    float64
	NegN          int
	NegRecoveredN int
	NegRecovery   float64
	Pass          bool
}

// Validate measures a fixed threshold against a labeled set and reports whether
// the positive-error-rate stays below maxPosErrRate (the #384 close-out check).
func Validate(scores []LabeledScore, gates Gates, threshold, maxPosErrRate float64) Report {
	var r Report
	r.Threshold = threshold
	for _, s := range scores {
		inst := detector.Instrumental(s.MusicSum, s.VocalPeak, s.SpeechMean, gates.MinConfidence, threshold, gates.SpeechMax)
		switch s.Label {
		case "vocal":
			r.PosN++
			if inst {
				r.PosErrN++
			}
		case "instrumental":
			r.NegN++
			if inst {
				r.NegRecoveredN++
			}
		}
	}
	if r.PosN > 0 {
		r.PosErrRate = float64(r.PosErrN) / float64(r.PosN)
	}
	if r.NegN > 0 {
		r.NegRecovery = float64(r.NegRecoveredN) / float64(r.NegN)
	}
	r.Pass = r.PosN > 0 && r.PosErrRate < maxPosErrRate
	return r
}
