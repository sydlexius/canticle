package innertube

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// chainFixtures is the full three-call chain: the captured search, next and
// browse responses that belong to one another. Every test that drives
// FindLyrics to completion serves all three.
func chainFixtures() map[string]string {
	return map[string]string{
		searchPath: "search.json",
		nextPath:   "next.json",
		browsePath: "browse.json",
	}
}

// fixtureTrack is the track the shipped search.json corresponds to. Asking for
// this one makes the correspondence gate PASS, which is what a happy-path test
// needs; asking for anything else is how the rejection tests below make it fail.
func fixtureTrack() models.Track {
	return models.Track{
		ArtistName: "Placeholder Artist Name",
		TrackName:  "Placeholder Song Title",
	}
}

// paths returns the request paths the server saw, in order. The ORDER is the
// assertion that matters for a composed chain: search must precede next, which
// must precede browse, since each consumes the previous one's output.
func paths(reqs []recordedRequest) []string {
	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.path)
	}
	return got
}

// requestFor returns the single recorded request issued to path. The chain
// issues each endpoint exactly once, so more than one is itself a defect.
func requestFor(t *testing.T, reqs []recordedRequest, path string) recordedRequest {
	t.Helper()
	var found []recordedRequest
	for _, r := range reqs {
		if r.path == path {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d requests to %s, want exactly 1", len(found), path)
	}
	return found[0]
}

// bodyString reads one string field out of a recorded JSON request body.
func bodyString(t *testing.T, r recordedRequest, key string) string {
	t.Helper()
	v, ok := r.body[key]
	if !ok {
		t.Fatalf("request to %s carried no %q field; body was %v", r.path, key, r.body)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("request to %s field %q = %T, want string", r.path, key, v)
	}
	return s
}

// --- AC: FindLyrics returns a populated Song for a fixture-backed happy path ---

func TestFindLyrics_HappyPath(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)

	song, err := c.FindLyrics(context.Background(), fixtureTrack())
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}

	if len(song.Subtitles.Lines) == 0 {
		t.Fatal("Subtitles.Lines is empty; the browse payload's timed cues did not survive the chain")
	}
	// The cue count comes from the browse fixture, so this pins that FindLyrics
	// hands the WHOLE payload to Decode rather than a truncated read.
	if got, want := len(song.Subtitles.Lines), 22; got != want {
		t.Errorf("Subtitles.Lines = %d, want %d (browse.json's cue count)", got, want)
	}
	// Cues must be timed. A zero Total on every line would mean the browse call
	// was served the WEB_REMIX untimed rendition -- the single failure doc.go
	// warns about -- and an assertion on line count alone would not see it.
	timed := false
	for _, ln := range song.Subtitles.Lines {
		if ln.Time.Total > 0 {
			timed = true
			break
		}
	}
	if !timed {
		t.Error("no line carries a non-zero timestamp; the result is untimed")
	}

	reqs := srv.snapshot()
	if got := paths(reqs); !slices.Equal(got, []string{searchPath, nextPath, browsePath}) {
		t.Errorf("call chain = %v, want search -> next -> browse", got)
	}

	// THE VALUES THREADED BETWEEN THE CALLS, not merely the fact that each call
	// happened. The fixture server dispatches on request PATH and ignores the
	// BODY, so without these three assertions every argument FindLyrics passes
	// from one call to the next is unverified -- and all three fields are
	// strings on structs whose neighbors are also strings, so the compiler
	// objects to none of the confusions.
	//
	// Measured against the un-asserted version of this test: passing
	// candidate.Title to Next instead of candidate.VideoID, passing the videoId
	// to Browse instead of next's browseId, and swapping Search's artist/title
	// arguments ALL left the suite fully green.
	//
	// The browse mix-up is the worst of the three. Next exists solely to
	// translate a videoId into an MPLY-prefixed lyrics browseId; a refactor
	// that hands Browse the videoId instead would fetch the wrong resource from
	// the real API while every test here reported success.
	if got, want := bodyString(t, requestFor(t, reqs, searchPath), "query"),
		"Placeholder Song Title Placeholder Artist Name"; got != want {
		t.Errorf("search query = %q, want %q (title then artist)", got, want)
	}
	if got, want := bodyString(t, requestFor(t, reqs, nextPath), "videoId"),
		"NrgmdOz227I"; got != want {
		t.Errorf("next videoId = %q, want %q -- the WINNING CANDIDATE's id", got, want)
	}
	browseID := bodyString(t, requestFor(t, reqs, browsePath), "browseId")
	if want := "MPLYt_Cn67yAcHym7-13"; browseID != want {
		t.Errorf("browse browseId = %q, want %q -- the browseId NEXT returned, "+
			"not the videoId that was sent to next", browseID, want)
	}
	if !strings.HasPrefix(browseID, "MPLY") {
		t.Errorf("browse browseId = %q, want the MPLY lyrics-tab prefix", browseID)
	}
}

