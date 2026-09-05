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

	// knownGood is the most recent track this credential successfully fetched at
	// least one song for, and it is what makes the outage decision EVIDENCE-BASED
	// rather than a guess (#767).
	//
	// A consecutive-miss count cannot distinguish "the provider has nothing for
	// THIS material" from "the provider has nothing for ANYTHING", and those need
	// opposite responses. A fallback lane sees exactly the obscure material the
	// primary already failed on, so a long miss run is its NORMAL operating
	// condition: measured at a 91% miss rate, a run of 20 had probability 0.742 of
	// occurring by chance. The threshold was firing on the population it was meant
	// to serve.
	//
	// Re-fetching a track the provider demonstrably had turns that into a real
	// test: a hit proves the credential works and the misses were material, a miss
	// proves the credential stopped working. Self-calibrating, because the control
	// comes from this provider's own past behavior rather than a hardcoded canary
	// that could itself be retired from the catalog.
	knownGood    models.Track
	hasKnownGood bool
	// probeInFlight stops a burst of concurrent misses from each launching their
	// own liveness probe. One probe answers the question for all of them.
	probeInFlight bool
}

// recordZeroResult counts a zero-song response and reports whether the run has
// reached the escalation threshold. Called for a response that parsed cleanly and
// simply carried no songs -- never for a transport or status failure, which say
// nothing about whether the application id still works.
//
// It DOES NOT LOG. Reaching the threshold is only SUSPICION under #767, not a
// finding: the probe in confirmOutage is what decides. An earlier revision warned
// here, which meant a perfectly healthy credential still logged "the application
// id may have been revoked" on every long run of obscure material -- the #767
// false positive surviving in the log path after being removed from the error
// path. Only reportConfirmedOutage logs, and only on evidence.
func (c *Client) recordZeroResult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveZero++
	return c.consecutiveZero >= ZeroResultThreshold
}

// reportConfirmedOutage latches and logs an outage the caller has CONFIRMED,
// either by a failed liveness probe or by the no-control count fallback.
//
// The latch makes this log once per outage rather than on every request past the
// threshold, and the emission sits outside the critical section: slog handlers
// can block on I/O and take locks of their own, and this mutex also paces every
// outbound request, so logging under it would serialize concurrent lookups at
// exactly the moment an outage fires. pace() follows the same shape.
func (c *Client) reportConfirmedOutage(probed bool) {
	c.mu.Lock()
	count := c.consecutiveZero
	first := !c.zeroReported
	c.zeroReported = true
	c.mu.Unlock()

	if first {
		slog.Warn("petitlyrics: provider returned no results for a sustained run and a liveness check did not clear it; the application id may have been revoked",
			"consecutive", count, "threshold", ZeroResultThreshold, "liveness_probed", probed)
	}
}

