package innertube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is the real innertube API host. Tests override baseURL to
// point at an httptest.Server.
const defaultBaseURL = "https://music.youtube.com"

// Endpoint paths for the three-call chain -- see doc.go.
const (
	searchPath = "/youtubei/v1/search"
	nextPath   = "/youtubei/v1/next"
	browsePath = "/youtubei/v1/browse"
)

// apiKey is the public, non-authenticating InnerTube key baked into YouTube
// Music's own web client and reused by every known unofficial client library
// (ytmusicapi, yt-dlp). It identifies the calling API surface to Google's
// gateway, not a user or app credential -- there is nothing secret behind it,
// and every consumer of this unofficial API ships the same value.
const apiKey = "AIzaSyC9XL3ZjWddXya6X74dJoCTL-WEYFDNX30" //nolint:gosec // reason: G101 - not a credential; the public InnerTube key baked into YouTube Music's own web client, reused by every unofficial client library. See the comment above.

// providerName is the canonical name of this provider.
const providerName = "innertube"

// userAgent identifies canticle honestly, mirroring internal/petitlyrics
// (issue #495): an automation-looking UA risks a denylist response, and there
// is no need to impersonate a browser or the official app.
const userAgent = "canticle/1.0 (+https://github.com/sydlexius/canticle)"

// Client context strings.
//
// webClientName/webClientVersion drive search and next: both are plain
// browsing/metadata calls, and the fixtures for both were captured against
// WEB_REMIX.
//
// browseClientName/browseClientVersion drive ONLY the browse call, and this
// pair is the single most fragile fact in this package -- see doc.go.
// WEB_REMIX returns plain, unsynced text for the identical browseId;
// ANDROID_MUSIC 7.03.52 (this pinned version) and IOS_MUSIC 7.04.2 both
// return timed cues; ANDROID_MUSIC 5.16.51 returns HTTP 400 (see
// statusError's ErrClientVersion mapping below). Changing either constant
// silently downgrades every future browse() call to untimed text -- exactly
// the failure TestBrowse_UsesTimedLyricsClientContext exists to catch.
const (
	webClientName    = "WEB_REMIX"
	webClientVersion = "1.20240101.01.00"

	browseClientName    = "ANDROID_MUSIC"
	browseClientVersion = "7.03.52"
)

// searchParamsSongsOnly restricts a search call to the Songs shelf. It is an
// opaque, undocumented protobuf-encoded value reverse engineered and
// published by unofficial YouTube Music clients; Google documents none of the
// innertube surface, so this is trusted the same way the client-context
// strings above are -- by observed behavior, not by spec. If YouTube ever
// changes its meaning the failure mode is a broader (not narrower) result
// set, which the correspondence guard downstream (see doc.go) already
// exists to police.
const searchParamsSongsOnly = "Eg-KAQwIARAAGAAgACgAMABqChAEEAUQAxAKEAk="

// maxResponseSize caps every response body read by this client. 8 MiB is
// generous headroom over any observed payload (the browse fixture's timed
// cue data runs to tens of KB; the surrounding tracking/context noise this
// client discards is the largest part of a real response).
const maxResponseSize = 8 << 20

// Client communicates with YouTube Music's unauthenticated innertube API.
type Client struct {
	httpClient *http.Client

	// baseURL is the host root; injectable so tests can target httptest.
	baseURL string

	// key is the InnerTube API key sent as a query parameter; injectable for
	// tests, though no test currently relies on a non-default value.
	key string
}

// NewClient creates a new innertube client.
func NewClient() *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		key:     apiKey,
	}
	c.httpClient = &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: c.checkRedirect,
	}
	return c
}

