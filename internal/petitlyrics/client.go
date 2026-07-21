// Package petitlyrics implements a lyrics provider adapter for Petit Lyrics.
//
// It targets the structured API at p0.petitlyrics.com used by the vendor's own
// client applications: a single form-encoded POST returning an XML document that
// carries track metadata and a base64 lyrics payload together. The request and
// response shapes are inferred from observation and may change without notice.
// The maintainer has accepted the access-mechanism ToS risk; Petit Lyrics
// content is JASRAC/NexTone-licensed.
//
// This replaces an earlier HTML-scraping client that drove three endpoints
// (search page, a CSRF token in a static JS file, and an AJAX lyrics call). That
// path was removed rather than repaired: it was broken in four independent ways
// (issue #495), and even fully repaired the web surface exposes no timestamps
// and no ISRC or duration, so it could not serve synced lyrics at all.
//
// Request cost is one call when the word-synced tier has the track, and two when
// it does not (the client then retries at the unsynced tier). The scrape needed
// three calls plus two large HTML pages in every case. The API also requires no
// cookies, session, or CSRF token, and returns ISRC, duration, and word-level
// timings, none of which the web surface exposes.
//
// The client mirrors the structure and pacing of internal/musixmatch: a *Client
// holding an *http.Client and a min pacing interval, exposing FindLyrics with
// the shared provider signature.
package petitlyrics

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
)

// defaultBaseURL is the real Petit Lyrics API host. Tests override baseURL to
// point at an httptest.Server.
const defaultBaseURL = "https://p0.petitlyrics.com"

// apiPath is the single endpoint this client drives.
const apiPath = "/api/GetPetitLyricsData.php"

// providerName is the canonical name of this provider.
const providerName = "petitlyrics"

// defaultClientAppID and terminalType identify the calling application to the
// API. Both are required; the API returns no results without them.
const (
	defaultClientAppID = "p1110417"
	terminalType       = "10"
)

// userAgent identifies canticle honestly.
//
// This is load-bearing, not cosmetic. The service denylists known automation
// User-Agents: Go's default "Go-http-client/1.1" was refused with HTTP 403 on
// every request, which is the entire root cause of issue #495. A self-
// identifying UA is accepted, so there is no need to impersonate a browser.
const userAgent = "canticle/1.0 (+https://github.com/sydlexius/canticle)"

// Client communicates with the Petit Lyrics API.
type Client struct {
	httpClient *http.Client

	// baseURL is the host root; injectable so tests can target httptest.
	baseURL string

	// clientAppID identifies the calling application; injectable for tests.
	clientAppID string

	// pacer fields -- zero value means no pacing (minInterval == 0).
	mu          sync.Mutex
	minInterval time.Duration
	lastRequest time.Time
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool
}

// NewClient creates a new Petit Lyrics client.
func NewClient() *Client {
	c := &Client{
		baseURL:     defaultBaseURL,
		clientAppID: defaultClientAppID,
		now:         time.Now,
		sleep:       ctxSleep,
	}
	c.httpClient = &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: c.checkRedirect,
	}
	return c
}

// checkRedirect pins redirects to the configured base host. The default
// http.Client follows up to 10 redirects without restricting the target host,
// so a 3xx from the API could otherwise move a request to an arbitrary host (an
// SSRF vector). This rejects cross-host redirects and preserves the standard
// 10-hop cap.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("petitlyrics: stopped after 10 redirects")
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("petitlyrics: parse base URL: %w", err)
	}
	if req.URL.Host != base.Host {
		return fmt.Errorf("petitlyrics: refusing cross-host redirect to %q", req.URL.Host)
	}
	return nil
}

// ctxSleep sleeps for d, returning true when the sleep completes and false when
// ctx is canceled before d elapses.
func ctxSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// MinAllowedInterval is the hard floor on the pacing interval for any positive
// value. Petit Lyrics publishes no rate limits (no robots.txt on either host, no
// rate-limit response headers), so the floor is set by policy rather than read
// from the service: comfortably slower than anything plausibly enforced, and
// deliberately not tuned by probing the service until it refuses.
//
// Note the interval is enforced per REQUEST, not per lookup, which is what makes
// it safe for the two-request miss path: a track the provider does not have
// costs a word-sync call plus an unsynced retry, and both are paced. Because
// petitlyrics is a fallback lane that only sees what the primary provider
// missed, that two-request path is the common case, not the exception -- so the
// floor is set against it rather than against the cheaper hit path.
const MinAllowedInterval = 10 * time.Second

