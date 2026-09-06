package innertube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
	// status, if non-zero, is returned instead of any fixture, for EVERY path.
	status int
	// statusFor overrides status for one path, so a test can serve a healthy
	// 200 for the early calls in the chain and a failure for a later one. That
	// is the only way to test the status mapping of any call but the first:
	// without it a 429 on browse is unreachable, because the same status would
	// have failed the search before browse was ever issued.
	statusFor map[string]int
	// onRequest, if set, is called with the request path BEFORE the response is
	// written, letting a test act at a chosen point in the chain -- canceling a
	// context when a particular call arrives, say. It runs on the SERVER
	// goroutine, so it must not touch *testing.T.
	onRequest func(path string)
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

// newTestClient wires a Client at an httptest server that serves srv.fixtures
// by request path, or srv.status if set.
func newTestClient(t *testing.T, srv *fixtureServer) *Client {
	t.Helper()
	if srv.fixtures == nil {
		srv.fixtures = map[string]string{}
	}
	// Preload every fixture on the TEST goroutine before the listener starts,
	// so the handler never calls readFixture (and therefore never t.Fatalf)
	// off-goroutine -- see readFixture's comment (854-R4F4).
	bodies := make(map[string][]byte, len(srv.fixtures))
	for path, name := range srv.fixtures {
		bodies[path] = readFixture(t, name)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.record(r)
		// Read under the lock: onRequest is set on the test goroutine before the
		// listener starts, but the handler runs on the server's.
		srv.mu.Lock()
		hook := srv.onRequest
		srv.mu.Unlock()
		if hook != nil {
			hook(r.URL.Path)
		}
		if code, ok := srv.statusFor[r.URL.Path]; ok && code != 0 {
			w.WriteHeader(code)
			return
		}
		if srv.status != 0 {
			w.WriteHeader(srv.status)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	c := NewClient()
	c.baseURL = server.URL
	return c
}

// --- AC: all three calls issue the expected method, endpoint and body shape ---
//
// HEADERS are deliberately NOT re-asserted per call (884-R1F1). Content-Type
// and User-Agent are set once in postJSON, which all three share, and
// client_test.go pins them there. Asserting them three more times here would
// test the same line thrice and drift out of sync with it; only Search keeps a
// User-Agent check, as a smoke test that the shared path is wired at all.

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
	// LITERAL, not the searchPath constant: comparing the constant to itself
	// is a tautology that survives any change to it, so a typo'd endpoint
	// would ship green and 404 in production (854-R5F2).
	if req.path != "/youtubei/v1/search" {
		t.Errorf("want path %q, got %q", "/youtubei/v1/search", req.path)
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
	// LITERAL, not just non-empty (884-R1F2). This opaque base64 blob is the
	// songs-only filter; any other non-empty value changes what the API
	// returns while a non-empty check stays green.
	if params, _ := req.body["params"].(string); params != "Eg-KAQwIARAAGAAgACgAMABqChAEEAUQAxAKEAk=" {
		t.Errorf("search request must carry the songs-only params filter verbatim, got %q", params)
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
	// A literal, for the reason given in TestSearch_SendsExpectedRequest.
	if req.path != "/youtubei/v1/next" {
		t.Errorf("want path %q, got %q", "/youtubei/v1/next", req.path)
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
	// A literal, for the reason given in TestSearch_SendsExpectedRequest.
	if req.path != "/youtubei/v1/browse" {
		t.Errorf("want path %q, got %q", "/youtubei/v1/browse", req.path)
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
		// BOTH context fields, against literals. The client name alone left the
		// VERSION unpinned, and a changed version alters API behavior while
		// this suite stays green (884-R1F2).
		if name, _ := client["clientName"].(string); name != "WEB_REMIX" {
			t.Errorf("%s should use client %q, got %q", req.path, "WEB_REMIX", name)
		}
		if ver, _ := client["clientVersion"].(string); ver != "1.20240101.01.00" {
			t.Errorf("%s should pin client version %q, got %q", req.path, "1.20240101.01.00", ver)
		}
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

// TestSearch_NoCandidatesIsErrNotFound covers a well-formed search response
// that simply carries no candidate shelf. The parser reports "no candidates,
// no error" and it is Search that turns that into ErrNotFound, so this
// asserts the call-level classification the parser deliberately leaves to
// its caller. Distinct from the "confident wrong hit" trap in doc.go, where
// a response DOES carry a candidate that merely does not correspond.
func TestSearch_NoCandidatesIsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"contents":{"tabbedSearchResultsRenderer":{"tabs":[]}}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL

	if _, err := c.Search(context.Background(), "artist", "title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
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

// TestBrowse_MalformedJSONIsErrNotFound covers the half a first-byte test
// cannot: a body that OPENS like an object and then breaks. Browse is the one
// call that never unmarshals -- it hands bytes to the decode package -- so
// without a full validity check it returned (nil error, undecodable bytes),
// the exact hand-off its own comment claimed to prevent (854-R5F1).
// Search and Next get this for free from their json.Unmarshal.
func TestBrowse_MalformedJSONIsErrNotFound(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"truncated_object", `{"contents":`},
		{"truncated_nested", `{"contents":{"sectionListRenderer":`},
		{"object_then_garbage", `{"contents":{}}garbage`},
		{"unclosed_string", `{"contents":"unterminated`},
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
				t.Fatalf("want ErrNotFound for a malformed payload, got %v", err)
			}
			if raw != nil {
				t.Errorf("want nil bytes, got %d: handing these downstream is the defect", len(raw))
			}
		})
	}
}

// TestNext_EmptyOrUnusableBodyIsErrNotFound pins Next's miss/transport
// boundary, which had no call-level test at all (854-R5F3). Next is the call
// where that boundary matters most: ErrNoLyricsTab wrapping ErrNotFound is
// what stops the worker treating a clean miss as a transport failure and
// ramping backoff toward retiring the row -- the #607 shape.
func TestNext_EmptyOrUnusableBodyIsErrNotFound(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"whitespace", "   \n\t"},
		{"html_error_page", "<html><body>captive portal</body></html>"},
		{"json_scalar", `"just a string"`},
		{"json_null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			c := NewClient()
			c.baseURL = srv.URL

			got, err := c.Next(context.Background(), "someVideoId")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("want an ErrNotFound-class miss, got %v", err)
			}
			if got != "" {
				t.Errorf("want no browseID on a miss, got %q", got)
			}
		})
	}
}

// TestClientContext_PinsLocale asserts every call sends hl/gl (854-R4F1): an
// unpinned locale is what let the gateway localize the tab title in the
// first place.
func TestClientContext_PinsLocale(t *testing.T) {
	srv := &fixtureServer{fixtures: map[string]string{
		searchPath: "search.json",
		nextPath:   "next.json",
		browsePath: "browse.json",
	}}
	c := newTestClient(t, srv)

	if _, err := c.Search(context.Background(), "artist", "title"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := c.Next(context.Background(), "NrgmdOz227I"); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, err := c.Browse(context.Background(), "MPLYt_Cn67yAcHym7-13"); err != nil {
		t.Fatalf("Browse: %v", err)
	}

	reqs := srv.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("want 3 requests, got %d", len(reqs))
	}
	for _, req := range reqs {
		clientCtx, _ := req.body["context"].(map[string]any)
		client, _ := clientCtx["client"].(map[string]any)
		if hl, _ := client["hl"].(string); hl != requestHl {
			t.Errorf("%s: hl = %q, want %q", req.path, hl, requestHl)
		}
		if gl, _ := client["gl"].(string); gl != requestGl {
			t.Errorf("%s: gl = %q, want %q", req.path, gl, requestGl)
		}
	}
}
