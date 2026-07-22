package petitlyrics

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// fixtureServer serves a fixture file for every request and records what the
// client sent.
type fixtureServer struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	userAgent   string
	contentType string
	form        map[string]string
}

func (f *fixtureServer) record(r *http.Request) {
	_ = r.ParseForm()
	form := map[string]string{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			form[k] = v[0]
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		userAgent:   r.Header.Get("User-Agent"),
		contentType: r.Header.Get("Content-Type"),
		form:        form,
	})
}

func (f *fixtureServer) snapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// newTestClient wires a Client at an httptest server that replies with the given
// fixture (by tier) or a fixed status.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *fixtureServer) {
	t.Helper()
	fs := &fixtureServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL
	return c, fs
}

func serveFixture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write(body)
	}
}

// TestFindLyrics_SendsIdentifyingUserAgent is the direct regression test for the
// root cause of issue #495. The previous client sent Go's default User-Agent,
// which the service denylists with HTTP 403 on every request. A request that
// goes out without an identifying UA is the bug, so assert on the wire.
func TestFindLyrics_SendsIdentifyingUserAgent(t *testing.T) {
	c, fs := newTestClient(t, serveFixture(t, "type3_wordsync.xml"))
	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"}); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	reqs := fs.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("want exactly 1 request, got %d", len(reqs))
	}
	ua := reqs[0].userAgent
	if ua == "" || strings.HasPrefix(ua, "Go-http-client") {
		t.Errorf("client must send an identifying User-Agent, got %q", ua)
	}
	if !strings.Contains(ua, "canticle") {
		t.Errorf("User-Agent should identify canticle, got %q", ua)
	}
}

// TestFindLyrics_OneRequestPerTrack pins the efficiency property that motivated
// the rewrite: the API returns metadata and payload together, so a successful
// lookup costs one request (the scrape it replaced cost three).
func TestFindLyrics_OneRequestPerTrack(t *testing.T) {
	c, fs := newTestClient(t, serveFixture(t, "type3_wordsync.xml"))
	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"}); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if n := len(fs.snapshot()); n != 1 {
		t.Errorf("want 1 request per track, got %d", n)
	}
}

func TestFindLyrics_SendsRequiredParams(t *testing.T) {
	c, fs := newTestClient(t, serveFixture(t, "type3_wordsync.xml"))
	track := models.Track{TrackName: "Lorem Ipsum", ArtistName: "Dolor Sit", AlbumName: "Amet"}
	if _, err := c.FindLyrics(context.Background(), track); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	form := fs.snapshot()[0].form
	for _, k := range []string{"clientAppId", "terminalType", "lyricsType", "key_title", "key_artist", "key_album"} {
		if form[k] == "" {
			t.Errorf("form is missing required param %q (got %v)", k, form)
		}
	}
	if form["key_title"] != "Lorem Ipsum" || form["key_artist"] != "Dolor Sit" {
		t.Errorf("track metadata not forwarded: %v", form)
	}
	// Word-sync is requested first because it is a superset of line-sync and its
	// payload is decodable, unlike the encrypted line-sync tier.
	if form["lyricsType"] != "3" {
		t.Errorf("first request should ask for word-sync, got lyricsType=%q", form["lyricsType"])
	}
}

func TestFindLyrics_WordSyncProducesCues(t *testing.T) {
	c, _ := newTestClient(t, serveFixture(t, "type3_wordsync.xml"))
	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) == 0 {
		t.Fatal("word-sync response should yield synced lines")
	}
	if song.Lyrics.LyricsBody != "" {
		t.Error("a synced result should not also populate the plain lyrics body")
	}
	// Provider metadata should enrich the track.
	if song.Track.ISRC == "" || song.Track.TrackLength == 0 {
		t.Errorf("provider metadata not carried onto the track: %+v", song.Track)
	}
}

