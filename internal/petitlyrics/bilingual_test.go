package petitlyrics

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// ajaxJSONMulti builds an AJAX response array from arbitrary (lyrics_type,
// base64) entries so multi-track fixtures can be assembled in tests.
func ajaxJSONMulti(entries ...ajaxEntry) string {
	out := "["
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += `{"lyrics_type":` + itoaN(e.LyricsType) + `,"lyrics":"` + e.Lyrics + `"}`
	}
	return out + "]"
}

func itoaN(i int) string {
	if i == lyricsTypeTranslation {
		return "4"
	}
	if i == lyricsTypeRomanization {
		return "5"
	}
	return itoa(i)
}

// TestFindLyrics_OriginalPlusTranslation verifies that a translation entry
// populates song.TranslationSubtitles while the first synced entry remains the
// original (song.Subtitles).
func TestFindLyrics_OriginalPlusTranslation(t *testing.T) {
	original := "[00:01.50]オリジナル\n[00:12.30]二行目\n"
	translation := "[00:01.50]original line\n[00:12.30]second line\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody: ajaxJSONMulti(
			ajaxEntry{LyricsType: 2, Lyrics: b64(original)},
			ajaxEntry{LyricsType: lyricsTypeTranslation, Lyrics: b64(translation)},
		),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("original synced lines = %d; want 2", len(song.Subtitles.Lines))
	}
	if song.Subtitles.Lines[0].Text != "オリジナル" {
		t.Fatalf("original line0 = %q; want オリジナル", song.Subtitles.Lines[0].Text)
	}
	if len(song.TranslationSubtitles.Lines) != 2 {
		t.Fatalf("translation lines = %d; want 2", len(song.TranslationSubtitles.Lines))
	}
	if song.TranslationSubtitles.Lines[0].Text != "original line" {
		t.Fatalf("translation line0 = %q; want \"original line\"", song.TranslationSubtitles.Lines[0].Text)
	}
	if len(song.RomanizationSubtitles.Lines) != 0 {
		t.Fatalf("romanization should be empty; got %d lines", len(song.RomanizationSubtitles.Lines))
	}
}

// TestFindLyrics_OriginalPlusRomanization verifies a romanization entry
// populates song.RomanizationSubtitles.
func TestFindLyrics_OriginalPlusRomanization(t *testing.T) {
	original := "[00:01.50]オリジナル\n"
	romaji := "[00:01.50]orijinaru\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody: ajaxJSONMulti(
			ajaxEntry{LyricsType: 2, Lyrics: b64(original)},
			ajaxEntry{LyricsType: lyricsTypeRomanization, Lyrics: b64(romaji)},
		),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.RomanizationSubtitles.Lines) != 1 || song.RomanizationSubtitles.Lines[0].Text != "orijinaru" {
		t.Fatalf("romanization = %+v; want one line \"orijinaru\"", song.RomanizationSubtitles.Lines)
	}
	if len(song.TranslationSubtitles.Lines) != 0 {
		t.Fatalf("translation should be empty; got %d lines", len(song.TranslationSubtitles.Lines))
	}
}

// TestFindLyrics_OriginalOnlyLeavesTranslationEmpty verifies a single-entry
// response leaves both new fields empty (backward-compatible default).
func TestFindLyrics_OriginalOnlyLeavesTranslationEmpty(t *testing.T) {
	original := "[00:01.50]hello\n[00:12.30]world\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody:   ajaxJSONMulti(ajaxEntry{LyricsType: 2, Lyrics: b64(original)}),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("original synced lines = %d; want 2", len(song.Subtitles.Lines))
	}
	if len(song.TranslationSubtitles.Lines) != 0 {
		t.Fatalf("translation should be empty for original-only; got %d", len(song.TranslationSubtitles.Lines))
	}
	if len(song.RomanizationSubtitles.Lines) != 0 {
		t.Fatalf("romanization should be empty for original-only; got %d", len(song.RomanizationSubtitles.Lines))
	}
}