// recordSuccess clears the outage run and records the track as the control for a
// later liveness probe, IN ONE mutex-held transition (#767).
//
// Atomicity is load-bearing, not tidiness. These were two separate locked calls,
// and CodeRabbit caught the race: between them a concurrent burst of misses could
// reach the threshold, observe hasKnownGood == false, take the no-control path,
// and report ErrProviderUnavailable on a credential that had JUST returned songs.
// The failure mode was the exact false outage this change exists to prevent.
//
// A track with no artist or title is not stored as a control -- it would be
// unfetchable as a probe -- but it still clears the run, because songs came back.
//
// The recovery log is emitted after the unlock, for the same reason as above. The
// control track itself is NEVER logged: it is library metadata.
func (c *Client) recordSuccess(track models.Track) {
	usable := track.TrackName != "" && track.ArtistName != ""

	c.mu.Lock()
	recovered := c.zeroReported
	after := c.consecutiveZero
	c.zeroReported = false
	c.consecutiveZero = 0
	if usable {
		c.knownGood = track
		c.hasKnownGood = true
	}
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

// lookupUnsyncedText fetches the plain lyric text for a track at tier 1.
//
// It exists for the line-sync path: an LSY payload carries timings but no
// usable words, so a tier-2 result is a JOIN of two responses. Routed through
// c.request so the second call is paced like any other, and so a credential or
// throttle failure surfaces as the same typed sentinel rather than a bespoke
// error this lane's classifier would not recognize.
//
// A tier-1 request that answers with a non-text payload is refused rather than
// passed to the zipper: pairing timings with bytes that are not the words is a
// route to confident-looking wrong output, which is the failure this whole path
// is written to avoid.
//
// PINNED TO lyricsID, and that is the load-bearing part. An earlier version
// re-ran selectCandidate against the tier-1 response, which scores on ISRC,
// duration, and title text and never consults the lyrics id. The second request
// could therefore settle on a DIFFERENT recording than the one that supplied
// the timings -- a live cut, a remaster, a cover. When that other recording
// happens to carry the same line count, zipLineSync sees a well-formed pair and
// emits an .lrc whose every cue belongs to a different performance. Nothing
// downstream can detect it. Selecting the exact id the tier-2 payload came from
// makes the two responses provably the same song, and its absence a typed miss.
func (c *Client) lookupUnsyncedText(ctx context.Context, track models.Track, lyricsID string) (string, error) {
	songs, err := c.request(ctx, track, tierUnsynced)
	if err != nil {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup: %w", err)
	}
	candidate, err := selectByLyricsID(songs, lyricsID)
	if err != nil {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup, no candidate matched: %w", err)
	}
	if candidate.LyricsData == "" {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup returned no payload: %w", ErrNotFound)
	}
	raw, err := base64.StdEncoding.DecodeString(candidate.LyricsData)
	if err != nil {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup, base64 decode: %w", err)
	}
	if got := classifyPayload(raw); got != tierUnsynced {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup returned tier %d, want plain text: %w",
			got, ErrNotFound)
	}
	text := decodeUnsynced(raw)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("petitlyrics: line-sync text lookup returned empty text: %w", ErrNotFound)
	}
	return text, nil
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
		// and models.MsToTime is monotone, making Expand's stable sort a no-op on this
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
		// Tier 2 carries timings ONLY, so a line-synced result costs a second
		// request for the words. That request goes through c.request, which
		// paces itself, rather than issuing a raw POST that would bypass the
		// configured floor (#535).
		timings, err := decodeLineSyncTimings(raw)
		if err != nil {
			return models.Song{}, err
		}
		text, err := c.lookupUnsyncedText(ctx, track, candidate.LyricsID)
		if err != nil {
			return models.Song{}, err
		}
		cues, err := zipLineSync(timings, text)
		if err != nil {
			return models.Song{}, err
		}
		// Same normalizer as every other write path (#470), so this lane holds
		// the one-cue-per-line model too. No word timings exist at this tier, so
		// unlike the word-sync branch there is no index-stability concern.
		song.Subtitles = lrcnormalize.Expand(models.Synced{Lines: cues})
		return song, nil

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

// request performs the single form POST, decodes the XML envelope, and maintains
// the zero-result outage state (#607/#767).
//
// It wraps rawRequest so the liveness probe can reach the API WITHOUT feeding the
// counter it exists to adjudicate: a probe that counted its own result would
// either clear the suspicion it was sent to test, or deepen it, depending on
// ordering. The probe calls rawRequest directly.
func (c *Client) request(ctx context.Context, track models.Track, tier int) ([]apiSong, error) {
	songs, err := c.rawRequest(ctx, track, tier)
	if err != nil {
		return nil, err
	}
	if len(songs) == 0 {
		// A clean HTTP 200 carrying no songs. Individually this is an ordinary
		// miss; sustained, it MIGHT be a revoked application id, since the service
		// keeps answering normally rather than returning 401 (#607).
		//
		// "Might" is the whole correction in #767. A miss run is also what an
		// obscure-material population looks like, so the count alone cannot decide.
		// When the run reaches the threshold, ASK the provider a question it can
		// only answer one way.
		if !c.recordZeroResult() {
			return nil, fmt.Errorf("petitlyrics: no songs in response: %w", ErrNotFound)
		}
		if c.confirmOutage(ctx) {
			return nil, ErrProviderUnavailable
		}
		return nil, fmt.Errorf("petitlyrics: no songs in response: %w", ErrNotFound)
	}
	// At least one song came back, which proves the application id is still
	// accepted. Reset regardless of what the client then makes of the payload: a
	// candidate that loses selection, or a tier this client cannot decode, is a
	// per-track outcome and says nothing about credential health. The same
	// transition records the control track, atomically -- see recordSuccess.
	c.recordSuccess(track)
	return songs, nil
}

