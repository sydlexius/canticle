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
// Request cost is exactly one call per lookup, hit or miss: the API returns
// whatever tier the track has, so a single request settles it. The scrape needed
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
	//
	// mu also guards the zero-result outage counter below. One mutex covers both
	// because the client is shared across worker goroutines and the two pieces of
	// state are touched on the same request path; a second lock would buy nothing
	// but a lock-ordering hazard.
	mu          sync.Mutex
	minInterval time.Duration
	lastRequest time.Time
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool

	// consecutiveZero counts back-to-back zero-result responses (#607). Reset by
	// any response carrying at least one song.
	consecutiveZero int
	// zeroReported latches the transition so the outage is logged ONCE rather
	// than on every request past the threshold. Cleared on recovery.
	zeroReported bool
}

// recordZeroResult counts a zero-song response and reports whether the run has
// reached the escalation threshold. Called for a response that parsed cleanly and
// simply carried no songs -- never for a transport or status failure, which say
// nothing about whether the application id still works.
// The log emission is deliberately OUTSIDE the critical section. slog handlers
// can block on I/O and take locks of their own, and this mutex also paces every
// outbound request, so logging under it would serialize concurrent lookups on
// the handler -- worst at exactly the moment an outage transition fires. The
// pacer at pace() already follows this shape; match it.
func (c *Client) recordZeroResult() bool {
	c.mu.Lock()
	c.consecutiveZero++
	count := c.consecutiveZero
	reached := count >= ZeroResultThreshold
	// Latch INSIDE the lock so exactly one goroutine can win the transition and
	// emit the warning, even though the emission happens after the unlock.
	transitioned := reached && !c.zeroReported
	if transitioned {
		c.zeroReported = true
	}
	c.mu.Unlock()

	if transitioned {
		slog.Warn("petitlyrics: provider has returned no results for a sustained run of lookups; the application id may have been revoked",
			"consecutive", count, "threshold", ZeroResultThreshold)
	}
	return reached
}

// recordNonZeroResult clears the outage counter. Any response carrying at least
// one song proves the application id is still accepted, whatever the client then
// makes of the payload.
// The recovery log is emitted after the unlock, for the same reason as the
// warning in recordZeroResult.
func (c *Client) recordNonZeroResult() {
	c.mu.Lock()
	recovered := c.zeroReported
	after := c.consecutiveZero
	c.zeroReported = false
	c.consecutiveZero = 0
	c.mu.Unlock()

	if recovered {
		slog.Info("petitlyrics: provider returned results again; the sustained zero-result run has ended",
			"after", after)
	}
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
// The interval is enforced per REQUEST, which is now the same as per lookup:
// every lookup costs exactly one call. That matters for a fallback lane, whose
// traffic is mostly misses (90 of 120 in a measured sample) -- misses are paced
// identically to hits and cost no more.
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

// apiSong is one <song> element. Only the fields this client uses are mapped.
//
// The response carries MORE than the fields listed here. A verified inventory
// lives in docs/superpowers/specs/2026-07-21-petitlyrics-api-rewrite-design.md;
// among the deliberately unmapped are writer, composer, jancode, jasracID, cdc,
// artistId, prefferedLyricsType, uploadDate, releaseDate.
// Do not treat that list as exhaustive either: it reflects observed responses.
type apiSong struct {
	LyricsID   string `xml:"lyricsId"`
	Title      string `xml:"title"`
	Artist     string `xml:"artist"`
	Album      string `xml:"album"`
	ISRC       string `xml:"isrc"`
	DurationMS int    `xml:"duration"`
	LyricsType int    `xml:"lyricsType"`
	LyricsData string `xml:"lyricsData"`
	// IsOfficial, Copyright, and AvailableTier are decoded for MEASUREMENT ONLY
	// (#615, #600). Nothing in the fetch path consumes them: whether isOfficial
	// can carry a per-result trust signal is exactly what the survey probe is
	// measuring, and availableLyricsType is decoded so the probe can report how
	// often it agrees with the tier the payload bytes actually carry. Neither
	// selection nor classification may consult AvailableTier: the payload stays
	// the authoritative discriminator (see classifyPayload and selectCandidate).
	IsOfficial    string `xml:"isOfficial"`
	Copyright     string `xml:"copyright"`
	AvailableTier int    `xml:"availableLyricsType"`
}

// FindLyrics looks up lyrics for the given track in a single request.
//
// It always asks for the word-synced tier, and that one request is the whole
// flow: the API returns whatever tier the track actually has, not only the tier
// requested. Measured over 107 hits, the response tier matched the track's
// advertised availableLyricsType with no exceptions -- a tier-3 request returns
// an unsynced payload for an unsynced-only track, and a line-sync payload for a
// line-sync-only one.
//
// So a tier-3 miss is a genuine miss. An earlier version retried at the unsynced
// tier on a miss, which could never rescue anything and simply doubled outbound
// volume on the miss path -- the dominant path for a fallback lane that only
// sees what the primary provider missed (90 of 120 lookups in a measured sample).
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	return c.lookup(ctx, track, tierWordSync)
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
		cues, timings, err := decodeWordSync(raw)
		if err != nil {
			return models.Song{}, err
		}
		// Run the shared normalizer so this lane holds the same one-cue-per-line
		// model as every other write path (#470).
		expanded := lrcnormalize.Expand(models.Synced{Lines: cues})
		song.Subtitles = expanded
		// Word timings are positional indices into the cue slice, so they are only
		// safe to attach when no cue was SPLIT. Expand splits a cue whose TEXT
		// carries an embedded timestamp, which shifts every later index and would
		// leave the timings pointing at the wrong words.
		//
		// The length comparison detects a split, which is not the same as proving
		// the order is unchanged: Expand ends in a sort, so in general it can
		// reorder at constant length. That cannot happen HERE because
		// decodeWordSync sorts by first-word start time before assigning indices
		// and msToTime is monotone, making Expand's stable sort a no-op on this
		// path -- an invariant defended by
		// TestDecodeWordSync_OrderingIsStableThroughExpand. If that sort is ever
		// removed, this check stops being sufficient.
		if len(expanded.Lines) == len(cues) {
			song.WordTimings = timings
		} else {
			// Info, not Debug: this silently demotes a result a full quality tier,
			// and it should be rare. If it ever becomes common that signals a
			// payload-shape change worth noticing in production.
			slog.Info("petitlyrics: cue normalization split a line; dropping word timings",
				"track", track.TrackName, "before", len(cues), "after", len(expanded.Lines))
		}
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
	res, err := c.httpClient.Do(req) //nolint:gosec // reason: G704 - the request host is c.baseURL (a fixed const, test-only override) and CheckRedirect pins redirects to that host, so a 3xx cannot move the request off-host; track inputs go in the form body, not the URL. No SSRF vector.
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
		// A clean HTTP 200 carrying no songs. Individually this is an ordinary
		// miss; sustained, it is the fingerprint of a revoked application id,
		// since the service keeps answering normally rather than returning 401
		// (#607).
		if c.recordZeroResult() {
			return nil, ErrProviderUnavailable
		}
		return nil, fmt.Errorf("petitlyrics: no songs in response: %w", ErrNotFound)
	}
	// At least one song came back, which proves the application id is still
	// accepted. Reset regardless of what the client then makes of the payload: a
	// candidate that loses selection, or a tier this client cannot decode, is a
	// per-track outcome and says nothing about credential health.
	c.recordNonZeroResult()
	return parsed.Songs, nil
}
