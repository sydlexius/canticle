package musixmatch

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// matcherStatusBody builds a macro envelope whose HTTP-level response is 200
// but whose inner matcher.track.get status_code is the given code, with no
// track body -- the shape a client-error status carries in practice (the
// matcher never returns a track alongside an error status).
func matcherStatusBody(code int) string {
	return `{"message": {"header": {"status_code": 200}, "body": {"macro_calls": {
		"matcher.track.get": {"message": {"header": {"status_code": ` +
		strconv.Itoa(code) + `}}},
		"track.lyrics.get": {"message": {"body": {}}},
		"track.subtitles.get": {"message": {"body": {}}}
	}}}}`
}

// countingRoundTripper wraps a roundTripFunc and counts every call, so a test
// can assert that NO outbound request was issued (the pre-flight guard's whole
// point is to avoid spending one).
type countingRoundTripper struct {
	calls int
	fn    roundTripFunc
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls++
	return c.fn(req)
}

// TestBlankTitleTrackNeverIssuesRequest is the #479 core guard: a track with no
// title and no alternate identifier must be rejected BEFORE any HTTP call, not
// merely classified favorably after one. The counting transport proves the
// round trip was never attempted.
func TestBlankTitleTrackNeverIssuesRequest(t *testing.T) {
	crt := &countingRoundTripper{fn: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "Some Artist", TrackName: "",
	})
	if err == nil {
		t.Fatal("FindLyrics accepted a titleless track with no alternate identifier")
	}
	if !errors.Is(err, ErrUnmatchable) {
		t.Fatalf("error = %v; want errors.Is(_, ErrUnmatchable)", err)
	}
	if !IsBenignMiss(err) {
		t.Fatal("IsBenignMiss(ErrUnmatchable) = false; want true (a stable, bounded-retry outcome)")
	}
	if crt.calls != 0 {
		t.Fatalf("outbound requests = %d; want 0 (the guard must pre-empt the request entirely)", crt.calls)
	}
}

// TestWhitespaceOnlyTitleIsUnmatchable: a real tagger can write a title field
// containing only whitespace (a stray space, a placeholder from a ripping
// tool). That is exactly as unmatchable as a bare "" and must not slip past the
// guard on a bare-emptiness check.
func TestWhitespaceOnlyTitleIsUnmatchable(t *testing.T) {
	crt := &countingRoundTripper{fn: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "Some Artist", TrackName: "   ",
	})
	if !errors.Is(err, ErrUnmatchable) {
		t.Fatalf("error = %v; want errors.Is(_, ErrUnmatchable) for a whitespace-only title", err)
	}
	if crt.calls != 0 {
		t.Fatalf("outbound requests = %d; want 0", crt.calls)
	}
}

// TestBlankTitleWithISRCStillIssuesRequest: the guard must be REVIVABLE and
// must not block a track that carries an alternate identifier. A blank title
// paired with an ISRC is a legitimate lookup (the matcher can resolve on ISRC
// alone), so it must reach the transport exactly like any other query.
func TestBlankTitleWithISRCStillIssuesRequest(t *testing.T) {
	crt := &countingRoundTripper{fn: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "", ISRC: "USRC17607839",
	})
	if err != nil {
		t.Fatalf("FindLyrics rejected a blank-title track carrying an ISRC: %v", err)
	}
	if crt.calls != 1 {
		t.Fatalf("outbound requests = %d; want 1 (ISRC alone is a matchable query)", crt.calls)
	}
}

// TestBlankTitleWithSpotifyIDStillIssuesRequest mirrors the ISRC case for the
// other alternate identifier the matcher accepts.
func TestBlankTitleWithSpotifyIDStillIssuesRequest(t *testing.T) {
	crt := &countingRoundTripper{fn: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		TrackName: "", SpotifyID: "4uLU6hMCjMI75M1A2tKUQC",
	})
	if err != nil {
		t.Fatalf("FindLyrics rejected a blank-title track carrying a Spotify id: %v", err)
	}
	if crt.calls != 1 {
		t.Fatalf("outbound requests = %d; want 1 (a Spotify id alone is a matchable query)", crt.calls)
	}
}

// TestMatcher4xxClassifiesStable covers the belt-and-braces half of #479: any
// matcher inner status_code in the 4xx range (that this client does not already
// name -- 401 and 404 are handled explicitly) must classify as a stable benign
// miss rather than the genuine/transient default, EXCEPT 429 (see the
// dedicated test below).
func TestMatcher4xxClassifiesStable(t *testing.T) {
	for _, code := range []int{400, 403, 410, 422, 499} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			client := newTestClient(http.StatusOK, matcherStatusBody(code))
			_, err := client.FindLyrics(context.Background(), models.Track{
				ArtistName: "artist", TrackName: "title",
			})
			if err == nil {
				t.Fatalf("matcher status_code %d: FindLyrics returned nil error", code)
			}
			if !errors.Is(err, ErrMatcherClientError) {
				t.Fatalf("matcher status_code %d: error = %v; want errors.Is(_, ErrMatcherClientError)", code, err)
			}
			if !IsBenignMiss(err) {
				t.Fatalf("matcher status_code %d: IsBenignMiss = false; want true (stable, bounded-retry)", code)
			}
		})
	}
}

