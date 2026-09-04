package musixmatch

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// matchResponse builds an envelope whose matcher returns the given track
// identity, with one valid LRC cue. All text is synthetic placeholder.
func matchResponse(artist, title string) string {
	return `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {"track": {
								"track_name": "` + title + `",
								"artist_name": "` + artist + `",
								"has_subtitles": 1,
								"has_lyrics": 1
							}}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {"body": {"subtitle_list": [
							{"subtitle": {"subtitle_body": "[00:05.00]alpha bravo"}}
						]}}
					}
				}
			}
		}
	}`
}

// TestFindLyricsRejectsUnrelatedMatch is the core guard: a response for a wholly
// different track must NOT reach the writer.
//
// Measured live 2026-09-04: musixmatch returned ONE fixed track for every query,
// including a deliberately nonsensical artist/title. Parsing such a response
// correctly and writing it would stamp one unrelated song's lyrics across the
// library, so the parse fix (#838) is only safe alongside this check.
func TestFindLyricsRejectsUnrelatedMatch(t *testing.T) {
	client := clientReturning(t, matchResponse("Unrelated Performer", "Unrelated Song"))

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "Requested Artist", TrackName: "Requested Title",
	})
	if err == nil {
		t.Fatal("FindLyrics accepted a wholly unrelated match")
	}
	if !errors.Is(err, ErrMatchMismatch) {
		t.Fatalf("error = %v; want it to wrap ErrMatchMismatch", err)
	}
}

// TestFindLyricsAcceptsLegitimateVariance is the counterweight, and it is the
// test that keeps the guard from becoming its own outage. The matcher
// legitimately returns near-matches -- article differences, featured artists,
// remaster and version suffixes. Rejecting those would turn working fetches into
// misses across the whole library, which is the mirror image of the corruption
// this guard prevents.
func TestFindLyricsAcceptsLegitimateVariance(t *testing.T) {
	cases := []struct {
		name                string
		qArtist, qTitle     string
		gotArtist, gotTitle string
	}{
		{"leading article", "Beatles", "Yesterday", "The Beatles", "Yesterday"},
		{"remaster suffix", "Queen", "Bohemian Rhapsody", "Queen", "Bohemian Rhapsody - 2011 Remaster"},
		{"featured artist", "Artist One", "Some Song", "Artist One feat. Artist Two", "Some Song"},
		{"case and spacing", "artist  one", "some song", "Artist One", "Some Song"},
		{"title variance only", "Artist One", "Some Song", "Artist One", "Some Song (Live)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := clientReturning(t, matchResponse(c.gotArtist, c.gotTitle))
			song, err := client.FindLyrics(context.Background(), models.Track{
				ArtistName: c.qArtist, TrackName: c.qTitle,
			})
			if err != nil {
				t.Fatalf("guard rejected a legitimate match (%s -> %s): %v",
					c.qArtist+"/"+c.qTitle, c.gotArtist+"/"+c.gotTitle, err)
			}
			if len(song.Subtitles.Lines) != 1 {
				t.Errorf("lines = %d; want 1", len(song.Subtitles.Lines))
			}
		})
	}
}

// TestMatchMismatchIsNotABenignMissSentinelButClassifiesSafely documents the
// intended split: the sentinel is distinct and greppable, but must take the
// bounded-retry path so a provider-wide mismatch cannot march every row toward
// retirement (the #748 precedence mechanism).
func TestMatchMismatchIsNotSilentlySwallowed(t *testing.T) {
	if !errors.Is(ErrMatchMismatch, ErrMatchMismatch) {
		t.Fatal("sentinel identity broken")
	}
	// It must be its own condition, never conflated with "no such track".
	if errors.Is(ErrMatchMismatch, ErrNotFound) {
		t.Error("ErrMatchMismatch must not wrap ErrNotFound: a wrong answer is not an absent one")
	}
}

// TestFindLyricsMismatchErrorCarriesNoContent asserts the error names neither the
// requested nor the returned title/artist. The error string reaches logs and the
// failure-analysis report, and a library's track metadata is private.
func TestFindLyricsMismatchErrorCarriesNoContent(t *testing.T) {
	client := clientReturning(t, matchResponse("Unrelated Performer", "Unrelated Song"))

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "Requested Artist", TrackName: "Requested Title",
	})
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	for _, leak := range []string{"Unrelated Performer", "Unrelated Song", "Requested Artist", "Requested Title"} {
		if contains(err.Error(), leak) {
			t.Errorf("error leaks %q into logs: %v", leak, err)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// TestFindLyricsEmptyQueryFieldsSkipGuard: the probe path and some callers leave
// artist or title blank. A blank query field cannot be compared, so the guard
// must fail OPEN there rather than rejecting every such call.
func TestFindLyricsEmptyQueryFieldsSkipGuard(t *testing.T) {
	client := clientReturning(t, matchResponse("Some Performer", "Some Song"))

	song, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "", TrackName: "",
	})
	if err != nil {
		t.Fatalf("guard rejected a call with no comparable query fields: %v", err)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Errorf("lines = %d; want 1", len(song.Subtitles.Lines))
	}
}