// TestFindLyrics_AttachesWordTimings pins that the decoded per-word timings
// actually reach models.Song. They were decoded and discarded before #603; the
// orchestrator now needs them to rank a word-synced result above a line-synced
// one, and #480's A2 writer will consume them.
func TestFindLyrics_AttachesWordTimings(t *testing.T) {
	c, _ := newTestClient(t, serveFixture(t, "type3_wordsync.xml"))
	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.WordTimings) == 0 {
		t.Fatal("word-synced result must carry word timings")
	}
	// Every timing must index a real cue, or downstream grouping is corrupt.
	for i, w := range song.WordTimings {
		if w.Line < 0 || w.Line >= len(song.Subtitles.Lines) {
			t.Fatalf("timing %d indexes line %d, outside 0..%d",
				i, w.Line, len(song.Subtitles.Lines)-1)
		}
	}
}

// TestFindLyrics_DropsTimingsWhenNormalizationShiftsCues covers the alignment
// guard. Word timings are positional indices into the cue slice, so if
// lrcnormalize.Expand splits a cue (it does that for a cue whose TEXT carries an
// embedded timestamp) every later index shifts and the timings would point at
// the wrong words. The client must drop them rather than ship misaligned data --
// the result degrades to line-sync quality, which is correct but lesser.
func TestFindLyrics_DropsTimingsWhenNormalizationShiftsCues(t *testing.T) {
	inner := `<wsy><line><linestring>[00:09.00]Lorem ipsum</linestring>` +
		`<word><starttime>3000</starttime><endtime>4000</endtime><wordstring>Lorem</wordstring></word>` +
		`</line></wsy>`
	body := `<response><songs><song><lyricsId>1</lyricsId><title>Lorem Ipsum</title>` +
		`<lyricsData>` + base64.StdEncoding.EncodeToString([]byte(inner)) +
		`</lyricsData></song></songs></response>`
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("expected the embedded stamp to expand to 2 cues, got %d", len(song.Subtitles.Lines))
	}
	if len(song.WordTimings) != 0 {
		t.Errorf("timings must be dropped when normalization changes the cue set, got %d", len(song.WordTimings))
	}
}

