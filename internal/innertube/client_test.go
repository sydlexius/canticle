package innertube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureServer serves a fixture file for every request and records what the
// client sent, mirroring internal/petitlyrics's test harness.
type fixtureServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	// fixtures maps a request path to the fixture file served for it, so one
	// server can stand in for the whole three-call chain in an integration
	// test.
	fixtures map[string]string
	// status, if non-zero, is returned instead of any fixture.
	status int
}

type recordedRequest struct {
	method      string
	path        string
	userAgent   string
	contentType string
	body        map[string]any
}

func (f *fixtureServer) record(r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		method:      r.Method,
		path:        r.URL.Path,
		userAgent:   r.Header.Get("User-Agent"),
		contentType: r.Header.Get("Content-Type"),
		body:        body,
	})
}

func (f *fixtureServer) snapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// newTestClient wires a Client at an httptest server that serves srv.fixtures
// by request path, or srv.status if set.
func newTestClient(t *testing.T, srv *fixtureServer) *Client {
	t.Helper()
	if srv.fixtures == nil {
		srv.fixtures = map[string]string{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		if srv.status != 0 {
			w.WriteHeader(srv.status)
			return
		}
		name, ok := srv.fixtures[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, name))
	}))
	t.Cleanup(server.Close)

	c := NewClient()
	c.baseURL = server.URL
	return c
}

// --- AC: all three calls issue the expected method, headers and body shape ---

func TestSearch_SendsExpectedRequest(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)

	if _, err := c.Search(context.Background(), "Placeholder Artist Name", "Placeholder Song Title"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	reqs := srv.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost {
		t.Errorf("want POST, got %s", req.method)
	}
	if req.path != searchPath {
		t.Errorf("want path %q, got %q", searchPath, req.path)
	}
	if req.contentType != "application/json" {
		t.Errorf("want application/json content type, got %q", req.contentType)
	}
	if req.userAgent == "" || strings.HasPrefix(req.userAgent, "Go-http-client") {
		t.Errorf("client must send an identifying User-Agent, got %q", req.userAgent)
	}
	if !strings.Contains(req.userAgent, "canticle") {
		t.Errorf("User-Agent should identify canticle, got %q", req.userAgent)
	}

	query, _ := req.body["query"].(string)
	if !strings.Contains(query, "Placeholder Song Title") || !strings.Contains(query, "Placeholder Artist Name") {
		t.Errorf("query should carry artist and title, got %q", query)
	}
	if params, _ := req.body["params"].(string); params == "" {
		t.Error("search request should carry the songs-only params filter")
	}
	clientCtx, _ := req.body["context"].(map[string]any)
	client, _ := clientCtx["client"].(map[string]any)
	if client["clientName"] != webClientName {
		t.Errorf("search should use client %q, got %v", webClientName, client["clientName"])
	}
}

func TestNext_SendsExpectedRequest(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{nextPath: "next.json"}}
	c := newTestClient(t, srv)

	if _, err := c.Next(context.Background(), "NrgmdOz227I"); err != nil {
		t.Fatalf("Next: %v", err)
	}

	reqs := srv.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost {
		t.Errorf("want POST, got %s", req.method)
	}
	if req.path != nextPath {
		t.Errorf("want path %q, got %q", nextPath, req.path)
	}
	if videoID, _ := req.body["videoId"].(string); videoID != "NrgmdOz227I" {
		t.Errorf("videoId not forwarded: got %q", videoID)
	}
}

func TestBrowse_SendsExpectedRequest(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{browsePath: "browse.json"}}
	c := newTestClient(t, srv)

	if _, err := c.Browse(context.Background(), "MPLYt_Cn67yAcHym7-13"); err != nil {
		t.Fatalf("Browse: %v", err)
	}

	reqs := srv.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost {
		t.Errorf("want POST, got %s", req.method)
	}
	if req.path != browsePath {
		t.Errorf("want path %q, got %q", browsePath, req.path)
	}
	if browseID, _ := req.body["browseId"].(string); browseID != "MPLYt_Cn67yAcHym7-13" {
		t.Errorf("browseId not forwarded: got %q", browseID)
	}
}

// --- AC: the ANDROID_MUSIC client context is asserted by a test ---

