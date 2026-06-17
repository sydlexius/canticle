package web

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
	"github.com/sydlexius/mxlrcgo-svc/internal/webauth"
	"github.com/sydlexius/mxlrcgo-svc/web/templates"
)

// SessionCookieName is the browser cookie carrying the raw session token. The
// raw token is sent only to the browser; the server stores only its SHA-256
// (see internal/webauth), so a stolen DB does not yield usable tokens.
const SessionCookieName = "mxlrc_session"

// defaultLoginError is the single, enumeration-safe message shown for every
// login failure. It never distinguishes an unknown username from a wrong
// password.
const defaultLoginError = "Invalid username or password."

// SessionService is the subset of *webauth.Service the web layer needs. Keeping
// it an interface lets tests substitute the real service over in-memory SQLite
// (the repo pattern) while the web package stays decoupled from the stores.
type SessionService interface {
	Login(ctx context.Context, username, password string) (string, error)
	ValidateSession(ctx context.Context, rawToken string) (*webauth.User, error)
	Logout(ctx context.Context, rawToken string) error
}

// Auth wires browser session authentication onto the web UI: the RequireSession
// middleware, the login/logout endpoints, the per-IP login rate limiter, and
// session-cookie management. It is safe for concurrent use.
type Auth struct {
	service   SessionService
	policy    *trustnet.Policy
	version   string
	cookieTTL time.Duration
	limiter   *loginLimiter
	// sleep applies the login backoff before responding to a failed attempt.
	// Overridable in tests so backoff logic can be asserted without real delays.
	sleep func(time.Duration)
}

// AuthOption customizes an Auth.
type AuthOption func(*Auth)

// WithCookieTTL overrides the session cookie Max-Age (default: the webauth
// session TTL, 7 days). It should match the server-side session lifetime.
func WithCookieTTL(ttl time.Duration) AuthOption {
	return func(a *Auth) {
		if ttl > 0 {
			a.cookieTTL = ttl
		}
	}
}

// withSleep overrides the backoff sleep (tests only).
func withSleep(sleep func(time.Duration)) AuthOption {
	return func(a *Auth) {
		if sleep != nil {
			a.sleep = sleep
		}
	}
}

// NewAuth builds the web auth subsystem. service validates credentials and
// sessions; policy supplies the trusted-network bypass and proxy-aware TLS
// detection (a nil policy means no bypass and Secure is set only on a direct TLS
// request); version labels the login page.
func NewAuth(service SessionService, policy *trustnet.Policy, version string, opts ...AuthOption) *Auth {
	if service == nil {
		panic("web: NewAuth: service must not be nil")
	}
	a := &Auth{
		service:   service,
		policy:    policy,
		version:   version,
		cookieTTL: webauth.DefaultSessionTTL,
		limiter:   newLoginLimiter(),
		sleep:     time.Sleep,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// RequireSession gates the web UI routes. A request from a trusted network
// bypasses the interactive session requirement entirely (this bypass applies
// ONLY to the UI - it never touches the API-key-protected webhook/admin
// endpoints, which are registered outside this middleware). Otherwise a valid
// mxlrc_session cookie is required; without one the request is redirected to
// /login (303, preserving a safe return path) for an HTML client, or answered
// with 401 for an XHR/JSON client.
func (a *Auth) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.policy != nil && a.policy.Trusted(r) {
			next.ServeHTTP(w, r)
			return
		}
		if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
			if _, verr := a.service.ValidateSession(r.Context(), cookie.Value); verr == nil {
				next.ServeHTTP(w, r)
				return
			}
		}
		if wantsJSON(r) {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(safeReturnPath(r)), http.StatusSeeOther)
	})
}

// handleLoginForm renders the login page (GET /login). A client that already
// holds a valid session is sent on to its destination rather than shown the
// form again.
func (a *Auth) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if a.hasValidSession(r) {
		//nolint:gosec // G710: redirect target is sanitized by safeNext to a local, same-site path (open-redirect closed)
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	a.renderLogin(w, r, a.version, "", next, "")
}

// handleLogin verifies credentials (POST /login). On success it creates a
// session, sets the cookie, and redirects to the validated return path. On
// failure it applies the per-IP backoff and re-renders the form with a generic,
// enumeration-safe error. A hard-locked IP is refused with 429 before any
// credential check.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) {
		return
	}
	// CSRF validation must come before the rate limiter so an attacker cannot
	// exhaust the lockout counter for legitimate users with CSRF-invalid requests.
	if !enforceCSRFToken(w, r) {
		return
	}
	ip := a.clientKey(r)
	if ok, retryAfter := a.limiter.allow(ip); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
		a.renderLoginStatus(w, r, http.StatusTooManyRequests, a.version,
			"Too many failed attempts. Try again later.", safeNext(r.FormValue("next")), "")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderLoginStatus(w, r, http.StatusBadRequest, a.version,
			"Invalid form submission.", "/config", "")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	token, err := a.service.Login(r.Context(), username, password)
	if err != nil {
		// Any failure - unknown user or wrong password - feeds the limiter and
		// returns the identical message (webauth.Login is itself enumeration-safe).
		backoff := a.limiter.fail(ip)
		a.sleep(backoff)
		a.renderLoginStatus(w, r, http.StatusUnauthorized, a.version,
			defaultLoginError, next, username)
		return
	}

	a.limiter.success(ip)
	a.setSessionCookie(w, r, token)
	//nolint:gosec // G710: next is sanitized by safeNext to a local, same-site path (open-redirect closed)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout revokes the session server-side and clears the cookie