// TestFindLyrics_StampsIdentityFromWinningCandidate pins that the returned Song
// identifies the track the provider actually served, not the one that was
// asked for. Decode is pure and returns only Subtitles, so a FindLyrics that
// forgot to stamp Track would return a song with a blank identity and every
// test above would still pass.
func TestFindLyrics_StampsIdentityFromWinningCandidate(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)

	// Album is carried in but not served by innertube's search response, so it
	// must survive from the local track rather than being blanked.
	req := fixtureTrack()
	req.AlbumName = "Placeholder Album"

	song, err := c.FindLyrics(context.Background(), req)
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Track.TrackName != "Placeholder Song Title" {
		t.Errorf("Track.TrackName = %q", song.Track.TrackName)
	}
	if song.Track.ArtistName != "Placeholder Artist Name" {
		t.Errorf("Track.ArtistName = %q", song.Track.ArtistName)
	}
	if song.Track.AlbumName != "Placeholder Album" {
		t.Errorf("Track.AlbumName = %q, want the local value preserved", song.Track.AlbumName)
	}
	if got, want := song.Track.TrackLength, 126; got != want {
		t.Errorf("Track.TrackLength = %d, want %d from the candidate", got, want)
	}
	if song.Track.HasSubtitles != 1 {
		t.Errorf("HasSubtitles = %d, want 1 for a timed result", song.Track.HasSubtitles)
	}
	// AudioDurationSeconds belongs to the FILE and must not be stamped from the
	// provider's catalog value -- doing so would make the accept-time timing
	// guard compare a lyric against the length it was timed against.
	if song.AudioDurationSeconds != 0 {
		t.Errorf("AudioDurationSeconds = %d, want 0; the provider must not supply it",
			song.AudioDurationSeconds)
	}
}

// --- AC: a rejected candidate returns a benign miss without issuing next/browse ---

func TestFindLyrics_RejectedCandidateIssuesOnlyTheSearch(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)

	// search.json answers ANY query with its one confident candidate -- that is
	// the trap doc.go documents. Asking for an unrelated track therefore still
	// gets that candidate back, and only the correspondence gate stands between
	// it and a wrong lyric.
	_, err := c.FindLyrics(context.Background(), models.Track{
		ArtistName: "Completely Different Artist",
		TrackName:  "Completely Different Song",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrNotFound", err)
	}

	// The cost claim in FindLyrics's doc comment: a miss costs ONE request. If
	// the gate ever moved after next(), this is what would catch it.
	if got := paths(srv.snapshot()); !slices.Equal(got, []string{searchPath}) {
		t.Errorf("call chain = %v, want the search alone; the gate must reject before next/browse", got)
	}
}

