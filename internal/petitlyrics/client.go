// Package petitlyrics implements a lyrics provider adapter for petitlyrics.com.
//
// Petit Lyrics has no public API. This adapter drives a set of reverse-
// engineered endpoints (HTML search, a CSRF token embedded in a static JS
// file, and an AJAX lyrics endpoint), so the request and response shapes are
// inferred and may change without notice. The maintainer has accepted the
// access-mechanism ToS risk; Petit Lyrics content is JASRAC/NexTone-licensed.
//
// The client mirrors the structure and pacing of internal/musixmatch: a
// *Client holding an *http.Client, a min pacing interval, and (here) CSRF /
// session state, exposing FindLyrics with the shared provider signature.
package petitlyrics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
)

// defaultBaseURL is the real petitlyrics.com host. Tests override baseURL to
// point at an httptest.Server.
const defaultBaseURL = "https://petitlyrics.com"

// providerName is the canonical name of this provider.
const providerName = "petitlyrics"

// lyricsLinkRe extracts the lyrics id from a "/lyrics/<id>" href in the search
// response HTML. The id is numeric in observed responses.
var lyricsLinkRe = regexp.MustCompile(`/lyrics/(\d+)`)

// csrfTokenRe extracts the CSRF token from the static pl-lib.js file. The token
// is assigned to a JS variable as a quoted string in observed responses.
var csrfTokenRe = regexp.MustCompile(`csrfToken\s*[:=]\s*["']([^"']+)["']`)

// candidateBlockRe matches the inner content of a single <li> result block
// (dot-all so the block can span multiple lines).
var candidateBlockRe = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`)

// candidateAlbumRe extracts the text from a lyrics-list-album element.
// The class token may appear alongside other classes (e.g. "lyrics-list-album active").
var candidateAlbumRe = regexp.MustCompile(`(?i)class=["'][^"']*\blyrics-list-album\b[^"']*["'][^>]*>([^<]+)<`)

// candidateSyncedRe detects a text_sync marker anywhere in a candidate block.
var candidateSyncedRe = regexp.MustCompile(`(?i)text_sync`)

// Client communicates with petitlyrics.com over its reverse-engineered
// endpoints.
type Client struct {
	httpClient *http.Client

	// baseURL is the host root; injectable so tests can target httptest.
	baseURL string

	// pacer fields -- zero value means no pacing (minInterval == 0).
	mu          sync.Mutex
	minInterval time.Duration
	lastRequest time.Time
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool
}

// NewClient creates a new Petit Lyrics client. A cookie jar is installed so the
// PLSESSION cookie set while fetching the CSRF token is carried into the
// subsequent AJAX request.
func NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{
		baseURL: defaultBaseURL,
		now:     time.Now,
		sleep:   ctxSleep,
	}
	c.httpClient = &http.Client{
		Timeout:       30 * time.Second,
		Jar:           jar,
		CheckRedirect: c.checkRedirect,
	}
	return c
}

// checkRedirect pins redirects to the configured base host. The default
// http.Client follows up to 10 redirects without restricting the target host,
// so a 3xx from petitlyrics.com could otherwise move a request to an arbitrary
// host (an SSRF vector). This rejects cross-host redirects and preserves the
// standard 10-hop cap.
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

// WithMinInterval sets the minimum duration between outbound requests and
// returns the receiver for chaining. A zero or negative value disables pacing
// (the default). Not goroutine-safe; call before sharing the client.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	c.minInterval = d
	return c
}

// MinInterval returns the configured minimum request interval. Zero means
// pacing is disabled.
func (c *Client) MinInterval() time.Duration {
	return c.minInterval
}

// pace enforces the minimum request interval, mirroring the musixmatch pacer.
// It is called before each outbound request (WithMinInterval is documented as a
// minimum between requests, not between lookups), so a single FindLyrics, which
// makes three calls, cannot burst. The wait is ctx-cancellable.
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

