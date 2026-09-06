package innertube

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// checkRedirect pins redirects to baseURL's host AND scheme,
// mirroring internal/ffmpeg.httpsOnlyRedirect rather than
// internal/petitlyrics.Client.checkRedirect -- petitlyrics is the sibling
// that lacks a scheme check (854-F1). The default http.Client follows up to
// 10 redirects without restricting the target host or scheme, so a 3xx from
// the API could otherwise move a request to an arbitrary host (SSRF) or
// silently downgrade an https call to cleartext http (or ftp), where the
// request and response become readable/modifiable in transit. url.URL.Host
// carries no scheme, so the host comparison alone cannot catch a same-host
// downgrade -- both must be checked. This rejects cross-host and
// cross-scheme redirects and preserves the standard 10-hop cap.
//
// The scheme is compared against base.Scheme, not a hardcoded "https": every
// test in this package injects an httptest http://127.0.0.1 baseURL, and a
// hardcoded https would refuse every redirect those tests follow -- a green
// suite alone does not prove the guard refuses a downgrade, only that it
// isn't hardcoded; see TestCheckRedirect_RefusesSchemeDowngradeOnTheWire for
// the actual end-to-end proof against a real listener.
//
// req.URL.Scheme != base.Scheme means a Client constructed with a plain-http
// base would ACCEPT an http redirect and REFUSE an https one (an upgrade).
// Both are deliberate, not oversights:
//   - accepting http when the base is already http adds no exposure -- the
//     channel was cleartext to begin with, so there is nothing left to
//     downgrade.
//   - refusing an https upgrade from an http base is fail-closed and
//     harmless (the caller loses a redirect it could have safely followed),
//     called out here so a future reader does not "fix" it into accepting
//     the upgrade, which would reintroduce scheme-mismatch exposure the
//     other direction.
//
// This is a free function rather than a method so the guard is testable,
// and reviewable, without a transport client: it needs a base URL and a
// request, nothing else. The transport slice wires it into
// http.Client.CheckRedirect through a closure that reads its own base URL at
// redirect time, which is what lets a test point a client at an httptest
// listener after construction.
//
// The guard deliberately takes no position on whether a non-https base URL
// should exist at all -- that is the constructing caller's decision. Its own
// job stays "don't let a redirect change origin out from under the caller's
// choice."
//
// url.Parse lowercases the scheme it returns regardless of the input's
// casing (verified: "HTTPS://..." and "Https://..." both parse to
// scheme "https"), so an uppercase scheme in a redirect Location cannot be
// used to bypass this comparison by case.
func checkRedirect(baseURL string, req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("innertube: stopped after 10 redirects")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("innertube: parse base URL: %w", err)
	}
	if !sameOrigin(req.URL, base) {
		return fmt.Errorf("innertube: refusing cross-origin redirect to %q", req.URL.String())
	}
	return nil
}

// sameOrigin reports whether a and b share a scheme and host, treating an
// explicit default port for the scheme as equivalent to an implicit one
// (854-F6): "https://music.youtube.com" and "https://music.youtube.com:443"
// are the same origin, but url.URL.Host carries the port verbatim, so a
// literal comparison would refuse a same-origin redirect that merely spells
// out the default port. Normalizing only the DEFAULT port keeps the guard's
// core cross-host protection unweakened -- an explicit non-default port
// still compares as written, so a redirect from :443 to :8443 on the same
// host is still refused.
func sameOrigin(a, b *url.URL) bool {
	if a.Scheme != b.Scheme {
		return false
	}
	return stripDefaultPort(a) == stripDefaultPort(b)
}

// stripDefaultPort returns u.Host with its port removed if that port is the
// default for u.Scheme, so "host:443" (https) and "host:80" (http) compare
// equal to bare "host".
//
// The suffix match is safe against two net/url guarantees rather than
// anything local: url.URL.Host keeps a bracketed IPv6 literal (e.g.
// "[::443]" ends in "3]", never ":443"), and userinfo never reaches .Host, so
// a userinfo segment ending in ":443" cannot be stripped either. A future
// caller that stops sourcing host from url.URL.Host would silently break
// this assumption.
func stripDefaultPort(u *url.URL) string {
	host := u.Host
	defaultPort := ""
	switch u.Scheme {
	case "https":
		defaultPort = "443"
	case "http":
		defaultPort = "80"
	}
	if defaultPort != "" && strings.HasSuffix(host, ":"+defaultPort) {
		return strings.TrimSuffix(host, ":"+defaultPort)
	}
	return host
}
