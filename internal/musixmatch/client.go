package musixmatch

import (
	"context"
	"encoding/json"
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

	"github.com/sydlexius/canticle/internal/models"
	"github.com/valyala/fastjson"
)

const apiURL = "https://apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get"

const (
	// adaptiveMaxLevel caps the adaptive ratcheting level. The effective request
	// interval is minInterval << adaptiveLevel, so a max level of 3 yields a
	// maximum multiplier of 1 << 3 = 8x the configured cooldown floor.
	adaptiveMaxLevel = 3
	// adaptiveSuccessThreshold is the number of consecutive successful fetches
	// required before the adaptive level steps down by one. Requiring a sustained
	// clean streak prevents premature recovery that would restart the sawtooth.
	adaptiveSuccessThreshold = 5
	// adaptiveDecayInterval is how long the pacer must go without a fresh
	// OnThrottle before it eases the adaptive level down by one step, evaluated
	// lazily on each pace() call (see decayLocked) rather than by a background
	// goroutine/ticker. It exists because adaptiveSuccessThreshold alone can
	// starve: measured production traffic settled 52 items in the window after
	// the last ratchet with only 2 reaching OnSuccess (most were benign misses,
	// which never reach the pacer), so the consecutive-streak path is
	// effectively unreachable and the level would otherwise be permanent until
	// restart (#492). 20 minutes is a deliberate middle ground: a full unwind
	// from adaptiveMaxLevel takes 3 * this (60 minutes), which is long enough
	// that a short lull between genuine 401s does not immediately erase the
	// ratchet and re-provoke the provider, but short enough to recover within a
	// single day of serve-mode operation even when catalog-hit luck is poor.
	adaptiveDecayInterval = 20 * time.Minute
)

// Sentinel errors returned by the Musixmatch client. Callers should use
// errors.Is to test for these classes rather than string-matching the message.
var (
	// ErrUnauthorized indicates HTTP 401 from the Musixmatch API. The token
	// may be invalid, expired, or (per observed behavior) the egress IP may
	// be throttled. Treat as a circuit-breaker signal.
	ErrUnauthorized = errors.New("musixmatch: unauthorized")
	// ErrRateLimited indicates HTTP 429 from the Musixmatch API. Treat as a
	// circuit-breaker signal.
	ErrRateLimited = errors.New("musixmatch: rate limited")
	// ErrNotFound indicates HTTP 404 or an inner status_code 404 from the
	// Musixmatch API meaning no matching track or lyrics were found.
	ErrNotFound = errors.New("musixmatch: no results found")
	// ErrNoLyrics indicates the track was matched but no usable lyrics could be
	// obtained: the catalog has no synced or plain lyrics, the lyrics are
	// restricted, or the response omitted the lyrics payload. Like ErrNotFound,
	// this is a benign miss (see IsBenignMiss): there are no fetchable lyrics
	// now and the upstream result is stable (it will not change on a near-term
	// retry), so callers must not count it as a fetch failure for backoff.
	//
	// Restricted tracks (licensing) are also classified here. Such restrictions
	// can be permanent, so a track wrapped as ErrNoLyrics may be re-checked on
	// the fixed benign-miss cooldown indefinitely; Defer never increments the
	// attempt count, so there is no natural ceiling. This is intentional:
	// catalogs and licensing change over time, and the days-scale cadence keeps
	// the cost negligible.
	ErrNoLyrics = errors.New("musixmatch: no lyrics available")
	// ErrTruncatedResponse indicates a structurally valid response (HTTP 200,
	// track present) whose inner data is missing -- e.g. has_subtitles=1 but the
	// subtitle_body is empty. This was originally observed during egress-IP
	// throttling, but detection here only proves the body was hollow for THIS
	// request; the callers (orchestrator, worker) classify it as a benign miss
	// rather than a throttle signal, since an empty body is deterministic per
	// request and not evidence of a transient rate limit (#496).
	ErrTruncatedResponse = errors.New("musixmatch: truncated or empty response body")
)

