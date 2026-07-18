package petitlyrics

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestTrackFromEntry(t *testing.T) {
	if _, ok := trackFromEntry(ajaxEntry{Lyrics: ""}); ok {
		t.Error("empty payload should not produce a track")
	}
	if _, ok := trackFromEntry(ajaxEntry{Lyrics: "!!!not-base64!!!"}); ok {
		t.Error("undecodable base64 should not produce a track")
	}
	// Decodes, but no timestamps: a plain-text secondary is not adopted.
	if _, ok := trackFromEntry(ajaxEntry{Lyrics: b64("just plain words\n")}); ok {
		t.Error("non-synced secondary should not be adopted")
	}
	tr, ok := trackFromEntry(ajaxEntry{Lyrics: b64("[00:01.00]hi\n")})
	if !ok || len(tr.Lines) != 1 {
		t.Fatalf("synced secondary should be adopted; ok=%v lines=%d", ok, len(tr.Lines))
	}
}

func TestApplySecondaryTracks(t *testing.T) {
	song := &models.Song{}
	applySecondaryTracks(song, []ajaxEntry{
		{LyricsType: 99, Lyrics: b64("[00:01.00]ignored\n")},                // unknown type -> ignored
		{LyricsType: lyricsTypeTranslation, Lyrics: b64("[00:01.00]t1\n")},  // adopted
		{LyricsType: lyricsTypeTranslation, Lyrics: b64("[00:02.00]t2\n")},  // second translation ignored (first wins)
		{LyricsType: lyricsTypeRomanization, Lyrics: b64("[00:01.00]r1\n")}, // adopted
	})
	if len(song.TranslationSubtitles.Lines) != 1 {
		t.Fatalf("want 1 translation line (first wins), got %d", len(song.TranslationSubtitles.Lines))
	}
	if len(song.RomanizationSubtitles.Lines) != 1 {
		t.Fatalf("want 1 romanization line, got %d", len(song.RomanizationSubtitles.Lines))
	}

	// An unknown-type-only / empty rest leaves both fields absent.
	empty := &models.Song{}
	applySecondaryTracks(empty, []ajaxEntry{{LyricsType: lyricsTypeTranslation, Lyrics: ""}})
	if len(empty.TranslationSubtitles.Lines) != 0 {
		t.Fatalf("empty payload must not populate translation, got %d", len(empty.TranslationSubtitles.Lines))
	}
}
