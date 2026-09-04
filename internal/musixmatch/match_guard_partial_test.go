package musixmatch

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestFindLyricsRejectsPartialMatch covers the false-accept class the original
// both-fields-must-fail rule let through (hostile review, #838).
//
// The rule was justified by the claim that legitimate variance "always keeps one
// field close". Measured against normalize.MatchConfidence, that is FALSE in the
// direction that matters: legitimate variance keeps BOTH fields high (weakest
// measured legitimate field 0.8051), while these two cases hold one field at 1.0
// and the other near 0.5. An OR rule cannot separate them; an AND rule can, with
// a wider margin than the OR rule ever had.
//
// Both are realistic, not contrived: a cover keeps the title and changes the
// artist, and a wrongly-served track on a compilation keeps the artist and
// changes the title. Either writes another song's words to disk, which the writer's
// format-transition rule can make unrecoverable.
func TestFindLyricsRejectsPartialMatch(t *testing.T) {
	cases := []struct {
		name            string
		qArtist, qTitle string
		gArtist, gTitle string
	}{
		{"cover: title matches, artist does not", "Aurora Kestrel", "Marigold Drift", "Bramblewood Quintet", "Marigold Drift"},
		{"wrong track: artist matches, title does not", "Aurora Kestrel", "Marigold Drift", "Aurora Kestrel", "Ninefold Ascent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := clientReturning(t, matchResponse(c.gArtist, c.gTitle))
			_, err := client.FindLyrics(context.Background(), models.Track{
				ArtistName: c.qArtist, TrackName: c.qTitle,
			})
			if err == nil {
				t.Fatal("accepted a response matching only one field; the other is a different song")
			}
			if !errors.Is(err, ErrMatchMismatch) {
				t.Fatalf("error = %v; want ErrMatchMismatch", err)
			}
		})
	}
}

// TestFindLyricsSingleComparableFieldStillChecked: when only one field is
// comparable (the other blank), that field must still be checked. Failing open
// on a blank field must not become failing open on the whole guard.
func TestFindLyricsSingleComparableFieldStillChecked(t *testing.T) {
	client := clientReturning(t, matchResponse("Bramblewood Quintet", "Ninefold Ascent"))
	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "", TrackName: "Marigold Drift",
	})
	if err == nil {
		t.Fatal("accepted a wrong title when it was the ONLY comparable field")
	}
	if !errors.Is(err, ErrMatchMismatch) {
		t.Fatalf("error = %v; want ErrMatchMismatch", err)
	}
}

// TestFindLyricsSingleComparableFieldAccepts is the counterweight: one blank
// field must not block an otherwise-good match.
func TestFindLyricsSingleComparableFieldAccepts(t *testing.T) {
	client := clientReturning(t, matchResponse("Bramblewood Quintet", "Marigold Drift"))
	song, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "", TrackName: "Marigold Drift",
	})
	if err != nil {
		t.Fatalf("rejected a good title match with a blank artist: %v", err)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Errorf("lines = %d; want 1", len(song.Subtitles.Lines))
	}
}

// TestNewSentinelsAreBenignMisses covers the fetch-mode half of the retry-burn
// fix. internal/app (the `canticle fetch` CLI) calls musixmatch.IsBenignMiss
// DIRECTLY and never reaches orchestrator.ClassifyOutcome, so classifying these
// only in the orchestrator left the CLI path backing off geometrically -- 1s,
// 2s, 4s ... toward the 1h cap -- on a condition no wait can fix.
//
// ErrTruncatedResponse is included because it has the same shape and the same
// pre-existing gap (#496): the orchestrator classifies it as a benign miss while
// fetch mode does not.
func TestNewSentinelsAreBenignMisses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unparsable subtitle body", ErrUnparsableSubtitleBody},
		{"match mismatch", ErrMatchMismatch},
		{"truncated response", ErrTruncatedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsBenignMiss(tc.err) {
				t.Errorf("IsBenignMiss(%v) = false; a deterministic per-request condition must not "+
					"trip the fetch-mode geometric backoff", tc.err)
			}
			// Wrapped, the way a lane returns it in production.
			if !IsBenignMiss(errWrap(tc.err)) {
				t.Errorf("IsBenignMiss(wrapped %v) = false; callers use errors.Is throughout", tc.err)
			}
		})
	}
}

// TestGenuineFailuresAreNotBenignMisses is the counterweight: widening
// IsBenignMiss must not swallow a real auth or transport fault, which WOULD
// legitimately warrant backoff.
func TestGenuineFailuresAreNotBenignMisses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unauthorized", ErrUnauthorized},
		{"rate limited", ErrRateLimited},
		{"token renewal required", ErrTokenRenewalRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if IsBenignMiss(tc.err) {
				t.Errorf("IsBenignMiss(%v) = true; a genuine failure must keep its backoff", tc.err)
			}
		})
	}
}

func errWrap(err error) error { return wrapped{err} }

type wrapped struct{ err error }

func (w wrapped) Error() string { return "lane: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }

// TestFindLyricsArtistOnlyComparable mirrors the title-only cases for the
// ARTIST-only path (CodeRabbit, PR #840): a blank TrackName with a non-empty
// ArtistName. Both directions are covered so a fail-open regression on this
// branch cannot pass the suite -- the title-only tests alone would not catch it,
// since fieldCorresponds is called once per field and either call could regress
// independently.
func TestFindLyricsArtistOnlyComparable(t *testing.T) {
	t.Run("rejects a wrong artist when it is the only comparable field", func(t *testing.T) {
		client := clientReturning(t, matchResponse("Bramblewood Quintet", "Ninefold Ascent"))
		_, err := client.FindLyrics(context.Background(), models.Track{
			ArtistName: "Aurora Kestrel", TrackName: "",
		})
		if err == nil {
			t.Fatal("accepted a wrong artist when it was the ONLY comparable field")
		}
		if !errors.Is(err, ErrMatchMismatch) {
			t.Fatalf("error = %v; want ErrMatchMismatch", err)
		}
	})

	t.Run("accepts a good artist with a blank title", func(t *testing.T) {
		client := clientReturning(t, matchResponse("Aurora Kestrel", "Ninefold Ascent"))
		song, err := client.FindLyrics(context.Background(), models.Track{
			ArtistName: "Aurora Kestrel", TrackName: "",
		})
		if err != nil {
			t.Fatalf("rejected a good artist match with a blank title: %v", err)
		}
		if len(song.Subtitles.Lines) != 1 {
			t.Errorf("lines = %d; want 1", len(song.Subtitles.Lines))
		}
	})
}
