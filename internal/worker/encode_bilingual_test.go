package worker

import (
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestEncodeDecodeSong_TranslationRoundTrip verifies a Song carrying a non-empty
// TranslationSubtitles (and RomanizationSubtitles) track survives the cache
// JSON round-trip, since encodeSong/decodeSong marshal the whole Song.
func TestEncodeDecodeSong_TranslationRoundTrip(t *testing.T) {
	song := models.Song{
		Track:                 models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles:             models.Synced{Lines: []models.Lines{{Text: "original", Time: models.Time{Seconds: 1}}}},
		TranslationSubtitles:  models.Synced{Lines: []models.Lines{{Text: "translation", Time: models.Time{Seconds: 1}}}},
		RomanizationSubtitles: models.Synced{Lines: []models.Lines{{Text: "romaji", Time: models.Time{Seconds: 1}}}},
	}

	encoded, err := encodeSong(song)
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	got := decodeSong(encoded, models.Track{})

	if len(got.TranslationSubtitles.Lines) != 1 || got.TranslationSubtitles.Lines[0].Text != "translation" {
		t.Errorf("TranslationSubtitles did not round-trip: %+v", got.TranslationSubtitles)
	}
	if len(got.RomanizationSubtitles.Lines) != 1 || got.RomanizationSubtitles.Lines[0].Text != "romaji" {
		t.Errorf("RomanizationSubtitles did not round-trip: %+v", got.RomanizationSubtitles)
	}
	if len(got.Subtitles.Lines) != 1 || got.Subtitles.Lines[0].Text != "original" {
		t.Errorf("Subtitles did not round-trip: %+v", got.Subtitles)
	}
}

// TestDecodeSong_OldCacheLacksTranslationFields verifies that an OLD cache JSON
// string lacking the new fields decodes to empty translation/romanization
// tracks (backward compatibility). Such a payload predates Phase 3.
func TestDecodeSong_OldCacheLacksTranslationFields(t *testing.T) {
	// A pre-Phase-3 cache entry: only Track/Lyrics/Subtitles present.
	old := `{"Track":{"artist_name":"Artist","track_name":"Track"},"Subtitles":{"Lines":[{"text":"original"}]}}`

	got := decodeSong(old, models.Track{})

	if len(got.Subtitles.Lines) != 1 || got.Subtitles.Lines[0].Text != "original" {
		t.Fatalf("old cache Subtitles did not decode: %+v", got.Subtitles)
	}
	if len(got.TranslationSubtitles.Lines) != 0 {
		t.Errorf("old cache must decode to empty TranslationSubtitles; got %+v", got.TranslationSubtitles)
	}
	if len(got.RomanizationSubtitles.Lines) != 0 {
		t.Errorf("old cache must decode to empty RomanizationSubtitles; got %+v", got.RomanizationSubtitles)
	}
}

// TestDecodeSong_OldCacheLacksWordTimings pins backward compatibility. Every
// cache row written before this change omits the field, and those rows are not
// rewritten -- they simply decode with no timings and fall back to line-synced
// output, exactly as they behave today. A decode error here would break every
// existing entry.
func TestDecodeSong_OldCacheLacksWordTimings(t *testing.T) {
	old := `{"Track":{"artist_name":"Artist","track_name":"Track"},"Subtitles":{"Lines":[{"text":"alpha beta"}]}}`

	got := decodeSong(old, models.Track{})

	if len(got.Subtitles.Lines) != 1 || got.Subtitles.Lines[0].Text != "alpha beta" {
		t.Fatalf("old cache Subtitles did not decode: %+v", got.Subtitles)
	}
	if len(got.WordTimings) != 0 {
		t.Errorf("old cache must decode to empty WordTimings; got %+v", got.WordTimings)
	}
}

// TestEncodeSong_OmitsEmptyWordTimings covers the cache-size half of the #480
// review. WordTimings is persisted now, but the overwhelming majority of cached
// songs carry none -- only petitlyrics' word-synced tier produces them -- so a
// bare field emits `"WordTimings":null` on every one of those rows.
//
// That is ~20 wasted bytes per row across the whole library for a field the row
// does not have, and the cache is written on every settle. omitempty removes it
// while keeping the bare Go field name, which is what makes the persisted key
// small in the first place.
func TestEncodeSong_OmitsEmptyWordTimings(t *testing.T) {
	plain := models.Song{
		Track:     models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{{Text: "alpha"}}},
	}
	encoded, err := encodeSong(plain)
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	if strings.Contains(encoded, "WordTimings") {
		t.Errorf("a song with no word timings still emits the key: %s", encoded)
	}

	// The control that keeps this from being satisfied by dropping the field
	// entirely: a song that HAS timings must still carry them.
	withTimings := plain
	withTimings.WordTimings = []models.WordTiming{{Line: 0, Text: "alpha", StartMS: 1000, EndMS: 1500}}
	encoded2, err := encodeSong(withTimings)
	if err != nil {
		t.Fatalf("encodeSong: %v", err)
	}
	if !strings.Contains(encoded2, "WordTimings") {
		t.Errorf("a song WITH word timings lost the key: %s", encoded2)
	}
}
