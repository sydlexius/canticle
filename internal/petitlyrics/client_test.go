package petitlyrics

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
)

// Compile-time assertions that *Client satisfies the shared provider contracts.
var (
	_ providers.Fetcher        = (*Client)(nil)
	_ providers.LyricsProvider = (*Client)(nil)
	_ Fetcher                  = (*Client)(nil)
)

func TestProviderNameConstantMatches(t *testing.T) {
	if NewClient().Name() != providers.PetitLyrics {
		t.Fatalf("Name() = %q; want %q", NewClient().Name(), providers.PetitLyrics)
	}
}

// fixtureServer wires an httptest.Server that emulates the three reverse-
// engineered petitlyrics.com endpoints. Each field lets a test override the
// behavior of one stage. A nil handler falls back to a sane default.
type fixtureServer struct {
	searchStatus  int
	searchBody    string
	jsStatus      int
	jsBody        string
	ajaxStatus    int
	ajaxBody      string
	gotCSRFHeader string
	gotXHRHeader  string
	gotCookie     string
}

func newFixtureServer(t *testing.T, f *fixtureServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search_lyrics", func(w http.ResponseWriter, r *http.Request) {
		if f.searchStatus != 0 && f.searchStatus != http.StatusOK {
			w.WriteHeader(f.searchStatus)
			return
		}
		_, _ = w.Write([]byte(f.searchBody))
	})
	mux.HandleFunc("/lib/pl-lib.js", func(w http.ResponseWriter, r *http.Request) {
		if f.jsStatus != 0 && f.jsStatus != http.StatusOK {
			w.WriteHeader(f.jsStatus)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "PLSESSION", Value: "test-session", Path: "/"})
		_, _ = w.Write([]byte(f.jsBody))
	})
	mux.HandleFunc("/com/get_lyrics.ajax", func(w http.ResponseWriter, r *http.Request) {
		f.gotCSRFHeader = r.Header.Get("X-CSRF-Token")
		f.gotXHRHeader = r.Header.Get("X-Requested-With")
		if ck, err := r.Cookie("PLSESSION"); err == nil {
			f.gotCookie = ck.Value
		}
		if f.ajaxStatus != 0 && f.ajaxStatus != http.StatusOK {
			w.WriteHeader(f.ajaxStatus)
			return
		}
		_, _ = w.Write([]byte(f.ajaxBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

const validJS = `var x = 1; var csrfToken = "tok-abc-123"; var y = 2;`

func searchHTMLWithID(id string) string {
	return `<html><body><ul class="results">` +
		`<li><a href="/lyrics/` + id + `">Some Song</a></li>` +
		`</ul></body></html>`
}

func ajaxJSON(lyricsB64 string, lyricsType int) string {
	return `[{"lyrics_type":` + itoa(lyricsType) + `,"lyrics":"` + lyricsB64 + `"}]`
}

func itoa(i int) string {
	switch i {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	default:
		return "0"
	}
}

func newTestClient(srv *httptest.Server) *Client {
	c := NewClient()
	c.baseURL = srv.URL
	// Adopt only the test server's transport so requests reach the httptest
	// server, while preserving the cookie jar and timeout from NewClient so the
	// PLSESSION jar behavior (set in the CSRF stage, replayed to the AJAX stage)
	// is actually exercised.
	c.httpClient.Transport = srv.Client().Transport
	return c
}

func TestName(t *testing.T) {
	if got := NewClient().Name(); got != "petitlyrics" {
		t.Fatalf("Name() = %q; want petitlyrics", got)
	}
}

func TestFindLyrics_Synced(t *testing.T) {
	lrc := "[00:01.50]hello\n[00:12.30]world\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody:   ajaxJSON(b64(lrc), 2),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("got %d synced lines; want 2", len(song.Subtitles.Lines))
	}
	l0 := song.Subtitles.Lines[0]
	if l0.Text != "hello" || l0.Time.Minutes != 0 || l0.Time.Seconds != 1 || l0.Time.Hundredths != 50 {
		t.Fatalf("line0 = %+v; want hello @ 00:01.50", l0)
	}
	l1 := song.Subtitles.Lines[1]
	if l1.Text != "world" || l1.Time.Seconds != 12 || l1.Time.Hundredths != 30 {
		t.Fatalf("line1 = %+v; want world @ 00:12.30", l1)
	}
	if song.Lyrics.LyricsBody != "" {
		t.Fatalf("synced result should not set plain lyrics body; got %q", song.Lyrics.LyricsBody)
	}
	if f.gotCSRFHeader != "tok-abc-123" {
		t.Fatalf("X-CSRF-Token = %q; want tok-abc-123", f.gotCSRFHeader)
	}
	if f.gotXHRHeader != "XMLHttpRequest" {
		t.Fatalf("X-Requested-With = %q; want XMLHttpRequest", f.gotXHRHeader)
	}
	// The PLSESSION cookie set by the CSRF (pl-lib.js) stage must be replayed to
	// the AJAX stage by the client's cookie jar.
	if f.gotCookie != "test-session" {
		t.Fatalf("PLSESSION cookie at AJAX stage = %q; want test-session (cookie jar should persist it)", f.gotCookie)
	}
}

func TestFindLyrics_Plain(t *testing.T) {
	plain := "just some words\nno timestamps here\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("99"),
		jsBody:     validJS,
		ajaxBody:   ajaxJSON(b64(plain), 1),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 0 {
		t.Fatalf("plain result should not set synced lines; got %d", len(song.Subtitles.Lines))
	}
	if !strings.Contains(song.Lyrics.LyricsBody, "just some words") {
		t.Fatalf("plain body = %q; want it to contain the lyrics", song.Lyrics.LyricsBody)
	}
}

func TestFindLyrics_NotFound_NoMatch(t *testing.T) {
	f := &fixtureServer{
		searchBody: `<html><body>no results</body></html>`,
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

func TestFindLyrics_RateLimited429(t *testing.T) {
	f := &fixtureServer{searchStatus: http.StatusTooManyRequests}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
}

func TestFindLyrics_Forbidden403(t *testing.T) {
	f := &fixtureServer{searchStatus: http.StatusForbidden}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited (403 mapped to rate limited)", err)
	}
}

func TestFindLyrics_Unauthorized401(t *testing.T) {
	f := &fixtureServer{searchStatus: http.StatusUnauthorized}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v; want ErrUnauthorized", err)
	}
}

func TestFindLyrics_CSRFFetchFailure(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsStatus:   http.StatusInternalServerError,
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err == nil {
		t.Fatal("expected error on CSRF fetch failure; got nil")
	}
}

func TestFindLyrics_CSRFTokenMissing(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     `var nothing = "here";`,
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err == nil {
		t.Fatal("expected error when CSRF token is absent; got nil")
	}
}

func TestFindLyrics_EmptyAjaxBody(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody:   "",
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound on empty ajax body", err)
	}
}

func TestFindLyrics_AjaxRateLimited(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxStatus: http.StatusTooManyRequests,
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
}

func TestFindLyrics_BadBase64(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("4242"),
		jsBody:     validJS,
		ajaxBody:   `[{"lyrics_type":2,"lyrics":"!!!not base64!!!"}]`,
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"})
	if err == nil {
		t.Fatal("expected error on undecodable base64; got nil")
	}
}

func TestDecodeKnownFixture(t *testing.T) {
	// A known base64 fixture decodes to the expected LRC text.
	const want = "[00:00.00]line\n"
	got, err := base64.StdEncoding.DecodeString(b64(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != want {
		t.Fatalf("decoded %q; want %q", got, want)
	}
}

func TestWithMinIntervalReturnsClient(t *testing.T) {
	c := NewClient()
	if got := c.WithMinInterval(5 * time.Second); got != c {
		t.Fatal("WithMinInterval did not return the receiver")
	}
	if c.MinInterval() != 5*time.Second {
		t.Fatalf("MinInterval = %v; want 5s", c.MinInterval())
	}
}

func TestFindLyrics_RefusesCrossHostRedirect(t *testing.T) {
	// A second host that must never be reached: the client should refuse to
	// follow a cross-host redirect rather than fetch from an arbitrary host.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("client followed a cross-host redirect to %s", r.URL.Path)
	}))
	t.Cleanup(other.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/search_lyrics", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/search_lyrics", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestClient(srv)

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "x", ArtistName: "y"}); err == nil {
		t.Fatal("expected error refusing cross-host redirect; got nil")
	}
}

