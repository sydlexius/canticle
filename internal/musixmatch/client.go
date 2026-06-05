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
	ErrNoLyrics = errors.New("musixmatch: no lyrics available")
)

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
}

// NewClient creates a new Musixmatch API client.
func NewClient(token string) *Client {
	return &Client{
		Token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Name returns the provider name.
func (c *Client) Name() string {
	return "musixmatch"
}

// FindLyrics looks up lyrics for the given track from the Musixmatch API.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
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
			return song, fmt.Errorf("%w: token may be invalid or expired", ErrUnauthorized)
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
		return song, fmt.Errorf("%w: token renewal required", ErrUnauthorized)
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
		return song, fmt.Errorf("%w: token may be invalid or expired", ErrUnauthorized)
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
		if err := json.Unmarshal(tsg.GetStringBytes("body", "subtitle_list", "0", "subtitle", "subtitle_body"), &song.Subtitles.Lines); err != nil {
			return song, err
		}
	} else {
		slog.Info("no synced lyrics found")
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
			slog.Info("song is instrumental")
		} else {
			return song, fmt.Errorf("%w: no synced or unsynced lyrics", ErrNoLyrics)
		}
	}
	return song, nil
}