// TestMatcher429StaysTransient is the regression this issue calls out by name:
// a rate limit must NEVER be folded into the stable-4xx path, or the client
// would stop backing off from a throttling provider and hammer it instead.
func TestMatcher429StaysTransient(t *testing.T) {
	client := newTestClient(http.StatusOK, matcherStatusBody(http.StatusTooManyRequests))
	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "title",
	})
	if err == nil {
		t.Fatal("matcher status_code 429: FindLyrics returned nil error")
	}
	if errors.Is(err, ErrMatcherClientError) {
		t.Fatalf("matcher status_code 429 classified as ErrMatcherClientError: %v; 429 must stay on the transient path", err)
	}
	if IsBenignMiss(err) {
		t.Fatal("matcher status_code 429: IsBenignMiss = true; want false (429 must keep its geometric backoff)")
	}
}

// TestMatcher5xxStaysTransient confirms the belt-and-braces classification does
// not accidentally widen to swallow server errors, which are genuinely
// transient and must keep their existing backoff.
func TestMatcher5xxStaysTransient(t *testing.T) {
	client := newTestClient(http.StatusOK, matcherStatusBody(500))
	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "title",
	})
	if err == nil {
		t.Fatal("matcher status_code 500: FindLyrics returned nil error")
	}
	if IsBenignMiss(err) {
		t.Fatal("matcher status_code 500: IsBenignMiss = true; want false (a 5xx is a genuine transient failure)")
	}
}

// TestBareInner401StillUnauthorizedNotBenign guards against the 4xx-stable
// change accidentally widening to swallow the existing, deliberately-NOT-benign
// bare-401 path (#554): a bare 401 is treated as a possible egress throttle,
// not a dead credential, and must keep tripping the circuit breaker rather than
// classify as a stable miss.
func TestBareInner401StillUnauthorizedNotBenign(t *testing.T) {
	client := newTestClient(http.StatusOK, matcherStatusBody(http.StatusUnauthorized))
	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "title",
	})
	if err == nil {
		t.Fatal("matcher status_code 401: FindLyrics returned nil error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("matcher status_code 401: error = %v; want errors.Is(_, ErrUnauthorized)", err)
	}
	if IsBenignMiss(err) {
		t.Fatal("matcher status_code 401: IsBenignMiss = true; want false, 401 must keep its throttle/circuit-breaker handling (#554)")
	}
}

// TestNormalMatchIsUnaffected is the plain-success counterweight: a normal
// title+artist query with a clean 200 match must be completely unaffected by
// the #479 changes.
func TestNormalMatchIsUnaffected(t *testing.T) {
	client := newTestClient(http.StatusOK, minimalMatchBody)
	song, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "title",
	})
	if err != nil {
		t.Fatalf("normal match path failed: %v", err)
	}
	if song.Track.TrackName != "title" {
		t.Fatalf("song.Track.TrackName = %q; want %q", song.Track.TrackName, "title")
	}
}

// TestWhitespaceTitleWithISRCSendsTrimmedQuery pins the guard's trimming as
// something the REQUEST honors, not merely something the admission decision
// consults. A whitespace-only title alongside a valid ISRC is admitted (the
// ISRC alone is matchable), so the request goes out -- and q_track must carry
// the trimmed empty value rather than the raw spaces, since sending a query
// the guard's own semantics call empty invites exactly the avoidable 4xx the
// #479 work exists to prevent.
//
// The assertion is on the OUTBOUND PARAM, not on a return value: trimming only
// where hasMatchableIdentity reads would leave this test green while the wire
// still carried "   ".
func TestWhitespaceTitleWithISRCSendsTrimmedQuery(t *testing.T) {
	var gotTrack string
	var seen bool
	crt := &countingRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		gotTrack = req.URL.Query().Get("q_track")
		seen = true
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "   ", ISRC: "USRC17607839",
	})
	if err != nil {
		t.Fatalf("FindLyrics rejected a whitespace-title track carrying an ISRC: %v", err)
	}
	if !seen {
		t.Fatal("no outbound request observed; the ISRC alone should be matchable")
	}
	if gotTrack != "" {
		t.Fatalf("outbound q_track = %q; want %q (the guard trims, so the request must too)", gotTrack, "")
	}
}

// TestWhitespaceISRCNotSentAsDisambiguator is the same contract for the
// alternate identifier: track_isrc is added only when non-empty, and a
// whitespace-only ISRC must count as empty there for the same reason it does
// in the guard. Without trimming before the params are built, the raw spaces
// pass the `!= ""` check and a meaningless track_isrc goes on the wire.
func TestWhitespaceISRCNotSentAsDisambiguator(t *testing.T) {
	var hasISRC bool
	crt := &countingRoundTripper{fn: func(req *http.Request) (*http.Response, error) {
		_, hasISRC = req.URL.Query()["track_isrc"]
		return jsonResponse(http.StatusOK, minimalMatchBody), nil
	}}
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: crt}

	_, err := client.FindLyrics(context.Background(), models.Track{
		ArtistName: "artist", TrackName: "title", ISRC: "   ",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if hasISRC {
		t.Fatal("outbound request carried track_isrc for a whitespace-only ISRC; want it omitted")
	}
}
