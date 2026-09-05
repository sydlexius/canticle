package models

import "testing"

// TestSongTranslationFieldsZeroValueAbsent verifies the new bilingual tracks are
// value-typed and default to absent (empty Lines) on a freshly constructed Song,
// matching the existing Subtitles convention (zero value = absent).
func TestSongTranslationFieldsZeroValueAbsent(t *testing.T) {
	var s Song
	if len(s.TranslationSubtitles.Lines) != 0 {
		t.Errorf("TranslationSubtitles default should be empty, got %d lines", len(s.TranslationSubtitles.Lines))
	}
	if len(s.RomanizationSubtitles.Lines) != 0 {
		t.Errorf("RomanizationSubtitles default should be empty, got %d lines", len(s.RomanizationSubtitles.Lines))
	}
}

// TestSongTranslationFieldsAssignable verifies the new fields accept Synced
// values and round-trip the assigned lines.
func TestSongTranslationFieldsAssignable(t *testing.T) {
	s := Song{
		TranslationSubtitles:  Synced{Lines: []Lines{{Text: "translation"}}},
		RomanizationSubtitles: Synced{Lines: []Lines{{Text: "romaji"}}},
	}
	if got := s.TranslationSubtitles.Lines[0].Text; got != "translation" {
		t.Errorf("TranslationSubtitles text = %q, want %q", got, "translation")
	}
	if got := s.RomanizationSubtitles.Lines[0].Text; got != "romaji" {
		t.Errorf("RomanizationSubtitles text = %q, want %q", got, "romaji")
	}
}

func TestSong_DetectorVersionField(t *testing.T) {
	s := Song{DetectorVersion: "1.2.3"}
	if s.DetectorVersion != "1.2.3" {
		t.Fatalf("DetectorVersion not carried: %q", s.DetectorVersion)
	}
}

func TestSong_DetectorTelemetryFields(t *testing.T) {
	s := Song{
		DetectorVersion:    "1.5.0",
		DetectorMusicSum:   0.9,
		DetectorVocalPeak:  0.01,
		DetectorSpeechMean: 0.02,
		DetectorVocalClass: "Singing",
	}
	if s.DetectorMusicSum != 0.9 || s.DetectorVocalPeak != 0.01 ||
		s.DetectorSpeechMean != 0.02 || s.DetectorVocalClass != "Singing" {
		t.Fatalf("telemetry fields not carried: %+v", s)
	}
}

// TestMsToTime covers all four Time fields together: the writer reads
// Minutes/Seconds/Hundredths, timing validation recomputes from those same
// three, and sorting uses Total, so a regression in any single field is a
// real bug, not a cosmetic one.
//
// The "over-100-minutes" row's ceiling (100) is chosen past the two-digit
// minute field of the LRC "mm:ss.xx" output format: a future change that
// makes Minutes fit that field (e.g. a modulus) would silently corrupt the
// timestamp of any track at or past 100 minutes. This does not close the class -- a modulus
// above whatever ceiling the table asserts can never be caught by a finite
// table -- it just moves the ceiling past the number the output format makes
// plausible.
func TestMsToTime(t *testing.T) {
	tests := []struct {
		name          string
		ms            int
		min, sec, hun int
		total         float64
	}{
		{"zero", 0, 0, 0, 0, 0},
		{"sub-second", 3790, 0, 3, 79, 3.79},
		{"over-a-minute", 65432, 1, 5, 43, 65.432},
		{"over-an-hour-no-wrap", 4200000, 70, 0, 0, 4200},
		{"over-100-minutes", 6000000, 100, 0, 0, 6000},
		{"just-under-two-minutes", 119999, 1, 59, 99, 119.999},
		{"negative clamps to zero", -1, 0, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MsToTime(tc.ms)
			if got.Minutes != tc.min {
				t.Errorf("MsToTime(%d).Minutes = %d, want %d", tc.ms, got.Minutes, tc.min)
			}
			if got.Seconds != tc.sec {
				t.Errorf("MsToTime(%d).Seconds = %d, want %d", tc.ms, got.Seconds, tc.sec)
			}
			if got.Hundredths != tc.hun {
				t.Errorf("MsToTime(%d).Hundredths = %d, want %d", tc.ms, got.Hundredths, tc.hun)
			}
			if got.Total != tc.total {
				t.Errorf("MsToTime(%d).Total = %v, want %v", tc.ms, got.Total, tc.total)
			}
		})
	}
}