// checkRedirect pins redirects to the configured base host AND scheme,
// mirroring internal/ffmpeg.httpsOnlyRedirect rather than
// internal/petitlyrics.Client.checkRedirect -- petitlyrics is the sibling
// that lacks a scheme check (854-F1). The default http.Client follows up to
// 10 redirects without restricting the target host or scheme, so a 3xx from
// the API could otherwise move a request to an arbitrary host (SSRF) or
// silently downgrade an https call to cleartext http (or ftp), where the
// request and response become readable/modifiable in transit. url.URL.Host
// carries no scheme, so the host comparison alone cannot catch a same-host
// downgrade -- both must be checked. This rejects cross-host and
// cross-scheme redirects and preserves the standard 10-hop cap.
//
// The scheme is compared against base.Scheme, not a hardcoded "https": every
// test in this package injects an httptest http://127.0.0.1 baseURL, and a
// hardcoded https would refuse every redirect those tests follow -- a green
// suite alone does not prove the guard refuses a downgrade, only that it
// isn't hardcoded; see TestClient_RefusesSchemeDowngradeOnTheWire for the
// actual end-to-end proof against a real listener.
//
// req.URL.Scheme != base.Scheme means a Client constructed with a plain-http
// base would ACCEPT an http redirect and REFUSE an https one (an upgrade).
// Both are deliberate, not oversights:
//   - accepting http when the base is already http adds no exposure -- the
//     channel was cleartext to begin with, so there is nothing left to
//     downgrade.
//   - refusing an https upgrade from an http base is fail-closed and
//     harmless (the caller loses a redirect it could have safely followed),
//     called out here so a future reader does not "fix" it into accepting
//     the upgrade, which would reintroduce scheme-mismatch exposure the
//     other direction.
//
// I checked every assignment to baseURL in this package (grep -rn
// "baseURL" internal/innertube): NewClient sets it once, from the
// unconditional const defaultBaseURL = "https://music.youtube.com"; every
// other assignment is test code overriding it to an httptest URL. No
// config, env, or CLI path in this repo constructs an innertube.Client with
// a non-https base -- so the plain-http-base case above is exercised only
// by this package's own tests, never by production. If a future change adds
// a configurable base URL, that caller becomes responsible for deciding
// whether to allow a non-https value at all; this guard's job stays
// "don't let a redirect change origin out from under the caller's choice."
//
// url.Parse lowercases the scheme it returns regardless of the input's
// casing (verified: "HTTPS://..." and "Https://..." both parse to
// scheme "https"), so an uppercase scheme in a redirect Location cannot be
// used to bypass this comparison by case.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("innertube: stopped after 10 redirects")
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("innertube: parse base URL: %w", err)
	}
	if !sameOrigin(req.URL, base) {
		return fmt.Errorf("innertube: refusing cross-origin redirect to %q", req.URL.String())
	}
	return nil
}

// sameOrigin reports whether a and b share a scheme and host, treating an
// explicit default port for the scheme as equivalent to an implicit one
// (854-F6): "https://music.youtube.com" and "https://music.youtube.com:443"
// are the same origin, but url.URL.Host carries the port verbatim, so a
// literal comparison would refuse a same-origin redirect that merely spells
// out the default port. Normalizing only the DEFAULT port keeps the guard's
// core cross-host protection unweakened -- an explicit non-default port
// still compares as written, so a redirect from :443 to :8443 on the same
// host is still refused.
func sameOrigin(a, b *url.URL) bool {
	if a.Scheme != b.Scheme {
		return false
	}
	return stripDefaultPort(a) == stripDefaultPort(b)
}

// stripDefaultPort returns u.Host with its port removed if that port is the
// default for u.Scheme, so "host:443" (https) and "host:80" (http) compare
// equal to bare "host".
//
// The suffix match is safe against two net/url guarantees rather than
// anything local: url.URL.Host keeps a bracketed IPv6 literal (e.g.
// "[::443]" ends in "3]", never ":443"), and userinfo never reaches .Host, so
// a userinfo segment ending in ":443" cannot be stripped either. A future
// caller that stops sourcing host from url.URL.Host would silently break
// this assumption.
func stripDefaultPort(u *url.URL) string {
	host := u.Host
	defaultPort := ""
	switch u.Scheme {
	case "https":
		defaultPort = "443"
	case "http":
		defaultPort = "80"
	}
	if defaultPort != "" && strings.HasSuffix(host, ":"+defaultPort) {
		return strings.TrimSuffix(host, ":"+defaultPort)
	}
	return host
}

