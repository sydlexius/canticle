package musixmatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/valyala/fastjson"
)

// clientIdentity pairs the API host with the app_id that identifies canticle to
// it. A token is valid only for the client identity it was minted for (#934),
// so the host and app_id MUST change together -- splitting them is a bug. This
// is the sole such pair in the package: token.go's tokenURL() and client.go's
// apiURL()/app_id param/authority header all derive from currentClientIdentity
// below, so a future swap (the next identity retirement) is a one-line edit
// here rather than N call sites that can drift out of sync.
//
// The Musixmatch DESKTOP client identity (host apic-desktop.musixmatch.com,
// app_id web-desktop-app-v1.0) was retired upstream some time before
// 2026-09-10: token.get began returning a 56-zero user_token, and every
// macro.subtitles.get made with it received one fixed decoy payload regardless
// of query (#914, #934). The ANDROID client identity below was validated live
// the same day: distinct, matching results for real queries and a clean 404 for
// a nonsense one.
type clientIdentity struct {
	host  string
	appID string
}

// tokenURL returns the unauthenticated endpoint that issues a user token for
// this client identity.
func (c clientIdentity) tokenURL() string {
	return "https://" + c.host + "/ws/1.1/token.get"
}

// apiURL returns the lyrics endpoint for this client identity.
func (c clientIdentity) apiURL() string {
	return "https://" + c.host + "/ws/1.1/macro.subtitles.get"
}

// currentClientIdentity is the ONE client identity canticle currently
// impersonates. Both token.go and client.go read it; there is no fallback
// chain to other identities (tokens are per-client-id and the mint endpoint is
// rate-limited per IP, so probing multiple identities multiplies mint traffic
// for no benefit) -- a future retirement is handled by changing this value, not
// by adding options.
var currentClientIdentity = clientIdentity{
	host:  "apic.musixmatch.com",
	appID: "android-player-v1.0",
}

// maxTokenBodyBytes caps the token response read. The body is a single small
// JSON object; anything larger is a wrong endpoint or an error page.
const maxTokenBodyBytes = 64 << 10

// ErrTokenMintRefused reports that the token endpoint declined to issue a token,
// which in practice means rate limiting: #554 measured three rapid successive
// mints from one egress all returning 401.
//
// Callers MUST treat this as "back off and keep whatever token you already
// have", never as a reason to retry immediately. A restart loop that re-mints
// would lock itself out of the bootstrap endpoint entirely.
var ErrTokenMintRefused = errors.New("musixmatch: token mint refused (rate limited)")

// ErrClientIdentityRetired reports that the token endpoint accepted the
// request (HTTP 200, inner status_code 200) but issued a DEGENERATE token: an
// empty string, or a string made of one character repeated (the desktop
// identity's retirement produced exactly this shape -- 56 zeros -- per #934).
// This is deliberately a DISTINCT sentinel from a generic mint failure: a
// degenerate token is not a transient hiccup like rate limiting, it is the
// upstream telling us, in a shape it never documented, that the client
// identity we are impersonating no longer works. Making that LOUD here (a
// named error, not a bare "carried no usable token") is what lets the next
// occurrence be diagnosed in minutes rather than the days #914/#934 took: grep
// for this sentinel's message and the retired identity is the first thing a
// reader sees, before they have to reconstruct the same live-traffic
// investigation from scratch.
//
// It must NEVER be persisted -- see Mint below, which returns it before the
// caller ever sees a token to store.
var ErrClientIdentityRetired = errors.New("musixmatch: client identity appears retired upstream (token endpoint returned a degenerate token)")

// isDegenerateToken reports whether tok is empty or made of a single character
// repeated -- the shape #934 measured from a retired client identity (56
// zeros). A degenerate token is syntactically well-formed (right length,
// right character set) so nothing downstream would reject it on its own; the
// defect only shows up as every subsequent lyrics request receiving the same
// fixed decoy payload, which is exactly what made it hard to diagnose live.
// Catching the shape at mint time, before it is ever persisted or used,
// converts that into an immediate, named failure.
func isDegenerateToken(tok string) bool {
	if tok == "" {
		return true
	}
	for i := 1; i < len(tok); i++ {
		if tok[i] != tok[0] {
			return false
		}
	}
	return true
}

