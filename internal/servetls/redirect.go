package servetls

import (
	"net"
	"net/http"
	"strings"
)

// RedirectHandler returns a handler that 301-redirects every request to the HTTPS
// scheme, preserving the request's host and path/query and substituting the TLS
// listener's port. tlsAddr is the HTTPS listen address (e.g. ":443" or
// "0.0.0.0:8443"); its port is used in the redirect target so a non-standard
// HTTPS port is reached correctly. The standard https port (443) is omitted from
// the target for a clean URL.
func RedirectHandler(tlsAddr string) http.Handler {
	tlsPort := portOf(tlsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		var authority string
		if tlsPort != "" && tlsPort != "443" && tlsPort != "0" {
			// net.JoinHostPort re-brackets an IPv6 literal so the target stays a
			// valid URL (e.g. "::1" + "8443" -> "[::1]:8443").
			authority = net.JoinHostPort(host, tlsPort)
		} else {
			authority = bracketHost(host)
		}
		target := "https://" + authority + r.URL.RequestURI()
		//nolint:gosec // G710: not an open redirect. The target host is the client's own request Host (the host it connected to), only the scheme is forced to https and the port swapped to the TLS port; the path/query are preserved verbatim. This is the standard HTTP->HTTPS host-preserving redirect (same pattern as golang.org/x/crypto/acme/autocert.HTTPHandler). A client setting a different Host only redirects itself.
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// portOf returns the port component of a listen address, or "" if none is present.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return ""
}

// hostOnly strips an optional ":port" suffix from a request Host header,
// returning the bare host without brackets. A bare host (no port) is returned
// unchanged, except a bracketed IPv6 literal ("[::1]") is unbracketed so callers
// can re-bracket it consistently via net.JoinHostPort / bracketHost.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host[1 : len(host)-1]
	}
	return host
}

// bracketHost wraps an IPv6 literal in brackets so it is a valid URL authority.
// A host already bracketed or without a colon is returned unchanged.
func bracketHost(host string) string {
	if strings.HasPrefix(host, "[") {
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