// TestFindLyrics_NoLyricsTabIsABenignMiss covers the other miss shape: the
// candidate corresponds, but the video has no lyrics rendition. It must reach
// the caller as a miss rather than a failure, and must not issue the browse
// call it has no browseId for.
func TestFindLyrics_NoLyricsTabIsABenignMiss(t *testing.T) {
	fx := chainFixtures()
	// A next response with no lyrics tab: served as a well-formed but
	// tab-less payload.
	fx[nextPath] = "next_no_lyrics_tab.json"
	srv := &fixtureServer{fixtures: fx}
	c := newTestClient(t, srv)

	_, err := c.FindLyrics(context.Background(), fixtureTrack())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrNotFound", err)
	}
	if !errors.Is(err, ErrNoLyricsTab) {
		t.Errorf("error = %v, want it to wrap ErrNoLyricsTab specifically", err)
	}
	if got := paths(srv.snapshot()); !slices.Equal(got, []string{searchPath, nextPath}) {
		t.Errorf("call chain = %v, want search -> next with no browse", got)
	}
}

// --- AC: the ctx-cancel path is tested ---

// TestFindLyrics_CancelBeforeTheFirstCall covers cancellation reaching the HTTP
// layer rather than the pacer: with pacing disabled there is no wait to
// interrupt, so the context must still stop the request itself.
func TestFindLyrics_CancelBeforeTheFirstCall(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FindLyrics(ctx, fixtureTrack())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
}

// --- AC: an undecidable request spends nothing ---

// TestFindLyrics_BlankTrackIssuesNoRequest pins the resource property: a track
// with neither artist nor title is refused from the input alone, before any I/O
// and before any pacing wait.
//
// Measured before the guard existed: FindLyrics(ctx, models.Track{}) issued one
// real request carrying an empty query, burned a full interval, and then failed
// in SelectCandidate with "no comparable field to verify the candidate
// against" -- a verdict that never needed the network. On a library backfill
// with sparse tags, a run of untagged files paid one interval each to ask
// Google nothing.
func TestFindLyrics_BlankTrackIssuesNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		track models.Track
	}{
		{"both fields empty", models.Track{}},
		{"both fields whitespace", models.Track{ArtistName: "  ", TrackName: "\t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fixtureServer{fixtures: chainFixtures()}
			c := newTestClient(t, srv)
			fc := withFakeClock(c)
			c.WithMinInterval(30 * time.Second)

			_, err := c.FindLyrics(context.Background(), tc.track)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want a miss wrapping ErrNotFound", err)
			}
			if n := len(srv.snapshot()); n != 0 {
				t.Errorf("requests issued = %d, want 0; an undecidable lookup must "+
					"not reach the network", n)
			}
			if len(fc.waits) != 0 {
				t.Errorf("pacer waited %v; a refused lookup must not spend an interval", fc.waits)
			}
		})
	}
}

// TestFindLyrics_OneBlankFieldStillSearches is the other side of that guard,
// and the reason it tests BOTH fields rather than either. A lookup with only a
// title (or only an artist) is perfectly verifiable -- the gate skips the blank
// field as non-comparable and holds the present one to the floor alone -- so
// refusing it would drop legitimate work.
func TestFindLyrics_OneBlankFieldStillSearches(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)

	// Title only, matching the fixture candidate so the gate admits it.
	_, err := c.FindLyrics(context.Background(), models.Track{
		TrackName: "Placeholder Song Title",
	})
	if err != nil {
		t.Fatalf("FindLyrics with a title but no artist: %v", err)
	}
	if n := len(srv.snapshot()); n == 0 {
		t.Error("no request issued; a single-field lookup is verifiable and must proceed")
	}
}