// do enforces pacing then executes req, wrapping a transport error with the
// stage label. Centralizing pacing here keeps WithMinInterval a per-request
// minimum: every outbound request in a lookup passes through do.
func (c *Client) do(ctx context.Context, req *http.Request, stage string) (*http.Response, error) {
	if err := c.pace(ctx); err != nil {
		return nil, err
	}
	res, err := c.httpClient.Do(req) //nolint:gosec // G704: the request host is c.baseURL (fixed petitlyrics.com const, test-only override) and the client's CheckRedirect pins redirects to that host, so a 3xx cannot move the request off-host; track inputs go in the form body, not the URL. No SSRF vector.
	if err != nil {
		return nil, fmt.Errorf("petitlyrics: %s: %w", stage, err)
	}
	return res, nil
}

// Name returns the provider name.
func (c *Client) Name() string {
	return providerName
}

// statusError maps a non-200 HTTP status to a sentinel error, or nil if the
// status is 200. 403/429 -> ErrRateLimited, 401 -> ErrUnauthorized.
func statusError(stage string, status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("petitlyrics: %s: %w", stage, ErrUnauthorized)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("petitlyrics: %s: HTTP %d: %w", stage, status, ErrRateLimited)
	default:
		return fmt.Errorf("petitlyrics: %s: unexpected HTTP status %d", stage, status)
	}
}

const maxResponseSize = 2 << 20 // 2 MiB

// readBody reads a capped response body.
func readBody(stage string, res *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("petitlyrics: %s: read body: %w", stage, err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("petitlyrics: %s: response too large (%d bytes)", stage, len(body))
	}
	return body, nil
}

// FindLyrics looks up lyrics for the given track from petitlyrics.com. It runs
// the three-stage reverse-engineered flow: search for a lyrics id, fetch the
// CSRF token, then request the lyrics payload via the AJAX endpoint.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	id, err := c.searchLyricsID(ctx, track)
	if err != nil {
		return models.Song{}, err
	}

	token, err := c.fetchCSRFToken(ctx)
	if err != nil {
		return models.Song{}, err
	}

	return c.fetchLyrics(ctx, id, token)
}

// searchLyricsID POSTs the search form and scrapes the first /lyrics/<id> link.
func (c *Client) searchLyricsID(ctx context.Context, track models.Track) (string, error) {
	form := url.Values{
		"title":  {track.TrackName},
		"artist": {track.ArtistName},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/search_lyrics", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("petitlyrics: search: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := c.do(ctx, req, "search")
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if err := statusError("search", res.StatusCode); err != nil {
		return "", err
	}

	body, err := readBody("search", res)
	if err != nil {
		return "", err
	}

	return selectCandidate(parseSearchCandidates(body), track.AlbumName)
}

// fetchCSRFToken fetches the static pl-lib.js file, scrapes the CSRF token, and
// (via the cookie jar) captures the PLSESSION cookie for the AJAX request.
func (c *Client) fetchCSRFToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/lib/pl-lib.js", nil)
	if err != nil {
		return "", fmt.Errorf("petitlyrics: csrf: build request: %w", err)
	}

	res, err := c.do(ctx, req, "csrf")
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if err := statusError("csrf", res.StatusCode); err != nil {
		return "", err
	}

	body, err := readBody("csrf", res)
	if err != nil {
		return "", err
	}

	m := csrfTokenRe.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("petitlyrics: csrf: token not found in pl-lib.js")
	}
	return string(m[1]), nil
}

// ajaxEntry is one element of the AJAX lyrics response array. Field names are
// inferred from observed reverse-engineered responses and may change.
type ajaxEntry struct {
	LyricsType int    `json:"lyrics_type"`
	Lyrics     string `json:"lyrics"`
}