// IsDegenerateToken exports isDegenerateToken for callers outside this package
// that must judge a PREVIOUSLY PERSISTED token (#934 AC3): a token stored
// before this fix could be the retired desktop identity's 56-zero value, which
// was accepted and persisted under the old code. internal/commands consults
// this at startup to discard such a stored token and mint a fresh one rather
// than keep sending it.
func IsDegenerateToken(tok string) bool {
	return isDegenerateToken(tok)
}

// TokenMinter obtains a Musixmatch user token from the unauthenticated
// token.get endpoint, so canticle can bootstrap itself with no operator-supplied
// credential (#554).
//
// A minted token MUST be persisted by the caller. Minting on every start would
// trip the endpoint's rate limit and leave the deployment with no token at all.
type TokenMinter struct {
	httpClient *http.Client
	// baseURL is overridden in tests; production always uses tokenURL.
	baseURL string
}

// NewTokenMinter returns a minter using httpClient, or a client with a sane
// timeout when httpClient is nil.
func NewTokenMinter(httpClient *http.Client) *TokenMinter {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &TokenMinter{httpClient: httpClient, baseURL: currentClientIdentity.tokenURL()}
}

// Mint requests a fresh user token.
//
// It returns ErrTokenMintRefused when the endpoint answers with status_code 401
// (rate limited), and ErrClientIdentityRetired when the endpoint accepts the
// request but issues a degenerate token (empty, or one repeated character --
// see isDegenerateToken). Every other failure -- transport, malformed body, or
// a 200 carrying no token at all -- returns a plain error, because persisting
// an empty or unparsable value would be worse than having no token at all.
func (m *TokenMinter) Mint(ctx context.Context) (string, error) {
	u, err := url.Parse(m.baseURL)
	if err != nil {
		return "", fmt.Errorf("musixmatch: parse token URL: %w", err)
	}
	q := u.Query()
	q.Set("app_id", currentClientIdentity.appID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("musixmatch: build token request: %w", err)
	}
	resp, err := m.httpClient.Do(req) //nolint:gosec // reason: G704 - the URL is derived from the package-level currentClientIdentity (tokenURL()), overridden only by tests
	if err != nil {
		return "", fmt.Errorf("musixmatch: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("musixmatch: token endpoint HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBodyBytes))
	if err != nil {
		return "", fmt.Errorf("musixmatch: read token response: %w", err)
	}

	var p fastjson.Parser
	v, err := p.ParseBytes(body)
	if err != nil {
		return "", fmt.Errorf("musixmatch: parse token response: %w", err)
	}
	if code := v.GetInt("message", "header", "status_code"); code == http.StatusUnauthorized {
		return "", ErrTokenMintRefused
	} else if code != http.StatusOK {
		return "", fmt.Errorf("musixmatch: token endpoint status_code %d", code)
	}
	token := string(v.GetStringBytes("message", "body", "user_token"))
	if token == "" {
		return "", errors.New("musixmatch: token response carried no user_token")
	}
	if isDegenerateToken(token) {
		// LOUD and named (see ErrClientIdentityRetired doc): the endpoint
		// answered HTTP 200 / status_code 200, which reads as success at every
		// other layer, so this is the ONE place that can catch it. Length only
		// -- never the token value itself, which per repo convention (redact
		// secrets) must never reach a log line.
		slog.Error("musixmatch: token endpoint issued a degenerate token; the client identity this build impersonates may have been retired upstream",
			"host", currentClientIdentity.host, "app_id", currentClientIdentity.appID, "token_length", len(token))
		return "", ErrClientIdentityRetired
	}
	return token, nil
}
