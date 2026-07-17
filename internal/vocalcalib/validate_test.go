package vocalcalib

import "testing"

func TestValidate_PassFail(t *testing.T) {
	gates := Gates{MinConfidence: 0.90, SpeechMax: 0.20}
	scores := sample() // from sweep_test.go: 200 positives (1 at 0.04), 100 negatives at 0.02

	// Threshold 0.30: only the single 0.04 positive is misclassified => 0.5% < 1% => pass.
	r := Validate(scores, gates, 0.30, 0.01)
	if !r.Pass || r.PosErrN != 1 || r.NegRecoveredN != 100 {
		t.Fatalf("expected pass with 1 pos err and full recovery, got %+v", r)
	}

	// Threshold 0.60: clean cluster at 0.50 now misclassified => >1% => fail.
	r2 := Validate(scores, gates, 0.60, 0.01)
	if r2.Pass {
		t.Fatalf("expected fail at 0.60, got %+v", r2)
	}
}