// confirmOutage decides whether a threshold-length miss run is a real provider
// outage, by re-fetching a track this credential previously succeeded on (#767).
//
// Returns true on POSITIVE evidence of an outage: the control track, which the
// provider served before, now returns nothing. A hit means the credential works
// and the misses were about the material, so the counter resets and the caller
// reports an ordinary miss.
//
// WITH NO CONTROL IT FALLS BACK TO THE COUNT, which preserves #607. That case is
// not a detail -- it is the ORIGINAL bug: a credential revoked before this
// process ever saw a hit (a restart with a dead clientAppId) never earns a
// control, so a probe-only design would leave that outage undetected FOREVER,
// strictly worse than counting. An earlier draft of this function returned false
// there and its comment claimed the next probe would catch it; there is no next
// probe when no control can ever be recorded.
//
// The two cases separate cleanly, which is what makes this sound rather than a
// compromise:
//
//   - A false positive (#767) happens on a lane that IS serving hits -- 6
//     successful fetches preceded the run that misfired -- so a control exists
//     and the probe adjudicates it.
//   - A cold-start outage (#607) has no hits by definition, so no control exists
//     and the count is the only signal available.
//
// A probe already in flight returns false: another goroutine is asking the same
// question, and two probes cannot be more informative than one.
func (c *Client) confirmOutage(ctx context.Context) bool {
	c.mu.Lock()
	control, have, busy := c.knownGood, c.hasKnownGood, c.probeInFlight
	if have && !busy {
		c.probeInFlight = true
	}
	c.mu.Unlock()

	if !have {
		// Never had a hit on this credential, so there is nothing to test against
		// and no way to do better than the count. This is the cold-start revoked
		// credential #607 exists to catch.
		c.reportConfirmedOutage(false)
		return true
	}
	if busy {
		// Another goroutine is already probing. Do not claim an outage on no
		// evidence; the in-flight probe will decide for the run.
		return false
	}
	defer func() {
		c.mu.Lock()
		c.probeInFlight = false
		c.mu.Unlock()
	}()

	// Deliberately rawRequest: the probe must not feed the counter it adjudicates.
	// Tier 1 because every song with lyrics has it -- a tier-2/3 probe could miss
	// on a healthy credential simply because that track has no higher tier.
	songs, err := c.rawRequest(ctx, control, tierUnsynced)
	if err != nil {
		// A transport or status failure says nothing about the credential; it is
		// the ordinary network-flake case, and statusError already classifies a
		// real 401/429.
		//
		// BACK THE COUNTER OFF rather than leaving it at the threshold. Otherwise
		// recordZeroResult keeps returning true and EVERY subsequent miss launches
		// another probe -- a retry storm of real paced requests during exactly the
		// transient failure that caused it. Halving costs one more threshold's
		// worth of misses before the next attempt, which is the right trade
		// against hammering a provider that is already struggling.
		c.backOffProbe()
		slog.Debug("petitlyrics: liveness probe failed in transport; not treating as an outage", "error", err)
		return false
	}
	if len(songs) > 0 {
		// The credential works. The miss run was about the MATERIAL, which is the
		// normal condition for a fallback lane. Clear it so the next run gets a
		// fresh threshold rather than escalating on the very next miss.
		//
		// The control is re-recorded here deliberately: it just proved itself
		// again, and passing it keeps the reset and the store atomic.
		c.recordSuccess(control)
		slog.Debug("petitlyrics: liveness probe succeeded; sustained miss run is material, not a credential outage")
		return false
	}
	// The provider served this track before and does not now. That is evidence.
	c.reportConfirmedOutage(true)
	return true
}

// backOffProbe halves the miss run after a probe that could not reach the API, so
// the next attempt is a threshold away instead of on the very next miss (#767
// review).
//
// Halving rather than zeroing: the misses were real, and discarding them entirely
// would let a genuine outage hide behind repeated probe failures -- a credential
// revoked at the same moment the network degrades would need two full runs to be
// noticed. Halving keeps the evidence while spacing the retries.
func (c *Client) backOffProbe() {
	c.mu.Lock()
	c.consecutiveZero /= 2
	c.mu.Unlock()
}

// rawRequest performs the single form POST and decodes the XML envelope, without
// touching the outage counter. Callers that represent real lookup traffic should
// use request instead.
func (c *Client) rawRequest(ctx context.Context, track models.Track, tier int) ([]apiSong, error) {
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
	// Zero-song handling and the outage counter live in request, the wrapper, so
	// the liveness probe can call this without adjudicating itself (#767). An
	// empty slice is returned as-is; the caller decides what it means.
	return parsed.Songs, nil
}