// TestBrowse_UsesTimedLyricsClientContext pins the single most fragile fact
// in this package (see doc.go): browse must use ANDROID_MUSIC 7.03.52, not
// the WEB_REMIX context used by search/next, because WEB_REMIX returns plain
// text with NO timings for the identical browseId. A silent downgrade of
// either constant would still "work" (200 OK, valid JSON) but quietly stop
// producing timed cues -- this test fails loudly on that change rather than
// leaving it to be discovered downstream.
func TestBrowse_UsesTimedLyricsClientContext(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{browsePath: "browse.json"}}
	c := newTestClient(t, srv)

	if _, err := c.Browse(context.Background(), "MPLYt_Cn67yAcHym7-13"); err != nil {
		t.Fatalf("Browse: %v", err)
	}

	req := srv.snapshot()[0]
	clientCtx, _ := req.body["context"].(map[string]any)
	client, _ := clientCtx["client"].(map[string]any)

	gotName, _ := client["clientName"].(string)
	gotVersion, _ := client["clientVersion"].(string)

	if gotName != "ANDROID_MUSIC" {
		t.Errorf("browse must send clientName ANDROID_MUSIC (WEB_REMIX returns untimed text), got %q", gotName)
	}
	if gotVersion != "7.03.52" {
		t.Errorf("browse must send clientVersion 7.03.52 (5.16.51 is rejected with HTTP 400), got %q", gotVersion)
	}
	// Pin the exported constants directly too: a change to the constant that
	// forgot to update the literals above (or vice versa) should still fail.
	if browseClientName != "ANDROID_MUSIC" || browseClientVersion != "7.03.52" {
		t.Fatalf("browse client constants changed: %s %s", browseClientName, browseClientVersion)
	}
}

// TestSearchAndNext_UseWebRemixContext is the sibling assertion: search and
// next are plain browsing calls and must NOT accidentally pick up the
// timed-lyrics client context (which is browse-specific and, per doc.go, not
// what the fixtures for these two calls were captured against).
func TestSearchAndNext_UseWebRemixContext(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{
		searchPath: "search.json",
		nextPath:   "next.json",
	}}
	c := newTestClient(t, srv)

	if _, err := c.Search(context.Background(), "artist", "title"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := c.Next(context.Background(), "NrgmdOz227I"); err != nil {
		t.Fatalf("Next: %v", err)
	}

	for _, req := range srv.snapshot() {
		clientCtx, _ := req.body["context"].(map[string]any)
		client, _ := clientCtx["client"].(map[string]any)
		if name, _ := client["clientName"].(string); name != webClientName {
			t.Errorf("%s should use client %q, got %q", req.path, webClientName, name)
		}
	}
}

// --- AC: videoId and the MPLY browseId are extracted from fixtures ---

func TestSearch_ExtractsVideoID(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search.json"}}
	c := newTestClient(t, srv)

	candidates, err := c.Search(context.Background(), "Placeholder Artist Name", "Placeholder Song Title")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}
	got := candidates[0]
	if got.VideoID != "NrgmdOz227I" {
		t.Errorf("videoId = %q, want NrgmdOz227I", got.VideoID)
	}
	if got.Title != "Placeholder Song Title" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Artist != "Placeholder Artist Name" {
		t.Errorf("artist = %q", got.Artist)
	}
	if got.DurationSeconds != 126 {
		t.Errorf("duration = %d, want 126 (2:06)", got.DurationSeconds)
	}
}

func TestNext_ExtractsLyricsBrowseID(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{nextPath: "next.json"}}
	c := newTestClient(t, srv)

	browseID, err := c.Next(context.Background(), "NrgmdOz227I")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if browseID != "MPLYt_Cn67yAcHym7-13" {
		t.Errorf("browseId = %q, want MPLYt_Cn67yAcHym7-13", browseID)
	}
	if !strings.HasPrefix(browseID, "MPLY") {
		t.Errorf("lyrics browseId must carry the MPLY prefix, got %q", browseID)
	}
}

