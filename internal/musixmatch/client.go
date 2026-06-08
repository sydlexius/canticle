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
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/valyala/fastjson"
)

const apiURL = "https://apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get"

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
// negative it returns immediately. Otherwise it loops: under the lock it
// checks how long remains until the next slot is free; if the slot is free it
// reserves it (sets lastRequest = now) and returns; if not, it releases the
// lock, sleeps for the remainder, and re-checks. Re-checking after each sleep
// prevents concurrent callers from computing the same wait, sleeping together,
// and then all proceeding in a burst.
//
// The wait is ctx-cancellable; if the context is canceled during the wait
// pace returns ctx.Err() wrapped with context.
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

		slog.Debug("musixmatch pacer: waiting before next request", "wait", wait)
		if !c.sleep(ctx, wait) {
			return fmt.Errorf("musixmatch: pace: %w", ctx.Err())
		}
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
		"track_spotify_id":  {""},
		"q_duration":        {""},
		"f_subtitle_length": {""},
	}
	baseURL.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL.String(), nil)
	if err != nil {
		return song, err
	}

	req.Header = http.Header{
		"authority": {"apic-desktop.musixmatch.com"},
		"cookie":    {"x-mxm-token-guid="},
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return song, err
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
