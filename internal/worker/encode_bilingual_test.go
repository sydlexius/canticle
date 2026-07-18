package worker

import (
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