// TestFindLyrics_MultiCandidateIntegration exercises the whole path end to end
// on real XML -- parse envelope, score candidates, decode payload -- rather than
// unit-testing selectCandidate against hand-built structs. The fixture holds two
// candidates: a shorter "(Live)" cut with no ISRC, and the studio version whose
// ISRC and duration match the requested track.
func TestFindLyrics_MultiCandidateIntegration(t *testing.T) {
	c, _ := newTestClient(t, serveFixture(t, "multi_candidate.xml"))
	song, err := c.FindLyrics(context.Background(), models.Track{
		TrackName: "Lorem Ipsum", AlbumName: "Amet Consectetur",
		TrackLength: 210, ISRC: "ZZZZZ0000001",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	// The ISRC-and-duration match must win over the decoy.
	if song.Track.ISRC != "ZZZZZ0000001" {
		t.Errorf("wrong candidate selected: ISRC = %q", song.Track.ISRC)
	}
	// Assert the CONVERTED value, not merely that it is non-zero: the provider
	// reports milliseconds and models.Track carries seconds, so a missing
	// division would show up here as 210000 rather than 210.
	if song.Track.TrackLength != 210 {
		t.Errorf("duration should convert ms -> s: got %d, want 210", song.Track.TrackLength)
	}
	if song.Track.AlbumName != "Amet Consectetur" {
		t.Errorf("album not carried from the provider: %q", song.Track.AlbumName)
	}
	if song.Track.HasLyrics != 1 {
		t.Error("a song with lyrics should be flagged HasLyrics")
	}
	if len(song.Subtitles.Lines) == 0 {
		t.Error("selected candidate should decode to synced cues")
	}
}

// TestFindLyrics_CuesAreNormalized pins the lrcnormalize.Expand call in lookup,
// which upholds the one-cue-per-line invariant (#470) on this lane.
//
// The exercise has to be a <linestring> whose TEXT carries an embedded
// timestamp, because that is the only input on which Expand does work: cues
// coming out of decodeWordSync are already one-per-line and time-sorted, so a
// well-formed payload cannot distinguish Expand from a no-op. Provider text is
// untrusted, so a stray leading stamp is reachable -- and without expansion it
// would render as literal lyric text in a player and never highlight.
func TestFindLyrics_CuesAreNormalized(t *testing.T) {
	inner := `<wsy><line><linestring>[00:09.00]Lorem ipsum</linestring>` +
		`<word><starttime>3000</starttime><endtime>4000</endtime><wordstring>Lorem</wordstring></word>` +
		`</line></wsy>`
	body := `<response><songs><song><lyricsId>1</lyricsId><title>Lorem Ipsum</title>` +
		`<lyricsData>` + base64.StdEncoding.EncodeToString([]byte(inner)) +
		`</lyricsData></song></songs></response>`

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	// One line in, two cues out: the cue's own 3.0s timestamp plus the 9.0s
	// stamp lifted out of the text.
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("embedded timestamp should expand to a second cue, got %d cues: %+v",
			len(song.Subtitles.Lines), song.Subtitles.Lines)
	}
	for i, ln := range song.Subtitles.Lines {
		if strings.HasPrefix(ln.Text, "[") {
			t.Errorf("cue %d still carries a literal timestamp in its text: %q", i, ln.Text)
		}
	}
	// Cues must come out time-ordered.
	if song.Subtitles.Lines[0].Time.Total > song.Subtitles.Lines[1].Time.Total {
		t.Error("expanded cues are not time-ordered")
	}
}

func TestFindLyrics_UnsyncedFallback(t *testing.T) {
	// Word-sync returns nothing; the client should retry at the unsynced tier
	// rather than reporting the track as a miss.
	var calls int
	c, fs := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		name := "type1_unsynced.xml"
		if calls == 1 {
			name = "empty.xml"
		}
		body, _ := os.ReadFile(filepath.Join("testdata", name))
		_, _ = w.Write(body)
	})

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics should fall back to unsynced: %v", err)
	}
	if song.Lyrics.LyricsBody == "" {
		t.Error("fallback should produce plain lyrics")
	}
	reqs := fs.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests (word-sync then unsynced), got %d", len(reqs))
	}
	if reqs[0].form["lyricsType"] != "3" || reqs[1].form["lyricsType"] != "1" {
		t.Errorf("tier order wrong: %q then %q", reqs[0].form["lyricsType"], reqs[1].form["lyricsType"])
	}
}

// TestFindLyrics_LineSyncIsUnsupported pins the deliberate gap: the line-sync
// payload is an encrypted binary blob, so it must surface as a typed tier miss
// rather than being mistaken for lyrics.
func TestFindLyrics_LineSyncIsUnsupported(t *testing.T) {
	c, _ := newTestClient(t, serveFixture(t, "type2_linesync.xml"))
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err == nil {
		t.Fatal("an encrypted line-sync payload must not be reported as success")
	}
	// After the word-sync attempt yields an unsupported tier, the fallback also
	// serves the same binary payload, so the final error is still the tier error.
	if !errors.Is(err, ErrUnsupportedTier) {
		t.Errorf("want ErrUnsupportedTier, got %v", err)
	}
}

func TestFindLyrics_NotFound(t *testing.T) {
	c, _ := newTestClient(t, serveFixture(t, "empty.xml"))
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Nothing"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestStatusMapping pins the corrected classification. The old client mapped 403
// to ErrRateLimited, which made the #495 User-Agent rejection read as throttling
// in every log line and misdirected the investigation.
func TestStatusMapping(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusTooManyRequests, ErrRateLimited},
	}
	for _, tc := range tests {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		})
		_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x"})
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: want %v, got %v", tc.status, tc.want, err)
		}
	}
}

func TestStatusMapping_ForbiddenIsNotRateLimited(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x"})
	if errors.Is(err, ErrRateLimited) {
		t.Error("403 must not be classified as rate limiting (regression: issue #495)")
	}
}