// --- trackFromCandidate, tested DIRECTLY ---
//
// It has to be tested here rather than only through FindLyrics, because the
// fixture cannot reach it. search.json's one candidate carries exactly the
// title and artist fixtureTrack() asks for -- it MUST, or the correspondence
// gate rejects it and the chain never reaches this function -- so at the
// FindLyrics level every assertion about the stamped identity compares a
// constant to itself and passes whether the value came from the candidate or
// from the local track.
//
// Measured against the FindLyrics-level test alone: deleting the title stamp,
// removing each of the three blank-guards, and flipping HasLyrics to 0 ALL
// left the suite green. The blank-guards are the function's entire stated
// purpose and had zero coverage.
func TestTrackFromCandidate(t *testing.T) {
	local := models.Track{
		TrackName:   "Local Title",
		ArtistName:  "Local Artist",
		AlbumName:   "Local Album",
		TrackLength: 200,
	}

	tests := []struct {
		name      string
		candidate SearchCandidate
		want      models.Track
	}{
		{
			// The candidate is authoritative when it has a value: it describes
			// what the provider actually served, which is not necessarily what
			// was asked for.
			name: "populated candidate overrides the local values",
			candidate: SearchCandidate{
				Title:           "Provider Title",
				Artist:          "Provider Artist",
				DurationSeconds: 126,
			},
			want: models.Track{
				TrackName:    "Provider Title",
				ArtistName:   "Provider Artist",
				AlbumName:    "Local Album",
				TrackLength:  126,
				HasLyrics:    1,
				HasSubtitles: 1,
			},
		},
		{
			// THE BLANK-GUARDS, which are why this function exists. A blank
			// candidate field must PRESERVE the local value, never overwrite it
			// with the blank. This is a live path, not a refactor hypothetical:
			// SelectCandidate rejects only an empty VideoID, and
			// checkCorresponds SKIPS a blank field as non-comparable, so a
			// candidate with a blank title and a matching artist clears the
			// gate and arrives here with Title == "".
			name: "blank candidate fields preserve the local values",
			candidate: SearchCandidate{
				Title:           "",
				Artist:          "",
				DurationSeconds: 0,
			},
			want: models.Track{
				TrackName:    "Local Title",
				ArtistName:   "Local Artist",
				AlbumName:    "Local Album",
				TrackLength:  200,
				HasLyrics:    1,
				HasSubtitles: 1,
			},
		},
		{
			// A negative duration is nonsense and must not be stamped. The
			// guard is `> 0`, not `!= 0`, and this is what distinguishes them.
			name: "a non-positive duration preserves the local length",
			candidate: SearchCandidate{
				Title:           "Provider Title",
				DurationSeconds: -5,
			},
			want: models.Track{
				TrackName:    "Provider Title",
				ArtistName:   "Local Artist",
				AlbumName:    "Local Album",
				TrackLength:  200,
				HasLyrics:    1,
				HasSubtitles: 1,
			},
		},
		{
			// Mixed: one field supplied, the others blank. Each guard must be
			// independent -- a single combined condition would fail this.
			name: "each field is guarded independently",
			candidate: SearchCandidate{
				Title:           "",
				Artist:          "Provider Artist",
				DurationSeconds: 0,
			},
			want: models.Track{
				TrackName:    "Local Title",
				ArtistName:   "Provider Artist",
				AlbumName:    "Local Album",
				TrackLength:  200,
				HasLyrics:    1,
				HasSubtitles: 1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := trackFromCandidate(tc.candidate, local)
			if got != tc.want {
				t.Errorf("trackFromCandidate() =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// --- the pacing COST claim, which is a claim about FindLyrics ---
//
// These two were dropped in the split that moved pacing to its own slice. The
// pacer's own arithmetic is tested there, but the claim fetcher.go makes to the
// operator -- that a MISS costs one interval and a HIT costs three -- is a
// statement about the composed chain and can only be tested here.

// TestFindLyrics_HitPaysThreeIntervals pins the expensive half of the cost
// claim. A hit issues three requests, so it waits twice: the first request
// finds no prior request to pace against.
func TestFindLyrics_HitPaysThreeIntervals(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)
	c.WithMinInterval(30 * time.Second)

	if _, err := c.FindLyrics(context.Background(), fixtureTrack()); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}

	if got, want := len(fc.waits), 2; got != want {
		t.Fatalf("pacer waited %d times %v, want %d (three requests, the first unpaced)",
			got, fc.waits, want)
	}
	for i, w := range fc.waits {
		if w != 30*time.Second {
			t.Errorf("wait %d = %v, want the full 30s interval", i, w)
		}
	}
}

// TestFindLyrics_MissPaysOneInterval pins the cheap half, and it is the half
// that matters for a fallback lane whose traffic is mostly misses: the
// correspondence gate rejects before next or browse is issued, so a track this
// provider has nothing for costs a single request and no wait at all.
func TestFindLyrics_MissPaysOneInterval(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)
	fc := withFakeClock(c)
	c.WithMinInterval(30 * time.Second)

	_, err := c.FindLyrics(context.Background(), models.Track{
		ArtistName: "Completely Different Artist",
		TrackName:  "Completely Different Song",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want a miss", err)
	}
	if len(fc.waits) != 0 {
		t.Errorf("pacer waited %v; a one-request miss must not wait", fc.waits)
	}
	if n := len(srv.snapshot()); n != 1 {
		t.Errorf("requests issued = %d, want 1; the gate must reject before "+
			"next/browse", n)
	}
}

// --- the TAIL of the chain: Browse's error class, Decode's error, per-call ctx ---
//
// These three cover the region a hostile review found entirely untested. The
// survivors clustered rather than scattering: every test above drives the chain
// only as far as the FIRST thing that goes wrong, so the last calls were
// covered by nothing.

// TestFindLyrics_BrowseFailurePreservesItsErrorClass stops a transport failure
// from being laundered into a benign miss.
//
// errors.go goes to real length to keep ErrRateLimited, ErrForbidden and
// ErrNotFound distinguishable, so the lane circuit breaker can bucket them.
// Measured: wrapping Browse's error in ErrNotFound left the whole suite green,
// while the identical mutation on Search reddens -- Search is incidentally
// covered by the cancel test, and Browse was covered by nothing.
//
// The consequence of that misclassification runs the wrong way: a browse-stage 429
// read as a clean miss never trips the breaker, so the lane keeps hammering a
// throttling gateway while reporting healthy misses.
func TestFindLyrics_BrowseFailurePreservesItsErrorClass(t *testing.T) {
	srv := &fixtureServer{
		fixtures:  chainFixtures(),
		statusFor: map[string]int{browsePath: 429},
	}
	c := newTestClient(t, srv)

	_, err := c.FindLyrics(context.Background(), fixtureTrack())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want it to wrap ErrRateLimited", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a browse-stage 429 also wrapped ErrNotFound; a throttle laundered " +
			"into a benign miss never trips the breaker, so the lane keeps " +
			"hammering a gateway that is refusing it")
	}
	// The chain must have reached browse for this to have tested anything.
	if got := paths(srv.snapshot()); !slices.Equal(got, []string{searchPath, nextPath, browsePath}) {
		t.Errorf("call chain = %v, want the full chain through browse", got)
	}
}

// TestFindLyrics_DecodeFailureIsNotASilentEmptyHit is the false-accept guard.
//
// Decode legitimately errors on a reachable production condition: a 200 whose
// lyrics section carries no cues. Measured, both of these left the suite green:
// dropping Decode's error check, and returning an empty Song with a nil error.
//
// The second is the dangerous shape. A nil error reads as a HIT, so the caller
// receives a Song carrying a confident stamped identity -- artist, title,
// HasLyrics=1, HasSubtitles=1 -- and ZERO lines. That is the silent-corruption
// class the correspondence gate exists to prevent, reached through the back
// door, past the gate.
func TestFindLyrics_DecodeFailureIsNotASilentEmptyHit(t *testing.T) {
	fx := chainFixtures()
	fx[browsePath] = "browse_no_cues.json"
	srv := &fixtureServer{fixtures: fx}
	c := newTestClient(t, srv)

	song, err := c.FindLyrics(context.Background(), fixtureTrack())
	if err == nil {
		t.Fatalf("error = nil for a cue-less browse response; got song with %d "+
			"lines and Track %+v. A nil error reads as a HIT, so this returns a "+
			"confident identity with no lyrics behind it",
			len(song.Subtitles.Lines), song.Track)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap ErrNotFound; a clean 200 carrying "+
			"nothing usable is a miss, not a transport failure", err)
	}
	if len(song.Subtitles.Lines) != 0 {
		t.Errorf("returned %d lines alongside an error", len(song.Subtitles.Lines))
	}
}

// ctxMarkerKey is a context key used to prove which context reached a request.
type ctxMarkerKey struct{}

// ctxRecorder is an http.RoundTripper that records, per request path, whether
// that request's context carried the marker value.
type ctxRecorder struct {
	base   http.RoundTripper
	mu     sync.Mutex
	marked map[string]bool
}

func (r *ctxRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	if r.marked == nil {
		r.marked = map[string]bool{}
	}
	_, ok := req.Context().Value(ctxMarkerKey{}).(string)
	r.marked[req.URL.Path] = ok
	r.mu.Unlock()
	return r.base.RoundTrip(req)
}

func (r *ctxRecorder) snapshot() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.marked))
	for k, v := range r.marked {
		out[k] = v
	}
	return out
}