// Name returns the provider name.
func (c *Client) Name() string {
	return providerName
}

// statusError maps a non-200 HTTP status to a sentinel error, or nil if the
// status is 200.
//
// The HTTP 400 mapping is a deliberate choice left open by errors.go: the
// only measured cause of a 400 from this API is a stale/rejected client
// version (ANDROID_MUSIC 5.16.51 -- see doc.go and the client-context
// constants above). The status code alone cannot distinguish that from a
// generic malformed request, and this client always sends a fixed,
// fixture-verified client context for every call, so a 400 reaching here is
// far more consistent with the API rejecting our pinned version than with a
// body-shape bug on our side -- there is no third, more specific signal
// available to split the two possibilities.
func statusError(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("innertube: HTTP 401: %w", ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("innertube: HTTP 403: %w", ErrForbidden)
	case http.StatusTooManyRequests:
		return fmt.Errorf("innertube: HTTP 429: %w", ErrRateLimited)
	case http.StatusBadRequest:
		return fmt.Errorf("innertube: HTTP 400: %w", ErrClientVersion)
	default:
		return fmt.Errorf("innertube: unexpected HTTP status %d", status)
	}
}

// readBody reads a capped response body.
func readBody(res *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("innertube: read body: %w", err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("innertube: response too large (%d bytes)", len(body))
	}
	return body, nil
}

// requestContext is the "context.client" envelope every innertube call
// carries, identifying which client is asking.
type requestContext struct {
	Client clientContext `json:"client"`
}

type clientContext struct {
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
}

type searchRequestBody struct {
	Context requestContext `json:"context"`
	Query   string         `json:"query"`
	Params  string         `json:"params,omitempty"`
}

type nextRequestBody struct {
	Context requestContext `json:"context"`
	VideoID string         `json:"videoId"`
}

type browseRequestBody struct {
	Context  requestContext `json:"context"`
	BrowseID string         `json:"browseId"`
}

