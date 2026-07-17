package vocalcalib

import (
	"bytes"
	"testing"
)

func TestJSONLRoundTrip(t *testing.T) {
	in := []LabeledScore{
		{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "", DetectorVersion: "1.17.0", Label: "instrumental"},
		{MusicSum: 0.98, VocalPeak: 0.61, SpeechMean: 0.002, VocalClass: "Singing", DetectorVersion: "1.17.0", Label: "vocal"},
	}
	var buf bytes.Buffer
	for _, s := range in {
		if err := WriteJSONL(&buf, s); err != nil {
			t.Fatalf("WriteJSONL: %v", err)
		}
	}
	got, err := ReadJSONL(&buf)
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if len(got) != 2 || got[1].VocalClass != "Singing" || got[0].Label != "instrumental" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
