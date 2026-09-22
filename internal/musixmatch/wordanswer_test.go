package musixmatch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestRichSyncWordAnswer pins the #982 word answer for each sub-call shape.
// Only an inner 404 is the provider saying "no word data"; a 401, a missing
// sub-call or an unreadable body must stay unknown, because absent is terminal.
// The synthetic fixtures are richsync_fetch_test.go's.
func TestRichSyncWordAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
		want models.WordAnswer
	}{
		{"served", richSyncCall(200, validRichSyncBody), models.WordAnswerServed},
		// A VALID body at 404, so only the status decides the answer.
		{"inner 404", richSyncCall(404, validRichSyncBody), models.WordAnswerAbsent},
		{"inner 401", richSyncCall(401, validRichSyncBody), models.WordAnswerUnknown},
		{"sub-call missing", "", models.WordAnswerUnknown},
		{"unparsable body", richSyncCall(200, `[{\"nope\":1}]`), models.WordAnswerUnknown},
		{"empty body", richSyncCall(200, ""), models.WordAnswerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newCountingClient(t, richSyncMacroBody(tc.call))
			song, err := client.FindLyrics(context.Background(), probeTrack())
			if err != nil {
				t.Fatalf("FindLyrics: %v", err)
			}
			if song.WordAnswer != tc.want {
				t.Errorf("WordAnswer = %q, want %q", song.WordAnswer, tc.want)
			}
			if (song.WordAnswer == models.WordAnswerServed) != (len(song.WordTimings) > 0) {
				t.Errorf("WordAnswer %q disagrees with %d attached timings", song.WordAnswer, len(song.WordTimings))
			}
		})
	}
}

// TestRichSyncWordAnswerWithoutCues: an unsynced result has no cues to bind
// words to, yet its sub-call's inner 404 is still a "no word data" answer.
func TestRichSyncWordAnswerWithoutCues(t *testing.T) {
	unsynced := func(status string) string {
		return `{"message":{"header":{"status_code":200},"body":{"macro_calls":{
		"matcher.track.get":{"message":{"header":{"status_code":200},"body":{"track":{
			"track_name":"title","artist_name":"artist","has_subtitles":0,"has_lyrics":1}}}},
		"track.lyrics.get":{"message":{"body":{"lyrics":{"lyrics_body":"alpha bravo"}}}},
		"track.subtitles.get":{"message":{"body":{}}},
		"track.richsync.get":{"message":{"header":{"status_code":` + status + `},"body":{}}}
		}}}}`
	}
	for status, want := range map[string]models.WordAnswer{
		"404": models.WordAnswerAbsent, "401": models.WordAnswerUnknown, "200": models.WordAnswerUnknown,
	} {
		client, _ := newCountingClient(t, unsynced(status))
		song, err := client.FindLyrics(context.Background(), probeTrack())
		if err != nil {
			t.Fatalf("inner %s: FindLyrics: %v", status, err)
		}
		if !strings.Contains(song.Lyrics.LyricsBody, "alpha") {
			t.Fatalf("inner %s: fixture did not yield an unsynced result", status)
		}
		if song.WordAnswer != want {
			t.Errorf("inner %s: WordAnswer = %q, want %q", status, song.WordAnswer, want)
		}
	}
}

func TestIsNoMatch(t *testing.T) {
	for err, want := range map[error]bool{
		ErrNotFound: true, fmt.Errorf("x: %w", ErrMatchMismatch): false, ErrUnmatchable: false,
		ErrMatcherClientError: false, ErrTruncatedResponse: false, ErrUnparsableSubtitleBody: false,
		fmt.Errorf("%w: restricted", ErrNoLyrics): false, ErrUnauthorized: false, nil: false,
	} {
		if got := IsNoMatch(err); got != want {
			t.Errorf("IsNoMatch(%v) = %v, want %v", err, got, want)
		}
	}
}
