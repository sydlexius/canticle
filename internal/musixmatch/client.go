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

	"github.com/doxazo-net/canticle/internal/models"
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
	// subtitle_body is empty. This is typically observed during egress-IP
	// throttling, where the upstream returns a well-formed but hollow body.
	// Treat as a circuit-breaker signal rather than a parse error.
	ErrTruncatedResponse = errors.New("musixmatch: truncated or empty response body")
)

// redactToken wraps err so its message has every occurrence of the usertoken
// replaced with "REDACTED", while preserving the wrapped error for errors.Is/As
// (context cancellation, the circuit breaker's sentinel checks, *url.Error
// detection). It is applied to the request-build and transport error paths,
// which are the ones that echo the request URL -- and thus the usertoken query
// parameter -- into their message. Returns err unchanged when there is nothing
// to redact (nil error or empty token).
func (c *Client) redactToken(err error) error {
	if err == nil || c.Token == "" {
		return err
	}
	return &redactedTokenError{err: err, token: c.Token}
}

// redactedTokenError overrides only the rendered message, delegating identity
// (Unwrap) to the wrapped error so errors.Is/As are unaffected.
//
// Note: because Unwrap exposes the original error, errors.As(err, &urlErr) still
// yields a *url.Error whose .URL field holds the raw token. That is safe only so
// long as no caller logs urlErr.URL directly; every current consumer renders via
// Error() (the redacted string). A future caller that reads the struct field must
// redact it itself.
type redactedTokenError struct {
	err   error
	token string
}

func (e *redactedTokenError) Error() string {
	msg := strings.ReplaceAll(e.err.Error(), e.token, "REDACTED")
	// url.Values.Encode percent-escapes the token in the URL; cover the escaped
	// form too so a token containing reserved characters cannot slip through.
	if esc := url.QueryEscape(e.token); esc != e.token {
		msg = strings.ReplaceAll(msg, esc, "REDACTED")
	}
	return msg
}

func (e *redactedTokenError) Unwrap() error { return e.err }

// GoString satisfies fmt.GoStringer so the %#v verb -- which otherwise prints the
// struct's raw fields, including the token -- renders the redacted message instead.
// Error() already covers %v/%s/%+v (they use the error interface); %#v is the one
// verb that bypasses it.
func (e *redactedTokenError) GoString() string { return e.Error() }

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

// Client communicates with the Musixmatch desktop API.
type Client struct {
	Token      string
	httpClient *http.Client

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
// A zero or negative value disables pacing (the default). This method is not
// goroutine-safe; call it before sharing the client across goroutines.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	c.minInterval = d
	return c
}

// MinInterval returns the configured minimum request interval. A zero value
// means pacing is disabled.
func (c *Client) MinInterval() time.Duration {
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
	if c.minInterval <= 0 {
		return nil
	}

	c.mu.Lock()
	now := c.now()
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
// exact sawtooth this fixes).
func (c *Client) OnThrottle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adaptiveLevel < adaptiveMaxLevel {
		c.adaptiveLevel++
	}
	c.consecutiveSuccesses = 0
}

// OnSuccess implements the providers.AdaptivePacer interface. It records a
// successful fetch; once consecutiveSuccesses reaches adaptiveSuccessThreshold
// it steps the adaptive level down by one (floored at 0) and resets the
// counter, gradually easing the effective interval back toward the floor.
func (c *Client) OnSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveSuccesses++
	if c.consecutiveSuccesses >= adaptiveSuccessThreshold {
		if c.adaptiveLevel > 0 {
			c.adaptiveLevel--
		}
		c.consecutiveSuccesses = 0
	}
}

// Name returns the provider name.
func (c *Client) Name() string {
	return "musixmatch"
}

// FindLyrics looks up lyrics for the given track from the Musixmatch API.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	if err := c.pace(ctx); err != nil {
		return models.Song{}, err
	}
	song := models.Song{}
	baseURL, err := url.Parse(apiURL)
	if err != nil {
		return song, fmt.Errorf("failed to parse API URL: %w", err)
	}
	params := url.Values{
		"format":            {"json"},
		"namespace":         {"lyrics_richsynched"},
		"subtitle_format":   {"mxm"},
		"app_id":            {"web-desktop-app-v1.0"},
		"usertoken":         {c.Token},
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
		return song, c.redactToken(err)
	}

	req.Header = http.Header{
		"authority": {"apic-desktop.musixmatch.com"},
		"cookie":    {"x-mxm-token-guid="},
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure surfaces as a *url.Error whose message embeds the
		// full request URL -- including the usertoken query parameter. Returning
		// it raw would leak the secret into work_queue.last_error, the
		// failure-analysis UI, and logs, so redact it at this boundary.
		return song, c.redactToken(err)
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