// lyrics_type sentinels for the secondary (non-original) tracks.
//
// ASSUMPTION: lyrics_type values are UNVERIFIED. They are derived from synthetic
// fixtures, not from observed petitlyrics.com responses. The mapping chosen here
// is: the FIRST entry is always the original track (whatever its lyrics_type --
// in observed fixtures 1 = unsynced text, 2 = line-synced, 3 = word-synced).
// Among the remaining entries, lyrics_type == lyricsTypeTranslation (4) is the
// translation track and lyrics_type == lyricsTypeRomanization (5) is the
// romanization track. Any other secondary lyrics_type is ignored. Revisit this
// mapping once real multi-track responses are captured.
const (
	lyricsTypeTranslation  = 4
	lyricsTypeRomanization = 5
)

// searchCandidate holds one result from the /search_lyrics HTML response.
type searchCandidate struct {
	id     string // numeric id from /lyrics/<id>
	album  string // text of lyrics-list-album element; empty if absent
	synced bool   // true when text_sync marker is present in the item block
}

// normalizeAlbum lowercases, trims, and collapses internal whitespace so that
// "(Deluxe Edition)" suffixes and minor spacing differences do not prevent a
// prefix match.
func normalizeAlbum(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// albumMatches reports whether candidate matches query using a normalized
// prefix comparison. Exact match and edition-suffix variants (e.g. "Album
// Name (Deluxe Edition)" vs "Album Name") both return true. Substring-only
// matches (e.g. "Hits" inside "Greatest Hits") do not. An empty candidate
// or query never matches so that blank-album candidates fall through to the
// fallback-to-all path rather than polluting an album-narrowed working set.
func albumMatches(candidate, query string) bool {
	nc := normalizeAlbum(candidate)
	nq := normalizeAlbum(query)
	if nc == "" || nq == "" {
		return false
	}
	return strings.HasPrefix(nc, nq) || strings.HasPrefix(nq, nc)
}

// parseSearchCandidates extracts all lyrics candidates from the /search_lyrics
// HTML response. Each <li> block is inspected for a /lyrics/<id> link; blocks
// without one are skipped. Album and sync-type are extracted from the
// lyrics-list-album class element and text_sync marker respectively.
func parseSearchCandidates(body []byte) []searchCandidate {
	blocks := candidateBlockRe.FindAllSubmatch(body, -1)
	var candidates []searchCandidate
	for _, block := range blocks {
		inner := block[1]
		m := lyricsLinkRe.FindSubmatch(inner)
		if m == nil {
			continue
		}
		album := ""
		if am := candidateAlbumRe.FindSubmatch(inner); am != nil {
			album = strings.TrimSpace(string(am[1]))
		}
		candidates = append(candidates, searchCandidate{
			id:     string(m[1]),
			album:  album,
			synced: candidateSyncedRe.Match(inner),
		})
	}
	return candidates
}

// selectCandidate picks the best candidate id from the search results.
// When album is non-empty, candidates whose album field matches (via
// albumMatches) form a preferred working set; if no candidates match the
// album, all candidates are used (best-effort fallback). Within the working
// set, the first synced candidate wins; otherwise the first candidate is
// returned. An empty candidates slice returns ErrNotFound.
func selectCandidate(candidates []searchCandidate, album string) (string, error) {
	if len(candidates) == 0 {
		return "", fmt.Errorf("petitlyrics: search: no lyrics link found: %w", ErrNotFound)
	}
	working := candidates
	if album != "" {
		var matched []searchCandidate
		for _, c := range candidates {
			if albumMatches(c.album, album) {
				matched = append(matched, c)
			}
		}
		if len(matched) > 0 {
			working = matched
		}
	}
	for _, c := range working {
		if c.synced {
			return c.id, nil
		}
	}
	return working[0].id, nil
}

// trackFromEntry decodes one ajaxEntry's base64 payload into synced lines via
// parseLRC. It returns ok=false when the payload is empty, undecodable, or
// carries no parseable timestamps (a secondary track is only adopted when it is
// itself synced, matching the interleaved-output contract).
func trackFromEntry(e ajaxEntry) (models.Synced, bool) {
	if e.Lyrics == "" {
		return models.Synced{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(e.Lyrics)
	if err != nil {
		slog.Debug("petitlyrics: secondary track failed base64 decode; skipping", "lyrics_type", e.LyricsType)
		return models.Synced{}, false
	}
	lines, ok := parseLRC(string(decoded))
	if !ok {
		return models.Synced{}, false
	}
	return models.Synced{Lines: lines}, true
}

// fetchLyrics POSTs to the AJAX endpoint with the lyrics id and CSRF token,
// then base64-decodes the payload into synced or plain lyrics.
func (c *Client) fetchLyrics(ctx context.Context, id, token string) (models.Song, error) {
	song := models.Song{}

	form := url.Values{"lyrics_id": {id}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/com/get_lyrics.ajax", strings.NewReader(form.Encode()))
	if err != nil {
		return song, fmt.Errorf("petitlyrics: ajax: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", token)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	res, err := c.do(ctx, req, "ajax")
	if err != nil {
		return song, err
	}
	defer func() { _ = res.Body.Close() }()

	if err := statusError("ajax", res.StatusCode); err != nil {
		return song, err
	}

	body, err := readBody("ajax", res)
	if err != nil {
		return song, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return song, fmt.Errorf("petitlyrics: ajax: empty response body: %w", ErrNotFound)
	}

	var entries []ajaxEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return song, fmt.Errorf("petitlyrics: ajax: decode JSON: %w", err)
	}
	if len(entries) == 0 || entries[0].Lyrics == "" {
		return song, fmt.Errorf("petitlyrics: ajax: response carried no lyrics: %w", ErrNotFound)
	}

	decoded, err := base64.StdEncoding.DecodeString(entries[0].Lyrics)
	if err != nil {
		return song, fmt.Errorf("petitlyrics: ajax: base64 decode lyrics: %w", err)
	}
	text := string(decoded)

	if lines, ok := parseLRC(text); ok {
		song.Subtitles.Lines = lines
		// Only a synced original gets secondary tracks merged in: bilingual
		// interleaving (docs/multilingual-output-policy.md) needs the original to
		// be synced, and a plain-text original has no timestamps to share.
		applySecondaryTracks(&song, entries[1:])
		return song, nil
	}

	// No parseable timestamps: treat as plain lyrics.
	song.Lyrics.LyricsBody = text
	return song, nil
}

// applySecondaryTracks populates song.TranslationSubtitles and
// song.RomanizationSubtitles from the non-first AJAX entries, keyed by the
// (unverified) lyrics_type sentinels documented above. Entries with an
// unrecognized lyrics_type, or that fail to decode to synced lines, are ignored
// so an original-only response leaves the new fields empty.
func applySecondaryTracks(song *models.Song, rest []ajaxEntry) {
	for _, e := range rest {
		switch e.LyricsType {
		case lyricsTypeTranslation:
			if track, ok := trackFromEntry(e); ok && len(song.TranslationSubtitles.Lines) == 0 {
				song.TranslationSubtitles = track
			}
		case lyricsTypeRomanization:
			if track, ok := trackFromEntry(e); ok && len(song.RomanizationSubtitles.Lines) == 0 {
				song.RomanizationSubtitles = track
			}
		}
	}
}

// parseLRC parses LRC-formatted text into synced lines. It returns ok=false
// when no line carries a parseable [mm:ss.xx] timestamp, signaling the content
// should be treated as plain lyrics instead.
//
// Expansion happens here rather than at write time (#470): a compressed line
// carrying several leading stamps ([t1][t2]text) becomes one cue per stamp, so
// everything downstream of this lane holds a one-cue-per-line model and every
// future write is player-compatible. Simple player frontends read only the
// first timestamp on a line, so a stranded [t2] would otherwise render as
// literal lyric text and the repeat would never highlight.
func parseLRC(text string) ([]models.Lines, bool) {
	cues := lrcnormalize.ParseBody(text).Cues
	return cues, len(cues) > 0
}