// postJSON marshals body, POSTs it to path with the API key query parameter,
// and returns the capped raw response bytes on a 200. Context cancellation
// is honored because the request is built with NewRequestWithContext and
// issued through httpClient.Do, which returns promptly (wrapping ctx.Err())
// once ctx is done -- no goroutine here ever blocks past that.
func (c *Client) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("innertube: encode request body: %w", err)
	}

	u := c.baseURL + path + "?key=" + url.QueryEscape(c.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("innertube: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("innertube: request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if err := statusError(res.StatusCode); err != nil {
		return nil, err
	}
	return readBody(res)
}

// Search issues the innertube search call for artist+title, restricted to
// the Songs shelf, and returns every candidate this client can extract from
// the response.
//
// It parses nothing beyond what selection needs to apply the correspondence
// guard documented in doc.go: search never signals "no match", so a
// nonsense query still returns a confident, fully-timed, unrelated
// candidate here exactly as a real hit would. Verifying a candidate actually
// corresponds to the queried track is the caller's job, not this client's.
func (c *Client) Search(ctx context.Context, artist, title string) ([]SearchCandidate, error) {
	query := strings.TrimSpace(title + " " + artist)
	body := searchRequestBody{
		Context: requestContext{Client: clientContext{ClientName: webClientName, ClientVersion: webClientVersion}},
		Query:   query,
		Params:  searchParamsSongsOnly,
	}
	raw, err := c.postJSON(ctx, searchPath, body)
	if err != nil {
		return nil, fmt.Errorf("innertube: search: %w", err)
	}
	candidates, err := parseSearchCandidates(raw)
	if err != nil {
		return nil, fmt.Errorf("innertube: search: %w", err)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("innertube: search returned no candidates: %w", ErrNotFound)
	}
	return candidates, nil
}

// Next issues the innertube next call for a videoId and returns the
// lyrics-tab browseId (prefixed "MPLY"), or ErrNoLyricsTab if the video has
// no lyrics rendition.
func (c *Client) Next(ctx context.Context, videoID string) (string, error) {
	body := nextRequestBody{
		Context: requestContext{Client: clientContext{ClientName: webClientName, ClientVersion: webClientVersion}},
		VideoID: videoID,
	}
	raw, err := c.postJSON(ctx, nextPath, body)
	if err != nil {
		return "", fmt.Errorf("innertube: next: %w", err)
	}
	browseID, err := parseLyricsBrowseID(raw)
	if err != nil {
		return "", fmt.Errorf("innertube: next: %w", err)
	}
	return browseID, nil
}

// Browse issues the innertube browse call for a lyrics browseId and returns
// the raw response body carrying the timed cue payload.
//
// This deliberately returns raw bytes rather than []Cue: decoding the
// timedLyricsData payload into Cue values is out of scope for this slice
// (see the lead notes) and belongs to the decode package. The one thing this
// call must get right is the client context it sends -- see the
// browseClientName/browseClientVersion constants above and
// TestBrowse_UsesTimedLyricsClientContext.
func (c *Client) Browse(ctx context.Context, browseID string) ([]byte, error) {
	body := browseRequestBody{
		Context:  requestContext{Client: clientContext{ClientName: browseClientName, ClientVersion: browseClientVersion}},
		BrowseID: browseID,
	}
	raw, err := c.postJSON(ctx, browsePath, body)
	if err != nil {
		return nil, fmt.Errorf("innertube: browse: %w", err)
	}
	if isUnusableBody(raw) {
		// A 200 with an empty, all-whitespace, or non-JSON body is not a
		// payload under any parse (854-F5, widened by 854-R2F1): passing it
		// through as success hands the decode slice (nil error, unusable
		// bytes) and forces it to invent its own emptiness check. ErrNotFound
		// is the right class -- a clean answer with nothing usable in it,
		// matching Search/Next below (854-F4) -- not a transport failure,
		// since nothing about the request or connection failed.
		return nil, fmt.Errorf("innertube: browse: unusable response body: %w", ErrNotFound)
	}
	return raw, nil
}

// --- response parsing: chain-relevant fields only ---

// searchResponse models only the path from the top of a search response down
// to the Songs-shelf candidates this client extracts. Every other renderer
// shape present in a real response (didYouMeanRenderer, itemSectionRenderer,
// ...) is deliberately unmodeled: encoding/json ignores fields it has no
// struct tag for, so those shapes are silently skipped rather than erroring.
type searchResponse struct {
	Contents struct {
		TabbedSearchResultsRenderer struct {
			Tabs []struct {
				TabRenderer struct {
					Content struct {
						SectionListRenderer struct {
							Contents []searchSectionContent `json:"contents"`
						} `json:"sectionListRenderer"`
					} `json:"content"`
				} `json:"tabRenderer"`
			} `json:"tabs"`
		} `json:"tabbedSearchResultsRenderer"`
	} `json:"contents"`
}

type searchSectionContent struct {
	MusicCardShelfRenderer *musicCardShelfRenderer `json:"musicCardShelfRenderer"`
}

type musicCardShelfRenderer struct {
	Title    runsContainer `json:"title"`
	Subtitle runsContainer `json:"subtitle"`
}

type runsContainer struct {
	Runs []textRun `json:"runs"`
}

type textRun struct {
	Text               string              `json:"text"`
	NavigationEndpoint *navigationEndpoint `json:"navigationEndpoint"`
}

type navigationEndpoint struct {
	WatchEndpoint  *watchEndpoint  `json:"watchEndpoint"`
	BrowseEndpoint *browseEndpoint `json:"browseEndpoint"`
}

type watchEndpoint struct {
	VideoID string `json:"videoId"`
}

type browseEndpoint struct {
	BrowseID string `json:"browseId"`
}

// durationPattern matches the "m:ss" or "mm:ss" duration text run innertube
// includes in a search subtitle, e.g. "2:06".
var durationPattern = regexp.MustCompile(`^\d{1,2}:\d{2}$`)

// parseDurationSeconds parses a duration text run, returning 0 (meaning "not
// supplied") for anything that does not match -- see
// SearchCandidate.DurationSeconds, which must fail open on zero.
//
// Documented edge behavior (854-F8), left as-is deliberately: durationPattern
// caps at two digits, so an "h:mm:ss" run (e.g. "1:02:03") or a track >= 100
// minutes (e.g. "100:00") does not match and returns 0 rather than a parsed
// value; an impossible seconds component like "9:99" DOES match the pattern
// and is arithmetically combined anyway (parsed as 9*60+99 = 639). Both are
// correct by construction rather than bugs: every caller treats 0 as "not
// supplied" and fails open on it (types.go), so an unparsed long-form
// duration only loses a pre-filter, never causes a wrong rejection; and "9:99"
// producing a slightly-wrong-but-nonzero second count is equally harmless,
// since the pre-filter this feeds is a coarse duration bucket, not an exact
// comparison. Fixing either would add complexity (multi-part time parsing,
// a seconds-range check) for a case that has never been observed in a real
// subtitle run and costs nothing if it ever occurs.
func parseDurationSeconds(s string) int {
	if !durationPattern.MatchString(s) {
		return 0
	}
	parts := strings.Split(s, ":")
	minutes, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	seconds, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return minutes*60 + seconds
}

// isUnusableBody reports whether raw is empty, all-whitespace, or does not
// start with a JSON value character -- i.e. it looks like a hollow 200 or a
// non-JSON interstitial (a captive portal, an edge error page, a bot
// challenge -- all realistic for an undocumented API accessed without
// credentials) rather than a genuinely malformed JSON document (854-F4). A
// caller uses this to distinguish that class from a real mid-document parse
// error, which stays a naked, unclassified error: the former is a clean miss
// ("reached it, nothing usable", matching ErrNotFound's contract in
// errors.go), the latter is a signal something is actually broken and should
// not be silently bucketed as benign.
func isUnusableBody(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	return trimmed[0] != '{' && trimmed[0] != '['
}

// parseSearchCandidates walks every tab and section of a search response and
// extracts one SearchCandidate per musicCardShelfRenderer found.
func parseSearchCandidates(raw []byte) ([]SearchCandidate, error) {
	var resp searchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		if isUnusableBody(raw) {
			return nil, fmt.Errorf("search response unusable: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	var out []SearchCandidate
	for _, tab := range resp.Contents.TabbedSearchResultsRenderer.Tabs {
		for _, section := range tab.TabRenderer.Content.SectionListRenderer.Contents {
			if section.MusicCardShelfRenderer == nil {
				continue
			}
			if cand, ok := candidateFromShelf(*section.MusicCardShelfRenderer); ok {
				out = append(out, cand)
			}
		}
	}
	return out, nil
}

// candidateFromShelf extracts a SearchCandidate from one musicCardShelfRenderer,
// or reports false if the shelf carries no usable videoId.
func candidateFromShelf(shelf musicCardShelfRenderer) (SearchCandidate, bool) {
	if len(shelf.Title.Runs) == 0 {
		return SearchCandidate{}, false
	}
	titleRun := shelf.Title.Runs[0]
	if titleRun.NavigationEndpoint == nil || titleRun.NavigationEndpoint.WatchEndpoint == nil {
		return SearchCandidate{}, false
	}
	videoID := titleRun.NavigationEndpoint.WatchEndpoint.VideoID
	if videoID == "" {
		return SearchCandidate{}, false
	}

	cand := SearchCandidate{VideoID: videoID, Title: titleRun.Text}
	artistSet := false
	for _, run := range shelf.Subtitle.Runs {
		if run.NavigationEndpoint != nil && run.NavigationEndpoint.BrowseEndpoint != nil {
			// A real subtitle reads "Song • Artist • Album • Duration", and
			// BOTH the artist run and the album run carry a browseEndpoint
			// (854-F2) -- the shipped fixture has only one such run, which
			// is why no prior test caught the last-wins overwrite. Take the
			// FIRST browse-bearing run rather than discriminating on the
			// browseId prefix ("UC..." for an artist channel, "MPRE..." for
			// an album): observed subtitle order always places the artist
			// run before the album run, and relying on run ORDER rather
			// than a prefix avoids trusting an undocumented, unversioned
			// string format as a second attack surface. If a future subtitle
			// shape ever puts a non-artist browse-bearing run first, this
			// degrades to that run's text in Artist -- still bounded by the
			// downstream correspondence guard (doc.go) that must
			// independently verify every candidate regardless.
			if !artistSet {
				cand.Artist = run.Text
				artistSet = true
			}
			continue
		}
		if d := parseDurationSeconds(run.Text); d > 0 {
			cand.DurationSeconds = d
		}
	}
	return cand, true
}

// nextResponse models only the path from the top of a next response down to
// the tab list this client scans for a Lyrics tab.
type nextResponse struct {
	Contents struct {
		SingleColumnMusicWatchNextResultsRenderer struct {
			TabbedRenderer struct {
				WatchNextTabbedResultsRenderer struct {
					Tabs []struct {
						TabRenderer struct {
							Title    string       `json:"title"`
							Endpoint *tabEndpoint `json:"endpoint"`
						} `json:"tabRenderer"`
					} `json:"tabs"`
				} `json:"watchNextTabbedResultsRenderer"`
			} `json:"tabbedRenderer"`
		} `json:"singleColumnMusicWatchNextResultsRenderer"`
	} `json:"contents"`
}

type tabEndpoint struct {
	BrowseEndpoint *browseEndpoint `json:"browseEndpoint"`
}

// lyricsBrowseIDPrefix is the browseId prefix doc.go documents for the
// Lyrics tab. Validated in parseLyricsBrowseID (854-F7): the prefix was
// previously only ASSERTED by a test against a well-formed fixture, not
// enforced by any code, which let the doc comment claim an invariant the
// parser did not actually hold.
const lyricsBrowseIDPrefix = "MPLY"

// parseLyricsBrowseID scans a next response's tabs for the one titled
// "Lyrics" and returns its browseId, or ErrNoLyricsTab if no such tab exists,
// it carries no browseId, or the browseId does not carry the documented MPLY
// prefix.
//
// A non-MPLY browseId under a "Lyrics"-titled tab has never been observed,
// but if the API ever renders one, ErrNoLyricsTab (rather than returning the
// unrecognized ID) is the right sentinel: it wraps ErrNotFound, and a
// browseId this client does not recognize as a lyrics rendition is
// indistinguishable in consequence from there being no lyrics tab at all --
// both are benign misses, not failures. Silently passing an unvalidated ID
// through to Browse risks feeding an unrelated (non-lyrics) browse target to
// a caller that assumes every browseId it receives is a lyrics tab.
func parseLyricsBrowseID(raw []byte) (string, error) {
	var resp nextResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		if isUnusableBody(raw) {
			return "", fmt.Errorf("next response unusable: %w", ErrNotFound)
		}
		return "", fmt.Errorf("decode next response: %w", err)
	}

	for _, tab := range resp.Contents.SingleColumnMusicWatchNextResultsRenderer.TabbedRenderer.WatchNextTabbedResultsRenderer.Tabs {
		tr := tab.TabRenderer
		if tr.Title != "Lyrics" {
			continue
		}
		if tr.Endpoint == nil || tr.Endpoint.BrowseEndpoint == nil || tr.Endpoint.BrowseEndpoint.BrowseID == "" {
			continue
		}
		browseID := tr.Endpoint.BrowseEndpoint.BrowseID
		if !strings.HasPrefix(browseID, lyricsBrowseIDPrefix) {
			continue
		}
		return browseID, nil
	}
	return "", fmt.Errorf("innertube: no lyrics tab found: %w", ErrNoLyricsTab)
}