// transportError converts a request-build or transport failure into a clean,
// groupable error that carries neither the request URL nor the usertoken. Go
// wraps http.Client.Do and http.NewRequestWithContext failures in a *url.Error
// whose message embeds the full request URL -- which contains the usertoken and
// the per-track query params (q_artist/q_track/q_album). Storing that raw
// fragments the failure-analysis grouping (every URL is unique, so each failure
// becomes its own single-count reason) and writes secrets plus library metadata
// into work_queue.last_error. Unwrapping to the underlying cause yields a stable
// "musixmatch: transport error: <cause>" (e.g. connection refused, timeout) that
// aggregates across tracks, while %w preserves errors.Is/As so the worker still
// classifies context cancellation and the like. Dropping the URL entirely also
// removes any need to scrub the token from the message.
func transportError(err error) error {
	cause := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	return fmt.Errorf("musixmatch: transport error: %w", cause)
}

// tokenRenewalError marks the upstream "renew" hint: the usertoken must be
// regenerated. It satisfies errors.Is for BOTH itself and ErrUnauthorized, so
// the circuit breaker (which keys off ErrUnauthorized) still trips while callers
// that care can distinguish a definite renewal from an ambiguous bare 401.
type tokenRenewalError struct{}

func (tokenRenewalError) Error() string { return "musixmatch: token renewal required" }

func (tokenRenewalError) Is(target error) bool {
	return target == ErrUnauthorized || target == ErrTokenRenewalRequired
}

// ErrTokenRenewalRequired indicates the upstream explicitly signaled the token
// must be renewed (in-body status_code 401 with hint=renew). This is the one
// genuine "renew your token" case, distinct from a bare 401 (which is, per
// observed behavior, usually an egress-IP throttle). errors.Is reports true for
// both ErrTokenRenewalRequired and ErrUnauthorized, so the circuit still trips.
var ErrTokenRenewalRequired error = tokenRenewalError{}

// IsBenignMiss reports whether err represents a benign miss: the track has no
// fetchable lyrics now (either no match at all, or a match with no usable
// lyrics). These outcomes are not failures of the API or the network, and the
// upstream result is stable -- it will not change on a near-term retry. Callers
// (worker, app) use this to skip the geometric backoff and the immediate retry
// that genuine, transient failures warrant. (This concerns only the upstream
// result; the queue row is not retired -- the worker re-checks it later on a
// generous cooldown as the catalog grows.)
func IsBenignMiss(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoLyrics)
}

// TokenRenewer supplies a replacement token when the API explicitly signals that
// the current one is finished. It is deliberately narrow: the ONLY trigger is the
// body-level hint=renew signal (see ErrTokenRenewalRequired), never a bare HTTP
// 401, which observed behavior says is usually an egress/token throttle rather
// than a dead credential (#554).
type TokenRenewer interface {
	Renew(ctx context.Context) (string, error)
}

// Client communicates with the Musixmatch desktop API.
type Client struct {
	// Token is the initial token. Read it through currentToken(), which also sees
	// a token installed later by a renewal.
	Token      string
	httpClient *http.Client

	// tokenMu guards the renewed token. It is separate from mu (the pacer lock) so
	// a token read can never nest inside the pacing critical section.
	tokenMu sync.RWMutex
	// renewedToken, when non-empty, supersedes Token after a successful renewal.
	renewedToken string
	// renewMu single-flights the renewal path so concurrent hint=renew signals
	// produce ONE mint rather than one per goroutine. It is held across the mint
	// call, so it is deliberately NOT tokenMu (which must stay uncontended for
	// per-request token reads).
	renewMu sync.Mutex
	// renewer, when non-nil, mints a replacement token on the hint=renew signal.
	// Nil disables renewal entirely, which is the behavior for every caller that
	// supplies its own operator token.
	renewer TokenRenewer

	// pacer fields -- zero value means no pacing (minInterval == 0).
	mu          sync.Mutex
	minInterval time.Duration
	lastRequest time.Time
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool

	// Adaptive pacing state, guarded by mu. adaptiveLevel ratchets the effective
	// request interval: effectiveMultiplier = 1 << adaptiveLevel (so level 0 ==
	// 1x, the configured floor). It rises on throttle notifications and only
	// falls after a sustained success streak, so it persists across circuit
	// recovery cycles (the breaker's trip count is deliberately NOT used).
	adaptiveLevel        int
	consecutiveSuccesses int
	// lastLevelChange is the time adaptiveLevel last changed (up via OnThrottle,
	// or down via either the success streak or time decay). The zero value means
	// "never changed" -- decayLocked will not fire until a throttle has actually
	// occurred, since there is nothing to decay from and no meaningful elapsed
	// window to measure.
	lastLevelChange time.Time
}

