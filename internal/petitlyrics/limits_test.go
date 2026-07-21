package petitlyrics

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// TestReadBody_RejectsOversizedResponse pins the response cap. Word-sync
// payloads legitimately run to a few hundred KB, so the ceiling is high; a
// response above it should fail loudly rather than being buffered into memory.
func TestReadBody_RejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("x", maxResponseSize+10)
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(huge))
	})
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x"})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized response should be rejected, got %v", err)
	}
}

func TestStatusError_UnexpectedStatus(t *testing.T) {
	err := statusError(http.StatusInternalServerError)
	if err == nil {
		t.Fatal("500 should be an error")
	}
	// A 500 is not any of the typed classes: it must not be silently treated as
	// a rate limit or a miss.
	for _, sentinel := range []error{ErrRateLimited, ErrForbidden, ErrUnauthorized, ErrNotFound} {
		if errors.Is(err, sentinel) {
			t.Errorf("500 must not classify as %v", sentinel)
		}
	}
}

// TestLookup_BadBase64 covers the payload-decode failure path: a malformed
// lyricsData must surface as an error, never as an empty-but-successful song.
func TestLookup_BadBase64(t *testing.T) {
	body := fmt.Sprintf(`<response><songs><song><lyricsId>1</lyricsId>`+
		`<title>Lorem</title><lyricsData>%s</lyricsData></song></songs></response>`,
		"!!!not-base64!!!")
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem"})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("want a base64 decode error, got %v", err)
	}
}

// TestLookup_EmptyPayloadIsMiss: a song element with no payload is a miss, not
// a malformed-response error.
func TestLookup_EmptyPayloadIsMiss(t *testing.T) {
	body := `<response><songs><song><lyricsId>1</lyricsId><title>Lorem</title>` +
		`<lyricsData></lyricsData></song></songs></response>`
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestLookup_PlainTextWithTimestampsIsPromoted: an unsynced-tier payload that
// nonetheless carries LRC timestamps should be read as synced rather than
// written out as a plain body.
func TestLookup_PlainTextWithTimestampsIsPromoted(t *testing.T) {
	lrc := "[00:01.00]Lorem ipsum\n[00:05.50]Dolor sit amet\n"
	payload := base64.StdEncoding.EncodeToString([]byte(lrc))
	body := fmt.Sprintf(`<response><songs><song><lyricsId>1</lyricsId>`+
		`<title>Lorem</title><lyricsData>%s</lyricsData></song></songs></response>`, payload)
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Errorf("timestamped plain text should promote to synced cues, got %d lines / body=%q",
			len(song.Subtitles.Lines), song.Lyrics.LyricsBody)
	}
}

func TestXMLRootPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no prologue", "<wsy>", "<wsy>"},
		{"leading whitespace", "  \n\t<wsy>", "<wsy>"},
		{"xml declaration", `<?xml version="1.0"?><wsy>`, "<wsy>"},
		{"declaration then whitespace", "<?xml version=\"1.0\"?>\n  <wsy>", "<wsy>"},
		{"comment", "<!-- note --><wsy>", "<wsy>"},
		{"declaration and comment", `<?xml version="1.0"?><!-- c --><wsy>`, "<wsy>"},
		// Unterminated prologues must return what is left rather than looping or
		// slicing out of range.
		{"unterminated declaration", "<?xml version", "<?xml version"},
		{"unterminated comment", "<!-- dangling", "<!-- dangling"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(xmlRootPrefix([]byte(tc.in))); got != tc.want {
				t.Errorf("xmlRootPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCtxSleep(t *testing.T) {
	if !ctxSleep(context.Background(), time.Millisecond) {
		t.Error("a completed sleep should report true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctxSleep(ctx, time.Hour) {
		t.Error("a canceled context should abort the sleep and report false")
	}
}

// TestDecodeWordSync_LineStringFallback: when <linestring> is absent the cue
// text is reconstructed from the words, so a timed cue is never emitted empty.
func TestDecodeWordSync_LineStringFallback(t *testing.T) {
	doc := `<wsy><line><wordnum>2</wordnum>` +
		`<word><starttime>100</starttime><endtime>200</endtime><wordstring>Lorem</wordstring></word>` +
		`<word><starttime>200</starttime><endtime>300</endtime><wordstring>Ipsum</wordstring></word>` +
		`</line></wsy>`
	cues, _, err := decodeWordSync([]byte(doc))
	if err != nil {
		t.Fatalf("decodeWordSync: %v", err)
	}
	if len(cues) != 1 || cues[0].Text == "" {
		t.Errorf("missing <linestring> should fall back to joined words, got %+v", cues)
	}
}