// TestNext_NoLyricsTabIsErrNoLyricsTab covers a next response whose tabs
// never include a "Lyrics" entry.
func TestNext_NoLyricsTabIsErrNoLyricsTab(t *testing.T) {
	body := `{"contents":{"singleColumnMusicWatchNextResultsRenderer":{"tabbedRenderer":{` +
		`"watchNextTabbedResultsRenderer":{"tabs":[{"tabRenderer":{"title":"Up next"}}]}}}}}`
	srv := &fixtureServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	c := NewClient()
	c.baseURL = server.URL

	_, err := c.Next(context.Background(), "someVideoId")
	if !errors.Is(err, ErrNoLyricsTab) {
		t.Fatalf("want ErrNoLyricsTab, got %v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("ErrNoLyricsTab must wrap ErrNotFound")
	}
}

// TestSearch_NoCandidatesIsErrNotFound covers a search response with no
// musicCardShelfRenderer at all -- a genuine empty shelf, distinct from the
// "confident wrong hit" trap documented in doc.go and exercised below.
func TestSearch_NoCandidatesIsErrNotFound(t *testing.T) {
	body := `{"contents":{"tabbedSearchResultsRenderer":{"tabs":[]}}}`
	srv := &fixtureServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	c := NewClient()
	c.baseURL = server.URL

	_, err := c.Search(context.Background(), "artist", "title")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestSearch_NonsenseQueryStillReturnsACandidate pins doc.go's second trap:
// a nonsense query still returns a confident, fully-timed, unrelated
// candidate. This client parses nothing beyond videoId/title/artist/duration
// and does NOT attempt to detect a "confident miss" -- that correspondence
// guard is out of scope for this transport slice (owned downstream) -- so
// this fixture must decode successfully, exactly like a real hit.
func TestSearch_NonsenseQueryStillReturnsACandidate(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search_nonsense.json"}}
	c := newTestClient(t, srv)

	candidates, err := c.Search(context.Background(), "flibbertigibbet", "wonkabazoo nonsense query zzzzz9999")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate even on a nonsense query (see doc.go), got %d", len(candidates))
	}
	if candidates[0].VideoID != "PLACEHOLDvi" {
		t.Errorf("videoId = %q, want PLACEHOLDvi", candidates[0].VideoID)
	}
}

// TestSearch_ArtistNotOverwrittenByAlbum guards 854-F2: a realistic subtitle
// carries both an artist run and an album run, each bearing a
// browseEndpoint. The shipped search.json fixture has only one such run and
// cannot exercise the last-browse-run-wins overwrite; search_artist_album.json
// (854-F2's dedicated fixture) supplies both.
func TestSearch_ArtistNotOverwrittenByAlbum(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{searchPath: "search_artist_album.json"}}
	c := newTestClient(t, srv)

	candidates, err := c.Search(context.Background(), "Placeholder Artist Name", "Placeholder Song Title")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}
	got := candidates[0]
	if got.Artist != "Placeholder Artist Name" {
		t.Errorf("Artist = %q, want the artist run, not the album run", got.Artist)
	}
	if got.DurationSeconds != 126 {
		t.Errorf("duration = %d, want 126 (2:06)", got.DurationSeconds)
	}
}

// --- AC: status codes map to sentinels; body cap and redirect pinning ---

func TestStatusError_MapsSentinels(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusBadRequest, ErrClientVersion},
	}
	for _, tc := range cases {
		err := statusError(tc.status)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v, want wrapping %v", tc.status, err, tc.want)
		}
	}
	if statusError(http.StatusOK) != nil {
		t.Error("200 should map to nil")
	}
}

// TestBrowse_StatusMapping exercises the sentinel mapping through the real
// call path (not just statusError in isolation), for each status this
// package cares about.
func TestBrowse_StatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"rate_limited", http.StatusTooManyRequests, ErrRateLimited},
		{"stale_client_version", http.StatusBadRequest, ErrClientVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fixtureServer{status: tc.status}
			c := newTestClient(t, srv)
			_, err := c.Browse(context.Background(), "MPLYsomeBrowseId")
			if !errors.Is(err, tc.want) {
				t.Errorf("status %d: got %v, want wrapping %v", tc.status, err, tc.want)
			}
		})
	}
}

// TestErrClientVersion_WrapsErrForbidden pins errors.go's documented
// wrapping relationship, exercised through this client's own mapping.
func TestErrClientVersion_WrapsErrForbidden(t *testing.T) {
	if !errors.Is(ErrClientVersion, ErrForbidden) {
		t.Fatal("ErrClientVersion must wrap ErrForbidden")
	}
}