// SetTokenRenewer installs the renewer consulted when the API signals
// hint=renew. A nil renewer leaves renewal disabled.
func (c *Client) SetTokenRenewer(r TokenRenewer) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.renewer = r
}

// currentToken returns the token to send: the renewed one when a renewal has
// succeeded, otherwise the token supplied at construction.
func (c *Client) currentToken() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	if c.renewedToken != "" {
		return c.renewedToken
	}
	return c.Token
}

// installRenewedToken records a freshly minted token for subsequent requests.
func (c *Client) installRenewedToken(tok string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.renewedToken = tok
}

// tokenRenewer returns the configured renewer, or nil.
func (c *Client) tokenRenewer() TokenRenewer {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.renewer
}

// NewClient creates a new Musixmatch API client.
func NewClient(token string) *Client {
	return &Client{
		Token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
		sleep:      ctxSleep,
	}
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

// WithMinInterval sets the minimum duration between outbound API requests.
// It returns c so callers can chain it on construction:
//
//	client := musixmatch.NewClient(token).WithMinInterval(15 * time.Second)
//
// A zero or negative value disables pacing (the default). The write is
// guarded by the same mutex pace() reads minInterval under (#494), so calling
// this concurrently with in-flight FindLyrics calls is race-free -- but it is
// still not a supported runtime reconfiguration knob: a concurrent caller may
// observe either the old or the new interval for any given pace() call, with
// no ordering guarantee beyond "no data race." Prefer setting it once before
// sharing the client across goroutines.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	c.mu.Lock()
	c.minInterval = d
	c.mu.Unlock()
	return c
}

// MinInterval returns the configured minimum request interval. A zero value
// means pacing is disabled. Guarded by the same mutex WithMinInterval writes
// under (#494).
func (c *Client) MinInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.minInterval
}

// pace enforces the minimum request interval. It must be called at the top of
// FindLyrics before the HTTP request is built. When minInterval is zero or
// negative it returns immediately. Otherwise, under the lock, it computes the
// next free slot from lastRequest and the adaptive interval, advances
// lastRequest to that slot (reserving it before releasing the lock), then
// sleeps outside the lock for whatever wait remains. Reserving the slot under
// the lock is what prevents convoying: N concurrent callers each claim a
// distinct, sequential slot rather than all reading the same lastRequest,
// computing the same wait, and bursting together when their sleeps elapse.
//
// The wait is ctx-cancellable; if the context is canceled during the wait
// pace returns ctx.Err() wrapped with context. A canceled caller releases its
// reserved slot best-effort (see the rollback below) so it does not push every
// later caller back one interval.
func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	// minInterval is read under the lock (#494): it is written by
	// WithMinInterval, which also takes c.mu, so reading it before Lock (as this
	// used to) was a bare data race against any concurrent setter, independent
	// of the adaptive-state fields below.
	if c.minInterval <= 0 {
		c.mu.Unlock()
		return nil
	}
	now := c.now()
	// Lazily evaluate time-based ratchet-down before reading the level: this is
	// the sole checkpoint (no goroutine/ticker), so it must run on every pace()
	// call, under the same lock that guards adaptiveLevel.
	c.decayLocked(now)
	// Adaptive interval: minInterval scaled by the current ratcheting level.
	// minInterval is the floor (level 0 == 1x), keeping api.cooldown as the
	// explicit override the operator configured.
	adaptiveLevel := c.adaptiveLevel
	effectiveMultiplier := 1 << adaptiveLevel
	baseInterval := c.minInterval
	effectiveInterval := baseInterval * time.Duration(effectiveMultiplier)
	// Reserve this caller's slot under the lock. The earliest the next request
	// may proceed is one effective interval after the previously reserved slot;
	// if that is already in the past, the slot is now. Advancing lastRequest to
	// the reserved slot means the next caller computes its own later slot, so
	// concurrent callers serialize instead of all sleeping the same wait.
	prev := c.lastRequest
	next := prev.Add(effectiveInterval)
	if next.Before(now) {
		next = now
	}
	c.lastRequest = next
	wait := next.Sub(now)
	c.mu.Unlock()

	if adaptiveLevel > 0 {
		slog.Debug("musixmatch pacer: adaptive interval in effect",
			"level", adaptiveLevel, "multiplier", effectiveMultiplier,
			"effective_interval", effectiveInterval, "base_interval", baseInterval)
	}

	if wait > 0 {
		slog.Debug("musixmatch pacer: waiting before next request", "wait", wait)
		if !c.sleep(ctx, wait) {
			// The wait was canceled before this caller ever used its slot.
			// Release the reservation best-effort: only if lastRequest is still
			// exactly the slot we reserved (no later caller has reserved past
			// us) do we roll it back to the value we reserved from. If a later
			// caller already advanced lastRequest, leave it alone rather than
			// stomp a newer reservation. Use Equal for the time comparison.
			c.mu.Lock()
			if c.lastRequest.Equal(next) {
				c.lastRequest = prev
			}
			c.mu.Unlock()
			return fmt.Errorf("musixmatch: pace: %w", ctx.Err())
		}
	}
	return nil
}

