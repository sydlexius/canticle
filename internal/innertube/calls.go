package innertube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Endpoint paths for the three-call chain -- see doc.go.
const (
	searchPath = "/youtubei/v1/search"
	nextPath   = "/youtubei/v1/next"
	browsePath = "/youtubei/v1/browse"
)

// requestHl/requestGl pin the response language and country the API renders
// for us. Without them the gateway picks a locale from the caller's IP or
// account, so every human-readable string in the response -- tab titles
// included -- varies by where canticle happens to run (854-R4F1). Pinning
// them is defense in depth behind the pageType match in parseLyricsBrowseID,
// not a substitute for it: a stable machine token is the primary key, and a
// pinned locale is only what makes the display-string FALLBACK deterministic
// for a response that carries no pageType.
const (
	requestHl = "en"
	requestGl = "US"
)

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

// requestContext is the "context.client" envelope every innertube call
// carries, identifying which client is asking.
type requestContext struct {
	Client clientContext `json:"client"`
}

type clientContext struct {
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
	Hl            string `json:"hl"`
	Gl            string `json:"gl"`
}

// newClientContext builds the client envelope for one call, pinning the
// locale on every request so no call site can forget it.
func newClientContext(name, version string) requestContext {
	return requestContext{Client: clientContext{
		ClientName:    name,
		ClientVersion: version,
		Hl:            requestHl,
		Gl:            requestGl,
	}}
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
		Context: newClientContext(webClientName, webClientVersion),
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
		Context: newClientContext(webClientName, webClientVersion),
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
		Context:  newClientContext(browseClientName, browseClientVersion),
		BrowseID: browseID,
	}
	raw, err := c.postJSON(ctx, browsePath, body)
	if err != nil {
		return nil, fmt.Errorf("innertube: browse: %w", err)
	}
	// Browse is the ONE call that never unmarshals: it hands the bytes to the
	// decode package. So it validates them fully rather than only checking how
	// they START. Search and Next get this for free -- an undecodable body
	// fails their json.Unmarshal -- but a truncated payload like `{"contents":`
	// opens like an object and passes a first-byte test, which handed decode
	// (nil error, undecodable bytes): the exact hand-off 854-F5 exists to
	// prevent, and which its comment wrongly claimed to cover (854-R5F1).
	if isUnusableBody(raw) || !json.Valid(raw) {
		// A 200 with an empty, all-whitespace, non-JSON, or malformed body is
		// not a payload under any parse (854-F5). ErrNotFound is the right
		// class -- a clean answer with nothing usable in it, matching
		// Search/Next (854-F4) -- not a transport failure, since nothing about
		// the request or connection failed.
		return nil, fmt.Errorf("innertube: browse: unusable response body: %w", ErrNotFound)
	}
	return raw, nil
}
