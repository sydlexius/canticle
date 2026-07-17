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