// OnThrottle implements the providers.AdaptivePacer interface. It raises the
// adaptive ratcheting level by one (capped at adaptiveMaxLevel), increasing the
// effective request interval, and resets the consecutive-success counter. It
// takes no parameter: the level is maintained independently of the circuit
// breaker's trip count so it persists across circuit recovery cycles (using the
// breaker's trips would snap the multiplier back to 1x on every recovery, the
// exact sawtooth this fixes). It always resets the decay clock to now, so a
// fresh throttle re-ratchets immediately and time decay cannot fire again until
// a full adaptiveDecayInterval of throttle-free time has passed since THIS
// event -- the provider gets the full benefit of the doubt period after every
// genuine throttle signal.
func (c *Client) OnThrottle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adaptiveLevel < adaptiveMaxLevel {
		c.adaptiveLevel++
	}
	c.consecutiveSuccesses = 0
	c.lastLevelChange = c.now()
}

// OnSuccess implements the providers.AdaptivePacer interface. It records a
// successful fetch; once consecutiveSuccesses reaches adaptiveSuccessThreshold
// it steps the adaptive level down by one (floored at 0) and resets the
// counter, gradually easing the effective interval back toward the floor. This
// streak-based path and decayLocked's time-based path are independent triggers
// for the same step-down action; either one firing resets the decay clock so
// the two do not double-count the same window.
func (c *Client) OnSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveSuccesses++
	if c.consecutiveSuccesses >= adaptiveSuccessThreshold {
		if c.adaptiveLevel > 0 {
			c.adaptiveLevel--
			c.lastLevelChange = c.now()
		}
		c.consecutiveSuccesses = 0
	}
}

// decayLocked steps the adaptive level down by one for each full
// adaptiveDecayInterval that has elapsed, throttle-free, since lastLevelChange.
// Called under c.mu from pace() -- the natural per-request checkpoint -- so no
// goroutine or lifecycle is needed to drive it. Looping (rather than a single
// step) bounds the catch-up to however much wall-clock time has genuinely
// elapsed, e.g. after an idle period with no requests at all, while never
// stepping down faster than one level per adaptiveDecayInterval. It does not
// consult minInterval: the floor guarantee comes from level 0 already being the
// operator's configured minInterval (1x multiplier) -- adaptiveLevel cannot go
// negative, so decay can never make the effective interval faster than that
// floor. OnThrottle must still win a race with decay: it is the only path that
// raises the level, and it always runs to completion under the same lock decay
// runs under, so worst case is one extra request slips through at the eased
// interval before the next throttle re-ratchets (an intentional, bounded cost
// per the design constraints, not a bug).
func (c *Client) decayLocked(now time.Time) {
	for c.adaptiveLevel > 0 && !c.lastLevelChange.IsZero() && now.Sub(c.lastLevelChange) >= adaptiveDecayInterval {
		c.adaptiveLevel--
		c.consecutiveSuccesses = 0
		c.lastLevelChange = c.lastLevelChange.Add(adaptiveDecayInterval)
	}
}

// Name returns the provider name.
func (c *Client) Name() string {
	return "musixmatch"
}

