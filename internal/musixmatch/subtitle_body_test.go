package musixmatch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// subtitleResponse builds a macro.subtitles.get envelope whose subtitle_body is
// the given already-JSON-escaped string. All text is synthetic placeholder.
func subtitleResponse(escapedBody string) string {
	return `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "title",
									"artist_name": "artist",
									"has_subtitles": 1,
									"has_lyrics": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{"subtitle": {"subtitle_body": "` + escapedBody + `"}}
								]
							}
						}
					}
				}
			}
		}
	}`
}

func clientReturning(t *testing.T, body string) *Client {
	t.Helper()
	c := NewClient("test-token")
	c.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, body), nil
	})}
	return c
}

// TestFindLyricsParsesLRCSubtitleBody is the core #838 assertion. Musixmatch
// changed subtitle_body from a JSON cue array to LRC text; the client must parse
// it. Measured 2026-09-04: HTTP 200, both inner status_codes 200, a complete
// 5457-byte body, failing json.Unmarshal at offset 3 because an LRC line opens
// with '[' exactly as a JSON array does.
func TestFindLyricsParsesLRCSubtitleBody(t *testing.T) {
	// Synthetic placeholder text, never real lyric content.
	lrc := `[00:12.34]alpha bravo\n[00:15.67]charlie delta\n[01:02.50]echo foxtrot`
	client := clientReturning(t, subtitleResponse(lrc))

	song, err := client.FindLyrics(context.Background(), models.Track{
		TrackName: "title", ArtistName: "artist",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 3 {
		t.Fatalf("subtitle lines = %d; want 3", len(song.Subtitles.Lines))
	}
	if got := song.Subtitles.Lines[0].Text; got != "alpha bravo" {
		t.Errorf("cue0 text = %q; want %q", got, "alpha bravo")
	}
	c0 := song.Subtitles.Lines[0].Time
	if c0.Minutes != 0 || c0.Seconds != 12 || c0.Hundredths != 34 {
		t.Errorf("cue0 time = %dm%ds%dh; want 0m12s34h", c0.Minutes, c0.Seconds, c0.Hundredths)
	}
	c2 := song.Subtitles.Lines[2].Time
	if c2.Minutes != 1 || c2.Seconds != 2 || c2.Hundredths != 50 {
		t.Errorf("cue2 time = %dm%ds%dh; want 1m2s50h", c2.Minutes, c2.Seconds, c2.Hundredths)
	}
}

// TestFindLyricsStillParsesJSONSubtitleBody pins the OLD encoding. The provider
// changed the format once and can change it back; a fix that only handles LRC
// would break on a revert. Both shapes must work.
func TestFindLyricsStillParsesJSONSubtitleBody(t *testing.T) {
	jsonBody := `[{\"text\":\"line one\",\"time\":{\"total\":1.23,\"minutes\":0,\"seconds\":1,\"hundredths\":23}}]`
	client := clientReturning(t, subtitleResponse(jsonBody))

	song, err := client.FindLyrics(context.Background(), models.Track{
		TrackName: "title", ArtistName: "artist",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Fatalf("subtitle lines = %d; want 1", len(song.Subtitles.Lines))
	}
	if got := song.Subtitles.Lines[0].Text; got != "line one" {
		t.Errorf("text = %q; want %q", got, "line one")
	}
	if got := song.Subtitles.Lines[0].Time.Seconds; got != 1 {
		t.Errorf("seconds = %d; want 1", got)
	}
}

// TestFindLyricsLRCWithHeaderTags asserts a leading [key:value] tag block does
// not become a bogus cue. An LRC body may carry [ar:], [ti:], [length:] headers,
// which open with '[' exactly as a cue does.
func TestFindLyricsLRCWithHeaderTags(t *testing.T) {
	lrc := `[ar:artist]\n[ti:title]\n[length:03:21]\n[00:05.00]alpha bravo`
	client := clientReturning(t, subtitleResponse(lrc))

	song, err := client.FindLyrics(context.Background(), models.Track{
		TrackName: "title", ArtistName: "artist",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Fatalf("subtitle lines = %d; want 1 (header tags must not become cues): %+v", len(song.Subtitles.Lines), song.Subtitles.Lines)
	}
	if got := song.Subtitles.Lines[0].Text; got != "alpha bravo" {
		t.Errorf("text = %q; want %q", got, "alpha bravo")
	}
}

// TestFindLyricsUnparsableSubtitleBodyReturnsSentinel guards the recurrence.
// A body that is neither LRC nor a JSON cue array must return a DISTINCT,
// identifiable error rather than a bare parse error. A bare error carries no
// sentinel, so orchestrator.ClassifyOutcome falls through to OutcomeTransport,
// which outranks OutcomeBenignMiss in precedence() -- so every attempt takes
// attempts++ plus geometric backoff toward retirement (the #748 mechanism).
// That is how this outage silently burned retry budget across ~12,644 rows.
func TestFindLyricsUnparsableSubtitleBodyReturnsSentinel(t *testing.T) {
	client := clientReturning(t, subtitleResponse(`not lrc and not json at all`))

	_, err := client.FindLyrics(context.Background(), models.Track{
		TrackName: "title", ArtistName: "artist",
	})
	if err == nil {
		t.Fatal("FindLyrics returned nil error for an unparsable subtitle_body")
	}
	if !errors.Is(err, ErrUnparsableSubtitleBody) {
		t.Fatalf("error = %v; want it to wrap ErrUnparsableSubtitleBody", err)
	}
	// The error must not leak the body content into logs.
	if strings.Contains(err.Error(), "not lrc and not json at all") {
		t.Errorf("error message leaks subtitle_body content: %v", err)
	}
}

// TestUnparsableSubtitleBodyIsNotABenignMiss asserts the new sentinel is not
// swallowed as a routine miss. A format change is a real upstream fault and must
// stay visible; classifying it benign would hide the next one entirely.
func TestUnparsableSubtitleBodyIsNotABenignMiss(t *testing.T) {
	if IsBenignMiss(ErrUnparsableSubtitleBody) {
		t.Error("ErrUnparsableSubtitleBody must not classify as a benign miss")
	}
}
