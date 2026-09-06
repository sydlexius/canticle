package innertube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// defaultBaseURL is the real innertube API host. Tests override baseURL to
// point at an httptest.Server.
const defaultBaseURL = "https://music.youtube.com"

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
		Timeout: 30 * time.Second,
		// A closure over c, NOT a capture of c.baseURL's VALUE: it reads the
		// field at REDIRECT time, so a caller that repoints the client after
		// NewClient still gets the guard pinned to the new host rather than to
		// the default one. (A bound method value would be equivalent -- it too
		// reads through the receiver at call time. The refactor that WOULD
		// break this is hoisting c.baseURL into a local and closing over
		// that.)
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRedirect(c.baseURL, req, via)
		},
	}
	return c
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

	// STRUCTURAL join, not string concatenation (882-R1F1). Concatenating let
	// a hostile path escape the base host entirely: `"@evil.example/x"` made
	// "music.youtube.com" read as USERINFO and sent the request to
	// evil.example. The redirect guard cannot catch that -- no redirect is
	// involved, the request goes to the wrong host directly. Two lesser
	// defects came with it: a path carrying a fragment silently DROPPED the
	// key, and one carrying a query produced a double "?".
	//
	// ResolveReference against the parsed base cannot leave the base's host
	// unless the reference is itself absolute, which the check below refuses.
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("innertube: parse base URL: %w", err)
	}
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("innertube: parse request path: %w", err)
	}
	// Refuse anything that could redirect the request off-host. Every caller
	// passes a fixed literal, so this is a guard against a FUTURE caller, and
	// it fails closed rather than silently retargeting.
	if ref.IsAbs() || ref.Host != "" || ref.User != nil {
		return nil, fmt.Errorf("innertube: request path must be relative, got %q", path)
	}
	u := base.ResolveReference(ref)
	q := u.Query()
	q.Set("key", c.key)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
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