// FindLyrics looks up lyrics for the given track from the Musixmatch API.
//
// When the API answers with the explicit hint=renew signal and a renewer is
// installed, it mints a replacement token, installs it, and retries the request
// EXACTLY ONCE (#554). The retry is deliberately bounded: a renewal that fails,
// is refused, or is followed by a second renewal signal returns the error rather
// than looping, because the mint endpoint is itself rate-limited and a retry loop
// would lock the deployment out of it entirely.
//
// A bare HTTP 401 never triggers renewal. errors.Is(err, ErrTokenRenewalRequired)
// matches only the body-level hint=renew case; the bare 401 path returns
// ErrUnauthorized, which observed behavior attributes to throttling rather than a
// dead credential.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	song, err := c.findLyricsOnce(ctx, track)
	if err == nil || !errors.Is(err, ErrTokenRenewalRequired) {
		return song, err
	}
	renewer := c.tokenRenewer()
	if renewer == nil {
		return song, err
	}

	// Single-flight the renewal. FindLyrics may be called concurrently, and the
	// mint endpoint is rate limited (three rapid mints from one egress were
	// measured to all return 401), so N goroutines seeing hint=renew together must
	// produce ONE mint, not N. Without this the feature triggers the exact lockout
	// it exists to avoid.
	staleToken := c.currentToken()
	c.renewMu.Lock()
	if c.currentToken() != staleToken {
		// Another goroutine renewed while this one waited. Use its token rather
		// than minting a second time.
		c.renewMu.Unlock()
		slog.Debug("musixmatch token was renewed concurrently; retrying with the existing replacement")
		return c.findLyricsOnce(ctx, track)
	}
	tok, rerr := renewer.Renew(ctx)
	if rerr != nil {
		c.renewMu.Unlock()
		slog.Warn("musixmatch signaled token renewal but minting a replacement failed",
			"error", rerr)
		return song, err
	}
	c.installRenewedToken(tok)
	c.renewMu.Unlock()
	slog.Info("musixmatch signaled token renewal; installed a freshly minted token and retrying once")
	return c.findLyricsOnce(ctx, track)
}