// TestSearch_EmptyOrUnusableBodyIsErrNotFound guards 854-F4: a 200 whose body
// is empty, whitespace, or a non-JSON interstitial (an HTML error page, a
// captive portal) must classify as ErrNotFound -- a benign miss -- rather
// than surfacing a naked, unclassifiable decode error.
func TestSearch_EmptyOrUnusableBodyIsErrNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", "   \n\t"},
		{"html_error_page", "<html><body>captive portal</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			c := NewClient()
			c.baseURL = srv.URL

			_, err := c.Search(context.Background(), "artist", "title")
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("want ErrNotFound, got %v", err)
			}
		})
	}
}

// TestSearch_GenuineParseErrorStaysUnclassified guards the boundary
// TestSearch_EmptyOrUnusableBodyIsErrNotFound draws: a body that DOES start
// like JSON but is truncated mid-document is a genuinely broken response,
// not a clean miss, and must not be silently bucketed as ErrNotFound.
func TestSearch_GenuineParseErrorStaysUnclassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"contents":{"tabbedSearchResultsRenderer":`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL

	_, err := c.Search(context.Background(), "artist", "title")
	if err == nil {
		t.Fatal("want an error for a truncated mid-document body")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a genuine mid-document parse error must not classify as ErrNotFound")
	}
}

// TestBrowse_EmptyOrUnusableBodyIsErrNotFound guards 854-F5, widened by
// 854-R2F1: Browse originally only checked len(raw) == 0, so a whitespace-only
// or non-JSON body (an HTML interstitial, a captive portal) reached the
// decode slice as a nil-error payload -- narrower than the same guard on
// Search and Next. Every case here must classify as ErrNotFound, matching
// TestSearch_EmptyOrUnusableBodyIsErrNotFound's coverage.
func TestBrowse_EmptyOrUnusableBodyIsErrNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"single_space", " "},
		{"whitespace", "   \n\t"},
		{"html_error_page", "<html><body>captive portal</body></html>"},
		{"plain_text", "service unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			c := NewClient()
			c.baseURL = srv.URL

			raw, err := c.Browse(context.Background(), "MPLYsomeBrowseId")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
			if raw != nil {
				t.Errorf("want nil bytes on error, got %d bytes", len(raw))
			}
		})
	}
}

// TestNext_NonMPLYBrowseIDIsErrNoLyricsTab guards 854-F7: a "Lyrics"-titled
// tab whose browseId does not carry the documented MPLY prefix must not be
// returned as a trusted lyrics browseId.
func TestNext_NonMPLYBrowseIDIsErrNoLyricsTab(t *testing.T) {
	body := `{"contents":{"singleColumnMusicWatchNextResultsRenderer":{"tabbedRenderer":{` +
		`"watchNextTabbedResultsRenderer":{"tabs":[{"tabRenderer":{"title":"Lyrics",` +
		`"endpoint":{"browseEndpoint":{"browseId":"VLPLnotalyricsbrowseid"}}}}]}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL

	_, err := c.Next(context.Background(), "someVideoId")
	if !errors.Is(err, ErrNoLyricsTab) {
		t.Fatalf("want ErrNoLyricsTab for a non-MPLY browseId, got %v", err)
	}
}

func TestReadBody_CapsResponseSize(t *testing.T) {
	oversized := strings.Repeat("a", maxResponseSize+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL

	_, err := c.Search(context.Background(), "artist", "title")
	if err == nil {
		t.Fatal("want an error for an oversized response")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should report the size cap, got: %v", err)
	}
}

// TestCheckRedirect_RefusesCrossHost pins the SSRF guard, mirroring
// internal/petitlyrics.TestCheckRedirect_RefusesCrossHost.
func TestCheckRedirect_RefusesCrossHost(t *testing.T) {
	c := NewClient()
	c.baseURL = "https://music.youtube.com"

	same, _ := http.NewRequest(http.MethodGet, "https://music.youtube.com/x", nil)
	if err := c.checkRedirect(same, nil); err != nil {
		t.Errorf("same-host redirect should be allowed: %v", err)
	}
	other, _ := http.NewRequest(http.MethodGet, "https://evil.example/x", nil)
	if err := c.checkRedirect(other, nil); err == nil {
		t.Error("cross-host redirect must be refused")
	}
	deep := make([]*http.Request, 10)
	if err := c.checkRedirect(same, deep); err == nil {
		t.Error("redirect chains must be capped at 10 hops")
	}
}

// TestClient_FollowsSameHostRedirect proves the guard does not merely reject
// on paper: a real same-host 3xx must still be followed end to end, while a
// cross-host one must not reach the target server at all.
func TestClient_FollowsSameHostRedirect(t *testing.T) {
	var hitFinal bool
	mux := http.NewServeMux()
	mux.HandleFunc(searchPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, searchPath+"/final", http.StatusFound)
	})
	mux.HandleFunc(searchPath+"/final", func(w http.ResponseWriter, _ *http.Request) {
		hitFinal = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "search.json"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	if _, err := c.Search(context.Background(), "artist", "title"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !hitFinal {
		t.Error("same-host redirect should have been followed")
	}
}

func TestClient_RefusesCrossHostRedirectOnTheWire(t *testing.T) {
	var evilHit bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		evilHit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evil.Close)

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(primary.Close)

	c := NewClient()
	c.baseURL = primary.URL

	_, err := c.Search(context.Background(), "artist", "title")
	if err == nil {
		t.Fatal("want an error when the API attempts a cross-host redirect")
	}
	if evilHit {
		t.Error("the client must never have reached the cross-host target")
	}
}

// TestClient_RefusesSchemeDowngradeOnTheWire is 854-F1's required end-to-end
// proof: a real https origin redirecting to a real plain-http target must be
// refused before the http target is ever reached, not merely rejected by the
// predicate in isolation.
func TestClient_RefusesSchemeDowngradeOnTheWire(t *testing.T) {
	var httpHit bool
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpHit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(httpTarget.Close)

	httpsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL+"/x", http.StatusFound)
	}))
	t.Cleanup(httpsOrigin.Close)

	c := NewClient()
	c.baseURL = httpsOrigin.URL
	c.httpClient.Transport = httpsOrigin.Client().Transport

	_, err := c.Search(context.Background(), "artist", "title")
	if err == nil {
		t.Fatal("want an error when the API attempts an https->http scheme downgrade")
	}
	if httpHit {
		t.Error("the client must never have followed the downgraded http redirect")
	}
}

// TestCheckRedirect_HostAndSchemeMatrix is the full regression matrix from
// the hostile review of 854-F1: every host-shaped attack the guard already
// handled correctly, the two scheme-downgrade cases F1 fixes, and the
// default-port normalization F6 adds. Table-driven so a future regression on
// any single case is immediately attributable.
func TestCheckRedirect_HostAndSchemeMatrix(t *testing.T) {
	c := NewClient()
	c.baseURL = "https://music.youtube.com"

	cases := []struct {
		name    string
		target  string
		allowed bool
	}{
		{"baseline same-host https", "https://music.youtube.com/x", true},
		{"scheme downgrade to http", "http://music.youtube.com/x", false},
		{"scheme downgrade to ftp", "ftp://music.youtube.com/x", false},
		{"suffix host evil-music.youtube.com", "https://evil-music.youtube.com/x", false},
		{"subdomain evil.music.youtube.com", "https://evil.music.youtube.com/x", false},
		{"prefix-match music.youtube.com.evil.tld", "https://music.youtube.com.evil.tld/x", false},
		// explicit default port normalizes to the implicit form (854-F6).
		{"explicit default port 443", "https://music.youtube.com:443/x", true},
		{"explicit non-default port 8443", "https://music.youtube.com:8443/x", false},
		{"userinfo host-looking user", "https://music.youtube.com@evil.example/x", false},
		{"userinfo before an on-host target", "https://evil.example@music.youtube.com/x", true},
		{"uppercase host", "https://MUSIC.YOUTUBE.COM/x", false},
		{"mixed case host", "https://Music.YouTube.com/x", false},
		{"trailing-dot FQDN", "https://music.youtube.com./x", false},
		{"punycode homograph", "https://xn--msic-0ra.youtube.com/x", false},
		{"unicode homograph (Cyrillic s)", "https://muѕic.youtube.com/x", false},
		{"IPv4 literal (IMDS)", "https://169.254.169.254/latest/meta-data/", false},
		{"IPv6 literal", "https://[::1]/x", false},
		{"localhost", "https://localhost/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatalf("parse target %q: %v", tc.target, err)
			}
			gotErr := c.checkRedirect(req, nil)
			allowed := gotErr == nil
			if allowed != tc.allowed {
				t.Errorf("%s: allowed=%v (err=%v), want allowed=%v", tc.target, allowed, gotErr, tc.allowed)
			}
		})
	}
}

