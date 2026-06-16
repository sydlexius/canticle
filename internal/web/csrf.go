package web

import (
	"net/http"
	"net/url"
	"strings"
)

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