// findLyricsOnce performs a single lookup with the currently installed token.
func (c *Client) findLyricsOnce(ctx context.Context, track models.Track) (models.Song, error) {
	if err := c.pace(ctx); err != nil {
		return models.Song{}, err
	}
	song := models.Song{}
	baseURL, err := url.Parse(apiURL)
	if err != nil {
		return song, fmt.Errorf("failed to parse API URL: %w", err)
	}
	params := url.Values{
		"format": {"json"},
		// This namespace does NOT make macro.subtitles.get return word-level
		// (richsync) timing. Probed live 2026-07-22 over 12 mainstream tracks:
		// every response carried exactly five macro calls -- matcher.track.get,
		// track.lyrics.get, track.snippet.get, track.subtitles.get,
		// userblob.get -- and never a richsync one. So the parser below is not
		// silently dropping word-level data on THIS endpoint; none arrives here.
		//
		// Word-level timing IS available on this token, from a DIFFERENT
		// endpoint: track.richsync.get, keyed by the commontrack_id that
		// matcher.track.get returns. Measured the same day, 6 of 9 matched
		// mainstream tracks had a richsync body (schema {l,te,ts,x}; absolute
		// word time = ts + o). It is a two-step flow, which is why no
		// single-request probe of this endpoint could ever find it.
		//
		// Both halves are recorded here because the first, alone, invites the
		// false conclusion that word-level timing is unreachable from
		// Musixmatch. It is not -- it is reachable, just not from here.
		// The parameter is kept because the request shape is otherwise
		// unchanged and untested to remove.
		"namespace":         {"lyrics_richsynched"},
		"subtitle_format":   {"mxm"},
		"app_id":            {"web-desktop-app-v1.0"},
		"usertoken":         {c.currentToken()},
		"q_album":           {track.AlbumName},
		"q_artist":          {track.ArtistName},
		"q_artists":         {track.ArtistName},
		"q_track":           {track.TrackName},
		"track_spotify_id":  {track.SpotifyID},
		"q_duration":        {""},
		"f_subtitle_length": {""},
	}
	// Recording-level disambiguators, sent only when present so the normal scan
	// path (which leaves these empty) keeps its existing request shape. q_duration
	// and track_spotify_id reuse their existing slots; track_isrc is added only
	// when supplied since it is not otherwise part of the request.
	if track.TrackLength > 0 {
		params.Set("q_duration", strconv.Itoa(track.TrackLength))
	}
	if track.ISRC != "" {
		params.Set("track_isrc", track.ISRC)
	}
	baseURL.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL.String(), nil)
	if err != nil {
		return song, transportError(err)
	}

	req.Header = http.Header{
		"authority": {"apic-desktop.musixmatch.com"},
		"cookie":    {"x-mxm-token-guid="},
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure surfaces as a *url.Error whose message embeds the
		// full request URL -- the usertoken and the per-track query params.
		// transportError strips the URL down to the underlying cause so the
		// stored reason is clean, groupable, and free of secrets/metadata.
		return song, transportError(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		switch res.StatusCode {
		case http.StatusUnauthorized:
			return song, fmt.Errorf("%w: HTTP 401 (token rejected or, per observed behavior, egress IP throttled)", ErrUnauthorized)
		case http.StatusTooManyRequests:
			return song, fmt.Errorf("%w: increase the cooldown time and try again in a few minutes", ErrRateLimited)
		case http.StatusNotFound:
			return song, ErrNotFound
		default:
			errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
			return song, fmt.Errorf("musixmatch API error: status %d, body: %s", res.StatusCode, strings.TrimSpace(string(errBody)))
		}
	}

	const maxResponseSize = 2 << 20 // 2 MiB
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return song, err
	}
	if len(body) > maxResponseSize {
		return song, fmt.Errorf("musixmatch API response too large (%d bytes)", len(body))
	}

	var p fastjson.Parser
	v, err := p.Parse(string(body))
	if err != nil {
		return song, err
	}

	if v.GetInt("message", "header", "status_code") == 401 && string(v.GetStringBytes("message", "header", "hint")) == "renew" {
		return song, fmt.Errorf("%w: token renewal required", ErrTokenRenewalRequired)
	}

	mtg := v.Get("message", "body", "macro_calls", "matcher.track.get", "message")
	tlg := v.Get("message", "body", "macro_calls", "track.lyrics.get", "message")
	tsg := v.Get("message", "body", "macro_calls", "track.subtitles.get", "message")

	switch mtg.GetInt("header", "status_code") {
	case 200:
		trackNode := mtg.Get("body", "track")
		if trackNode == nil {
			// status_code 200 with no track body is an unexpected upstream shape,
			// not a benign miss -- intentionally returned as a genuine/transient
			// error (IsBenignMiss is false) so it retries rather than deferring.
			return song, errors.New("musixmatch: matcher status_code 200 but response missing track data")
		}
		if err := json.Unmarshal(trackNode.MarshalTo(nil), &song.Track); err != nil {
			return song, err
		}
	case 401:
		return song, fmt.Errorf("%w: HTTP 401 (token rejected or, per observed behavior, egress IP throttled)", ErrUnauthorized)
	case 404:
		return song, ErrNotFound
	default:
		// An unexpected matcher status_code is a genuine/transient upstream
		// condition, not a benign miss -- intentionally returned non-sentinel
		// (IsBenignMiss is false) so it is retried, and it carries the observed
		// code for diagnosis.
		return song, fmt.Errorf("musixmatch: unexpected matcher status_code %d", mtg.GetInt("header", "status_code"))
	}

	if song.Track.HasSubtitles == 1 {
		subBody := tsg.GetStringBytes("body", "subtitle_list", "0", "subtitle", "subtitle_body")
		if len(subBody) == 0 {
			return song, fmt.Errorf("%w: subtitle_body empty despite HasSubtitles=1", ErrTruncatedResponse)
		}
		if err := json.Unmarshal(subBody, &song.Subtitles.Lines); err != nil {
			return song, err
		}
	} else {
		slog.Debug("no synced lyrics found")
		if song.Track.HasLyrics == 1 {
			if tlg.GetInt("body", "lyrics", "restricted") == 1 {
				return song, fmt.Errorf("%w: restricted", ErrNoLyrics)
			}
			lyricsNode := tlg.Get("body", "lyrics")
			if lyricsNode == nil {
				return song, fmt.Errorf("%w: response missing lyrics data", ErrNoLyrics)
			}
			if err := json.Unmarshal(lyricsNode.MarshalTo(nil), &song.Lyrics); err != nil {
				return song, err
			}
		} else if song.Track.Instrumental == 1 {
			slog.Debug("song is instrumental")
		} else {
			return song, fmt.Errorf("%w: no synced or unsynced lyrics", ErrNoLyrics)
		}
	}
	return song, nil
}