// TestFindLyrics_EveryCallCarriesTheCallersContext covers ctx on EVERY call,
// which is what fetcher.go's "every step is ctx-cancellable" actually claims.
//
// A CANCELLATION test cannot do this, and that is the point worth recording.
// Canceling and asserting on the returned error only ever proves that SOME
// call honored ctx: measured, dropping ctx from Search alone left the suite
// green, because the search then succeeded against a canceled context and Next
// -- which still had ctx -- returned the expected error. Moving the cancel
// mid-chain does not fix it either, it moves the blind spot one call along: a
// cancel delivered at next makes Next itself fail, so browse is never issued
// and a Browse mutation is unreachable. Each call ends up covered only by the
// survival of the ones after it, and the last call by nothing.
//
// So this observes the context DIRECTLY, at the transport, rather than
// inferring it from an error. A marker value on the caller's context reaches
// every request built from it; a call handed context.Background() instead
// carries no marker. That is deterministic -- no cancellation race -- and it
// fails for exactly one reason.
func TestFindLyrics_EveryCallCarriesTheCallersContext(t *testing.T) {
	srv := &fixtureServer{fixtures: chainFixtures()}
	c := newTestClient(t, srv)
	rec := &ctxRecorder{base: c.httpClient.Transport}
	if rec.base == nil {
		rec.base = http.DefaultTransport
	}
	c.httpClient.Transport = rec

	ctx := context.WithValue(context.Background(), ctxMarkerKey{}, "caller")
	if _, err := c.FindLyrics(ctx, fixtureTrack()); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}

	marked := rec.snapshot()
	for _, path := range []string{searchPath, nextPath, browsePath} {
		seen, issued := marked[path]
		if !issued {
			t.Errorf("no request recorded for %s; the chain did not reach it", path)
			continue
		}
		if !seen {
			t.Errorf("the request to %s did NOT carry the caller's context. That "+
				"call was handed a fresh context, so a cancellation or deadline on "+
				"the caller's ctx cannot stop it -- an operator's shutdown would "+
				"not prevent this outbound request", path)
		}
	}
}

// TestClient_SatisfiesTheFetcherSignature is a compile-time check that this
// client is usable as a provider. The providers package is NOT imported (it
// would be an unnecessary dependency edge from an adapter to the abstraction
// that wraps it), so the method set is asserted against a local mirror of
// providers.LyricsProvider instead.
func TestClient_SatisfiesTheFetcherSignature(t *testing.T) {
	type lyricsProvider interface {
		FindLyrics(ctx context.Context, track models.Track) (models.Song, error)
		Name() string
	}
	var _ lyricsProvider = NewClient()
}