// DefaultMinInterval is the recommended pacing interval: 30s, or 120 tracks/hr.
// Petit Lyrics is a fallback lane that only sees what the primary provider
// misses, so real demand sits well under this.
const DefaultMinInterval = 30 * time.Second

// WithMinInterval sets the minimum duration between outbound requests and
// returns the receiver for chaining.
//
// A zero or negative value disables pacing (the default), which is what one-shot
// CLI fetches and tests want. Any POSITIVE value is clamped up to
// MinAllowedInterval, so a misconfigured cooldown cannot make this lane
// impolite.
//
// The clamp bounds requests this client ISSUES. It does not bound redirect hops:
// pacing runs once before http.Client.Do, and the transport follows any 3xx
// inside that call. A server returning redirects can therefore drive up to the
// 10-hop cap (same-host only, per checkRedirect) without waiting. Bounded and
// same-host, so the exposure is small, but the guarantee is "paced calls", not
// "paced HTTP round-trips".
//
// Not goroutine-safe; call before sharing the client.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	if d > 0 && d < MinAllowedInterval {
		slog.Warn("petitlyrics: configured cooldown is below the allowed floor; clamping",
			"configured", d, "floor", MinAllowedInterval)
		d = MinAllowedInterval
	}
	c.minInterval = d
	return c
}

// MinInterval returns the configured minimum request interval. Zero means
// pacing is disabled.
func (c *Client) MinInterval() time.Duration {
	return c.minInterval
}

// pace enforces the minimum request interval, mirroring the musixmatch pacer.
// The wait is ctx-cancellable.
func (c *Client) pace(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	for {
		c.mu.Lock()
		now := c.now()
		wait := c.minInterval - now.Sub(c.lastRequest)
		if wait <= 0 {
			c.lastRequest = now
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()

		slog.Debug("petitlyrics pacer: waiting before next request", "wait", wait)
		if !c.sleep(ctx, wait) {
			return fmt.Errorf("petitlyrics: pace: %w", ctx.Err())
		}
	}
}

// Name returns the provider name.
func (c *Client) Name() string {
	return providerName
}

// statusError maps a non-200 HTTP status to a sentinel error, or nil if the
// status is 200.
func statusError(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("petitlyrics: HTTP 401: %w", ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("petitlyrics: HTTP 403: %w", ErrForbidden)
	case http.StatusTooManyRequests:
		return fmt.Errorf("petitlyrics: HTTP 429: %w", ErrRateLimited)
	default:
		return fmt.Errorf("petitlyrics: unexpected HTTP status %d", status)
	}
}

const maxResponseSize = 8 << 20 // 8 MiB; word-sync payloads run to a few hundred KB

// readBody reads a capped response body.
func readBody(res *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("petitlyrics: read body: %w", err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("petitlyrics: response too large (%d bytes)", len(body))
	}
	return body, nil
}

// apiResponse mirrors the XML document returned by the API.
type apiResponse struct {
	XMLName xml.Name  `xml:"response"`
	Songs   []apiSong `xml:"songs>song"`
}

// apiSong is one <song> element. Only the fields this client uses are mapped;
// the response carries additional metadata (writer, composer, jancode, jasracID,
// cdc, upload/release dates) that is intentionally ignored.
type apiSong struct {
	LyricsID   string `xml:"lyricsId"`
	Title      string `xml:"title"`
	Artist     string `xml:"artist"`
	Album      string `xml:"album"`
	ISRC       string `xml:"isrc"`
	DurationMS int    `xml:"duration"`
	LyricsType int    `xml:"lyricsType"`
	LyricsData string `xml:"lyricsData"`
}

// FindLyrics looks up lyrics for the given track.
//
// It requests the word-synced tier first: word-sync is a superset of line-sync
// (a line's cue is its first word's start time), and its payload is plain XML
// whereas the line-sync tier is an encrypted binary blob this client cannot yet
// decode. When the word-sync request yields nothing usable, it retries at the
// unsynced tier so a track with only plain lyrics still produces a result.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	song, err := c.lookup(ctx, track, tierWordSync)
	if err == nil {
		return song, nil
	}
	// A word-sync miss is not a track miss: fall back to plain lyrics. Transport
	// and policy failures (401/403/429, context cancellation) are returned as-is
	// so the circuit breaker sees them.
	if !isTierMiss(err) {
		return models.Song{}, err
	}
	slog.Debug("petitlyrics: word-sync tier unavailable, retrying unsynced",
		"track", track.TrackName, "reason", err)
	return c.lookup(ctx, track, tierUnsynced)
}