func TestFindLyrics_ContextCanceled(t *testing.T) {
	f := &fixtureServer{
		searchBody: searchHTMLWithID("1"),
		jsBody:     validJS,
		ajaxBody:   ajaxJSON(b64("[00:00.00]x\n"), 2),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FindLyrics(ctx, models.Track{TrackName: "x", ArtistName: "y"})
	if err == nil {
		t.Fatal("expected error when the context is canceled; got nil")
	}
}

func TestPacerEnforcesMinInterval(t *testing.T) {
	lrc := "[00:01.00]x\n"
	f := &fixtureServer{
		searchBody: searchHTMLWithID("1"),
		jsBody:     validJS,
		ajaxBody:   ajaxJSON(b64(lrc), 2),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)
	c.WithMinInterval(10 * time.Second)

	base := time.Unix(1000, 0)
	fakeNow := base
	c.now = func() time.Time { return fakeNow }
	var sleeps []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		fakeNow = fakeNow.Add(d)
		return true
	}

	// Pacing is enforced before each outbound request, not once per lookup. A
	// single FindLyrics makes three requests: the first does not wait (no prior
	// request), the second and third each wait the full interval.
	track := models.Track{TrackName: "x", ArtistName: "y"}
	if _, err := c.FindLyrics(context.Background(), track); err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(sleeps) != 2 {
		t.Fatalf("got %d paced waits in one lookup; want 2 (before the 2nd and 3rd requests)", len(sleeps))
	}
	for i, d := range sleeps {
		if d != 10*time.Second {
			t.Fatalf("paced wait %d = %v; want 10s", i, d)
		}
	}
}