// TestFindLyrics_PolicyErrorsDoNotRetry: a 403/401/429 is a request failure, not
// a tier miss, so the client must not spend a second request on a fallback.
func TestFindLyrics_PolicyErrorsDoNotRetry(t *testing.T) {
	c, fs := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "x"})
	if n := len(fs.snapshot()); n != 1 {
		t.Errorf("a policy failure must not trigger a tier fallback; got %d requests", n)
	}
}

func TestFindLyrics_MalformedXML(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<response><songs><song>truncated"))
	})
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x"})
	if err == nil {
		t.Error("malformed XML must not be reported as success")
	}
}

func TestName(t *testing.T) {
	if got := NewClient().Name(); got != "petitlyrics" {
		t.Errorf("Name() = %q", got)
	}
}

func TestMinInterval_RoundTrip(t *testing.T) {
	c := NewClient()
	if c.MinInterval() != 0 {
		t.Errorf("default should disable pacing, got %v", c.MinInterval())
	}
	c.WithMinInterval(30 * time.Second)
	if c.MinInterval() != 30*time.Second {
		t.Errorf("MinInterval() = %v", c.MinInterval())
	}
}

// TestWithMinInterval_ClampsBelowFloor pins the politeness guard: a
// misconfigured cooldown must not let this lane outrun the policy floor.
func TestWithMinInterval_ClampsBelowFloor(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"below floor clamps up", time.Second, MinAllowedInterval},
		{"at floor is kept", MinAllowedInterval, MinAllowedInterval},
		{"above floor is kept", DefaultMinInterval, DefaultMinInterval},
		{"zero disables pacing", 0, 0},
		{"negative disables pacing", -time.Second, -time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewClient().WithMinInterval(tc.in).MinInterval(); got != tc.want {
				t.Errorf("WithMinInterval(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPace_EnforcesInterval(t *testing.T) {
	c := NewClient()
	var slept []time.Duration
	base := time.Unix(0, 0)
	c.now = func() time.Time { return base }
	c.sleep = func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		base = base.Add(d)
		return true
	}
	c.WithMinInterval(30 * time.Second)

	if err := c.pace(context.Background()); err != nil {
		t.Fatalf("first pace: %v", err)
	}
	if len(slept) != 0 {
		t.Errorf("first request should not wait, slept %v", slept)
	}
	if err := c.pace(context.Background()); err != nil {
		t.Fatalf("second pace: %v", err)
	}
	if len(slept) != 1 || slept[0] != 30*time.Second {
		t.Errorf("second request should wait the full interval, slept %v", slept)
	}
}

func TestPace_ContextCancel(t *testing.T) {
	c := NewClient()
	c.WithMinInterval(time.Hour)
	c.now = func() time.Time { return time.Unix(0, 0) }
	c.sleep = func(_ context.Context, _ time.Duration) bool { return false }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.pace(ctx) // priming call records lastRequest
	if err := c.pace(ctx); err == nil {
		t.Error("a canceled context should abort pacing")
	}
}

// TestCheckRedirect_RefusesCrossHost pins the SSRF guard.
func TestCheckRedirect_RefusesCrossHost(t *testing.T) {
	c := NewClient()
	c.baseURL = "https://p0.petitlyrics.com"

	same, _ := http.NewRequest(http.MethodGet, "https://p0.petitlyrics.com/x", nil)
	if err := c.checkRedirect(same, nil); err != nil {
		t.Errorf("same-host redirect should be allowed: %v", err)
	}
	other, _ := http.NewRequest(http.MethodGet, "https://evil.example/x", nil)
	if err := c.checkRedirect(other, nil); err == nil {
		t.Error("cross-host redirect must be refused")
	}
	deep := make([]*http.Request, 10)
	if err := c.checkRedirect(same, deep); err == nil {
		t.Error("redirect chains must be capped")
	}
}