// (POST /logout), then redirects to the login page.
func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) {
		return
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		// Best-effort revoke; a failure here must not prevent clearing the cookie.
		_ = a.service.Logout(r.Context(), cookie.Value)
	}
	a.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// hasValidSession reports whether r carries a currently valid session cookie.
func (a *Auth) hasValidSession(r *http.Request) bool {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	_, verr := a.service.ValidateSession(r.Context(), cookie.Value)
	return verr == nil
}

// clientKey resolves the limiter/bypass key for r: the real client IP under the
// trusted-proxy policy, falling back to the raw RemoteAddr when no policy is set
// or the IP cannot be resolved (so a request is never un-keyed).
func (a *Auth) clientKey(r *http.Request) string {
	if a.policy != nil {
		if ip := a.policy.ClientIP(r); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// setSessionCookie writes the session cookie. Secure is set automatically when
// the effective connection is TLS (direct, or TLS terminated by a trusted proxy
// that set X-Forwarded-Proto: https) so the cookie is never sent in cleartext on
// an HTTPS deployment, while plain-HTTP local dev still works.
func (a *Auth) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	//nolint:gosec // G124: Secure is set automatically under TLS via secureRequest; it is intentionally off on plain HTTP so local dev works (issue #204 design). HttpOnly + SameSite=Lax are always set.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secureRequest(r),
		MaxAge:   int(a.cookieTTL.Seconds()),
	})
}

// clearSessionCookie expires the session cookie. The attributes (Path, flags)
// mirror setSessionCookie so the browser reliably overwrites the original.
func (a *Auth) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	//nolint:gosec // G124: an expiring (MaxAge<0) cookie; Secure mirrors setSessionCookie (auto under TLS). HttpOnly + SameSite=Lax are always set.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secureRequest(r),
		MaxAge:   -1,
	})
}

// secureRequest reports whether the effective connection is TLS. A direct TLS
// request is trusted outright; a forwarded X-Forwarded-Proto: https is believed
// only when the immediate peer is a configured trusted proxy (never from a
// spoofable header alone).
func (a *Auth) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if a.policy != nil && a.policy.FromTrustedProxy(r) &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return false
}

// renderLogin renders the login page with an implicit 200 status.
func (a *Auth) renderLogin(w http.ResponseWriter, r *http.Request, version, errMsg, next, username string) {
	a.renderLoginStatus(w, r, http.StatusOK, version, errMsg, next, username)
}

// renderLoginStatus renders the login page with an explicit status code. It
// generates a fresh CSRF token, sets the CSRF cookie, then renders the page
// into a buffer so a render failure yields a clean 500. Login pages are never
// cached (they reflect attempt state and may set cookies).
func (a *Auth) renderLoginStatus(w http.ResponseWriter, r *http.Request, status int, version, errMsg, next, username string) {
	csrfToken, err := ensureCSRFToken(w, r, a.secureRequest(r))
	if err != nil {
		slog.Error("login: failed to generate CSRF token", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	renderWithStatus(w, r, status, templates.LoginPage(version, errMsg, next, username, csrfToken))
}

// retryAfterSeconds renders a Retry-After header value (whole seconds, min 1).
func retryAfterSeconds(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// wantsJSON reports whether the client prefers a JSON/programmatic response
// (an XHR or a JSON Accept header) over an HTML redirect to the login page.
func wantsJSON(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// safeReturnPath builds the post-login return path from the current request's
// URI, sanitized so it can only point back into this site.
func safeReturnPath(r *http.Request) string {
	return safeNext(r.URL.RequestURI())
}

// safeNext sanitizes a caller-supplied return path to a local, same-site path,
// defaulting to /config. It rejects anything that is not a single-slash-rooted
// path (absolute URLs, scheme-relative "//host", and backslash tricks) to close
// the open-redirect vector.
func safeNext(raw string) string {
	const fallback = "/config"
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '/' {
		return fallback
	}
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return fallback
	}
	if strings.ContainsAny(raw, "\\\r\n") {
		return fallback
	}
	// Reject a path that parses to an absolute URL with a host/scheme.
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
		return fallback
	}
	return raw
}