// TestCheckRedirect_HTTPBaseAllowsHTTPRedirect documents the deliberate
// fail-open half of checkRedirect's scheme comparison (854-F1 follow-up): a
// Client whose base is already plain http gains no exposure from following
// an http redirect, because the channel was cleartext to begin with. This is
// the shape every test in this package actually relies on (an
// httptest.Server base), and the assertion here is what would catch a
// regression that accidentally hardcoded "https" into checkRedirect.
func TestCheckRedirect_HTTPBaseAllowsHTTPRedirect(t *testing.T) {
	c := NewClient()
	c.baseURL = "http://127.0.0.1:9"

	same, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/x", nil)
	if err := c.checkRedirect(same, nil); err != nil {
		t.Errorf("an http base should allow a same-scheme http redirect: %v", err)
	}
}

// TestCheckRedirect_HTTPBaseRefusesHTTPSUpgrade documents the other half:
// a Client whose base is plain http refuses a redirect that switches to
// https. This is fail-closed and harmless (the caller merely loses a
// redirect it could safely have followed), called out explicitly so a
// future reader does not "fix" the asymmetry by allowing an upgrade, which
// would legitimize scheme MISMATCH in general rather than only the
// no-exposure http-to-http case above.
func TestCheckRedirect_HTTPBaseRefusesHTTPSUpgrade(t *testing.T) {
	c := NewClient()
	c.baseURL = "http://127.0.0.1:9"

	upgraded, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1:9/x", nil)
	if err := c.checkRedirect(upgraded, nil); err == nil {
		t.Error("an http base should refuse a same-host https upgrade (scheme mismatch), not just a downgrade")
	}
}

