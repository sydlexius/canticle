package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CSRFCookieName is the browser cookie carrying the CSRF token for the
// double-submit pattern on the pre-authentication forms (/login, /setup).
const CSRFCookieName = "mxlrc_csrf"

// csrfTokenMaxAge is the lifetime of the CSRF cookie in seconds. Short enough
// to limit exposure while long enough for a human to fill the form.
const csrfTokenMaxAge = 3600 // 1 hour

// csrfTokenLen is the expected byte length of a CSRF token value: 32 random
// bytes hex-encoded = 64 characters.
const csrfTokenLen = 64

// generateCSRFToken returns a 64-character hex-encoded random token (32 random
// bytes from crypto/rand) used as the double-submit CSRF token.
func generateCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("csrf: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// setCSRFCookie writes the CSRF cookie with the same security attributes as
// the session cookie: HttpOnly (the server embeds the value in the form field
// so JS does not need to read it), SameSite=Lax, Secure auto-under-TLS, Path=/.
func setCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	//nolint:gosec // G124: Secure is set automatically under TLS (caller passes secureRequest result). HttpOnly + SameSite=Lax are always set. Plain-HTTP local dev intentionally omits Secure.
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   csrfTokenMaxAge,
	})
}

// enforceCSRFToken validates the double-submit CSRF token on a POST: the
// submitted csrf_token form field must exactly match the mxlrc_csrf cookie,
// compared in constant time to prevent timing side-channels. It writes 403
// and returns false on missing/mismatch; returns true when the caller may
// proceed. Call after enforceSameOrigin and before any auth work.
func enforceCSRFToken(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return false
	}
	formToken := r.PostFormValue("csrf_token")
	if len(formToken) != csrfTokenLen {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formToken)) != 1 {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return false
	}
	return true
}

// ensureCSRFToken returns the CSRF token to embed in a rendered form. If a
// valid mxlrc_csrf cookie already exists (non-empty, correct length), its value
// is reused so multiple open tabs share the same token within the cookie
// lifetime. Otherwise a fresh token is generated and the cookie is written.
func ensureCSRFToken(w http.ResponseWriter, r *http.Request, secure bool) (string, error) {
	if c, err := r.Cookie(CSRFCookieName); err == nil && len(c.Value) == csrfTokenLen {
		return c.Value, nil
	}
	token, err := generateCSRFToken()
	if err != nil {
		return "", err
	}
	setCSRFCookie(w, token, secure)
	return token, nil
}

// isSameOriginRequest reports whether r is safe to treat as a same-origin,
// non-cross-site state change. It is a lightweight CSRF guard for the
// cookie-bearing POST endpoints (/setup, /login, /logout): SameSite=Lax does not
// protect /setup (there is no pre-existing session cookie on the first run), so a
// header-based same-origin check is layered in front of every state change.
//
// The predicate, in order:
//   - Sec-Fetch-Site (Fetch Metadata, sent by every modern browser): allow only
//     "same-origin" and "none" (a user-initiated navigation); reject "cross-site"
//     and "same-site".
//   - else Origin: allow only when its host:port equals the request Host.
//   - else Referer: allow only when its host:port equals the request Host
//     (reject on a parse error or empty host).
//   - else if a session cookie is present: reject. A browser carrying a session
//     but stripped of every provenance header (privacy tooling, a very old
//     client) is treated as untrusted, so a cross-site POST cannot ride the
//     victim's session to a state-changing endpoint. This closes the CSRF bypass
//     the unconditional allow used to leave open.
//   - else (no provenance headers and no session, e.g. curl or another
//     non-browser client): allow, because there is no browser-driven CSRF vector
//     to defend against.
func isSameOriginRequest(r *http.Request) bool {
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		u, err := url.Parse(referer)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
		return false
	}
	return true
}

// enforceSameOrigin rejects a cross-site state-changing request with 403 and
// reports whether the caller may proceed. It is the single entry point wired at
// the top of the state-changing POST handlers.
func enforceSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if isSameOriginRequest(r) {
		return true
	}
	http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	return false
}
