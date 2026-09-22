package petitlyrics

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// payload serves one synthetic song carrying p.
func payload(p string) func(t *testing.T) http.HandlerFunc {
	return func(t *testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tierEnvelope(t, []byte(p))) }
	}
}

// TestFindLyrics_WordAnswer pins the #982 word answer per payload tier, over
// the existing synthetic fixtures. The API returns the highest tier a track has,
// so a line-sync or plain-text answer is "no word data"; timings dropped for a
// split cue are unknown, because the provider DID serve words.
func TestFindLyrics_WordAnswer(t *testing.T) {
	split := `<wsy><line><linestring>[00:09.00]Lorem ipsum</linestring>` +
		`<word><starttime>3000</starttime><endtime>4000</endtime><wordstring>Lorem</wordstring></word>` +
		`</line></wsy>`
	word := `<wsy><line><linestring>Lorem</linestring>` +
		`<word><starttime>3000</starttime><endtime>4000</endtime><wordstring>Lorem</wordstring></word></line></wsy>`
	lineSync := func(t *testing.T) http.HandlerFunc {
		blob := buildLSY(t, 0xdc18, true, []int{100, 250})
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if r.PostForm.Get("lyricsType") == "1" {
				_, _ = w.Write(tierEnvelope(t, []byte("alpha\nbeta\n")))
				return
			}
			_, _ = w.Write(tierEnvelope(t, blob))
		}
	}
	for _, tc := range []struct {
		name    string
		handler func(t *testing.T) http.HandlerFunc
		want    models.WordAnswer
	}{
		{"word tier", func(t *testing.T) http.HandlerFunc { return serveFixture(t, "type3_wordsync.xml") }, models.WordAnswerServed},
		{"line tier", lineSync, models.WordAnswerAbsent},
		{"unsynced tier", func(t *testing.T) http.HandlerFunc { return serveFixture(t, "type1_unsynced.xml") }, models.WordAnswerAbsent},
		{"split cue drops timings", payload(split), models.WordAnswerUnknown},
		// encoding/xml accepts a BOM, so a BOM-prefixed <wsy> is a word payload.
		{"BOM-prefixed word tier", payload("\xEF\xBB\xBF" + word), models.WordAnswerServed},
		// Markup the decoder does not recognize is undecidable, never absent.
		{"renamed root", payload(strings.Replace(word, "wsy>", "wsx>", 2)), models.WordAnswerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.handler(t))
			song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
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

// TestFindLyrics_OnlyNoSongsIsANoMatch: a decode failure or an empty line-sync
// continuation still wraps ErrNotFound, but only "no songs" is a no-match (#982).
func TestFindLyrics_OnlyNoSongsIsANoMatch(t *testing.T) {
	emptyContinuation := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("lyricsType") == "1" {
			serveFixture(t, "empty.xml")(w, r)
			return
		}
		_, _ = w.Write(tierEnvelope(t, buildLSY(t, 0xdc18, true, []int{100, 250})))
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		noMatch bool
	}{
		{"no songs", serveFixture(t, "empty.xml"), true},
		{"word payload with no timed lines", payload("<wsy></wsy>")(t), false},
		{"empty line-sync continuation", emptyContinuation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.handler)
			_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
			if !errors.Is(err, ErrNotFound) || IsNoMatch(err) != tc.noMatch {
				t.Errorf("err %v: IsNoMatch = %v, want %v (and ErrNotFound)", err, IsNoMatch(err), tc.noMatch)
			}
		})
	}
}
