package models

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// TestSong_UpstreamIsNeverSerialized pins the `json:"-"` on Song.Upstream.
//
// THE TAG IS THE WHOLE SAFETY ARGUMENT, and until this test existed nothing
// held it there: a hostile review mutated it to `json:"upstream,omitempty"` and
// the mutation SURVIVED the entire tree, green in every package. The field's own
// doc comment devotes six lines to why the tag is required, which made the gap
// worse rather than better -- a heavily argued invariant that no test enforces
// reads as covered.
//
// Why it matters concretely: encodeSong/decodeSong round-trip this struct
// through the lyrics cache, and the cache is keyed on (artist, title, duration
// bucket) with no knowledge of which licensor served the entry it stored. A
// serialized upstream would let a cache HIT resurrect an attribution that was
// true for a DIFFERENT fetch, and the writer would then stamp that licensor into
// a sidecar it never served -- a false attribution on disk, in the user's own
// file, with nothing downstream able to detect it.
//
// Both directions are asserted. Marshal alone would still pass if the tag were
// `json:"upstream"` on a struct that never round-trips; the Unmarshal half is
// what pins the resurrection path itself.
func TestSong_UpstreamIsNeverSerialized(t *testing.T) {
	// WinningLane is the CONTROL. It carries the same tag for the same reason,
	// so if a change ever made Song serializable wholesale, this row shows the
	// finding is about the struct rather than about Upstream alone.
	blob, err := json.Marshal(Song{Upstream: "musixmatch", WinningLane: "innertube"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "musixmatch") {
		t.Errorf("Song.Upstream was SERIALIZED into %s -- it must carry `json:\"-\"`, or a cache hit can resurrect an attribution true for a different fetch", blob)
	}
	if strings.Contains(string(blob), "innertube") {
		t.Errorf("Song.WinningLane was SERIALIZED into %s -- the same invariant, and its breach means the whole struct became serializable", blob)
	}

	// The resurrection path proper: a cache row written by some other producer
	// that DOES carry the key must not populate the field on the way back in.
	var s Song
	if err := json.Unmarshal([]byte(`{"upstream":"lyricfind","WinningLane":"innertube"}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Upstream != "" {
		t.Errorf("Song.Upstream = %q after decoding a blob that carried the key; it must stay empty so a cache hit asserts no attribution", s.Upstream)
	}
	if s.WinningLane != "" {
		t.Errorf("Song.WinningLane = %q after decoding a blob that carried the key; same invariant", s.WinningLane)
	}
}