// TestCheckRedirect_SchemeComparisonIsCaseInsensitive guards the casing
// question directly: url.Parse lowercases the scheme it returns regardless
// of the input's casing, so an uppercase or mixed-case scheme in a redirect
// Location cannot be used to slip past the scheme comparison by case.
func TestCheckRedirect_SchemeComparisonIsCaseInsensitive(t *testing.T) {
	c := NewClient()
	c.baseURL = "https://music.youtube.com"

	for _, raw := range []string{
		"https://music.youtube.com/x",
		"HTTPS://music.youtube.com/x",
		"Https://music.youtube.com/x",
	} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if req.URL.Scheme != "https" {
			t.Fatalf("url.Parse did not lowercase scheme for %q: got %q", raw, req.URL.Scheme)
		}
		if err := c.checkRedirect(req, nil); err != nil {
			t.Errorf("%q: same-origin redirect should be allowed regardless of scheme casing: %v", raw, err)
		}
	}
}

// --- AC: context cancellation is honored ---

func TestClient_HonorsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer close(block)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Search(ctx, "artist", "title")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error for a canceled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Search did not honor context cancellation within 5s")
	}
}

func TestClient_HonorsContextDeadline(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer close(block)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Next(ctx, "someVideoId")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want context.DeadlineExceeded, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not honor the context deadline within 5s")
	}
}

// --- AC: no live network access in any test ---
//
// Every test above targets an httptest.Server via an injected baseURL; none
// resolves a real host. NewClient's default baseURL (music.youtube.com) is
// never dialed because every test overrides c.baseURL immediately after
// construction.

// TestNewClient_DefaultsAreSane is a smoke test for NewClient itself,
// asserting the defaults without making any request.
func TestNewClient_DefaultsAreSane(t *testing.T) {
	c := NewClient()
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.Name() != "innertube" {
		t.Errorf("Name() = %q, want innertube", c.Name())
	}
	if c.httpClient.CheckRedirect == nil {
		t.Error("NewClient must wire the SSRF redirect guard")
	}
}

// TestParseDurationSeconds pins the duration-text-run parser used by search
// candidate extraction, including its fail-open-on-unparsable contract
// (SearchCandidate.DurationSeconds documents zero as "not supplied").
func TestParseDurationSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"2:06", 126},
		{"0:09", 9},
		{"12:34", 754},
		{"Song", 0},
		{" • ", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := parseDurationSeconds(tc.in); got != tc.want {
			t.Errorf("parseDurationSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