// isTierMiss reports whether err means "this tier had nothing" rather than "the
// request failed", which decides whether falling back to another tier is worth a
// second request.
func isTierMiss(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupportedTier)
}

// lookup performs one API request at the given tier and decodes the result.
func (c *Client) lookup(ctx context.Context, track models.Track, tier int) (models.Song, error) {
	songs, err := c.request(ctx, track, tier)
	if err != nil {
		return models.Song{}, err
	}

	candidate, err := selectCandidate(songs, track)
	if err != nil {
		return models.Song{}, fmt.Errorf("petitlyrics: no candidate matched: %w", err)
	}
	if candidate.LyricsData == "" {
		return models.Song{}, fmt.Errorf("petitlyrics: candidate carried no lyrics payload: %w", ErrNotFound)
	}

	raw, err := base64.StdEncoding.DecodeString(candidate.LyricsData)
	if err != nil {
		return models.Song{}, fmt.Errorf("petitlyrics: base64 decode lyrics: %w", err)
	}

	song := models.Song{Track: trackFromCandidate(candidate, track)}

	switch classifyPayload(raw) {
	case tierWordSync:
		cues, _, err := decodeWordSync(raw)
		if err != nil {
			return models.Song{}, err
		}
		// Run the shared normalizer so this lane holds the same one-cue-per-line
		// model as every other write path (#470).
		song.Subtitles = lrcnormalize.Expand(models.Synced{Lines: cues})
		return song, nil

	case tierLineSync:
		return models.Song{}, fmt.Errorf("petitlyrics: lyricsType 2 (encrypted LSY): %w", ErrUnsupportedTier)

	default:
		text := decodeUnsynced(raw)
		if strings.TrimSpace(text) == "" {
			return models.Song{}, fmt.Errorf("petitlyrics: empty lyrics payload: %w", ErrNotFound)
		}
		// A plain-text payload may still carry LRC timestamps; prefer them.
		if doc := lrcnormalize.ParseBody(text); len(doc.Cues) > 0 {
			song.Subtitles = models.Synced{Lines: doc.Cues}
			return song, nil
		}
		song.Lyrics.LyricsBody = text
		return song, nil
	}
}

// trackFromCandidate fills a models.Track from the provider's metadata, keeping
// the local track's values where the provider has none.
func trackFromCandidate(s apiSong, local models.Track) models.Track {
	t := local
	if s.Title != "" {
		t.TrackName = s.Title
	}
	if s.Artist != "" {
		t.ArtistName = s.Artist
	}
	if s.Album != "" {
		t.AlbumName = s.Album
	}
	if s.ISRC != "" {
		t.ISRC = s.ISRC
	}
	if s.DurationMS > 0 {
		t.TrackLength = s.DurationMS / 1000
	}
	t.HasLyrics = 1
	return t
}

// request performs the single form POST and decodes the XML envelope.
func (c *Client) request(ctx context.Context, track models.Track, tier int) ([]apiSong, error) {
	form := url.Values{
		"clientAppId":  {c.clientAppID},
		"terminalType": {terminalType},
		"lyricsType":   {strconv.Itoa(tier)},
		"key_title":    {track.TrackName},
		"key_artist":   {track.ArtistName},
		"key_album":    {track.AlbumName},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+apiPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("petitlyrics: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	if err := c.pace(ctx); err != nil {
		return nil, err
	}
	res, err := c.httpClient.Do(req) //nolint:gosec // G704: the request host is c.baseURL (a fixed const, test-only override) and CheckRedirect pins redirects to that host, so a 3xx cannot move the request off-host; track inputs go in the form body, not the URL. No SSRF vector.
	if err != nil {
		return nil, fmt.Errorf("petitlyrics: request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if err := statusError(res.StatusCode); err != nil {
		return nil, err
	}

	body, err := readBody(res)
	if err != nil {
		return nil, err
	}

	var parsed apiResponse
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("petitlyrics: decode XML response: %w", err)
	}
	if len(parsed.Songs) == 0 {
		return nil, fmt.Errorf("petitlyrics: no songs in response: %w", ErrNotFound)
	}
	return parsed.Songs, nil
}
