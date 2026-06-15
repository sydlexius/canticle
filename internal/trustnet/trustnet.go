// Package trustnet implements the client-IP resolution and trusted-network
// allowlist primitive shared by the serve-mode HTTP surface (issue #204,
// Area 2). It answers one question safely: "is this request coming from an IP
// the operator trusts?" - without ever trusting a spoofable header.
//
// The security-critical rule lives in ClientIP: X-Forwarded-For is consulted
// ONLY when the immediate TCP peer (RemoteAddr) is itself a configured trusted
// proxy. Otherwise the header is ignored entirely, so a directly connected
// attacker cannot forge a trusted source by setting a header. When a trusted
// proxy is in front, the XFF chain is walked right-to-left skipping known
// proxies, which defeats a spoofed leftmost entry.
package trustnet

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseCIDRs parses a list of CIDR strings into networks, returning an error on
// the first invalid entry so callers can fail fast at startup rather than fail
// open. A nil or empty input yields a nil slice and no error.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", entry, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// ipInAny reports whether ip is contained in any of the given networks.
func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteAddrIP extracts the IP from an http.Request RemoteAddr ("ip:port" or a
// bare "ip"). It returns nil when the value cannot be parsed as an IP.
func remoteAddrIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// No port present (or otherwise unsplittable); try the raw value.
		host = remoteAddr
	}
	return net.ParseIP(strings.TrimSpace(host))
}

// ClientIP determines the real client IP for a request.
//
// If RemoteAddr is not within any trusted proxy CIDR, X-Forwarded-For is
// ignored and RemoteAddr's IP is returned (the safe default for a directly
// exposed daemon). If RemoteAddr IS a trusted proxy, the X-Forwarded-For chain
// is walked right-to-left and the first address that is not itself a trusted
// proxy is returned as the real client; malformed entries are skipped. When the
// header is absent, empty, malformed, or lists only trusted proxies, the
// proxy's own address (RemoteAddr) is returned. Returns nil only when
// RemoteAddr itself cannot be parsed.
//
// Security note: trusted_proxies entries must NOT overlap the client cidrs
// allowlist. An IP that appears in both lists would be skipped as a proxy hop
// during the XFF chain walk and could never be recognized as the real client.
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) net.IP {
	remoteIP := remoteAddrIP(r.RemoteAddr)
	if remoteIP == nil || !ipInAny(remoteIP, trustedProxies) {
		// Direct connection (or unparsable peer): never trust XFF.
		return remoteIP
	}
	// The immediate peer is a trusted proxy, so XFF is meaningful. Join all
	// XFF header lines before splitting: r.Header.Get returns only the first
	// line, so a proxy that appends the real client as a separate header line
	// (rather than comma-coalescing) would otherwise let an attacker's forged
	// first line win the right-to-left walk. Header.Values returns all lines
	// in received order, keeping the proxy-appended (nearest) line rightmost.
	for _, raw := range splitXFF(strings.Join(r.Header.Values("X-Forwarded-For"), ",")) {
		entry := raw
		// Some proxies include the port (e.g. "203.0.113.7:443"); strip it.
		if host, _, err := net.SplitHostPort(raw); err == nil {
			entry = host
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			continue // skip malformed entry
		}
		if ipInAny(ip, trustedProxies) {
			continue // skip a known proxy hop
		}
		return ip
	}
	// No usable client entry in the chain; fall back to the proxy address.
	return remoteIP
}

// splitXFF splits an X-Forwarded-For header value into trimmed, right-to-left
// (last hop first) entries, dropping empty fields.
func splitXFF(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		if entry := strings.TrimSpace(parts[i]); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// Allowlist matches an IP against a set of trusted CIDRs. Loopback is always
// implicitly trusted (127.0.0.0/8 and ::1) so same-host clients work with an
// empty allowlist and no configuration.
type Allowlist struct {
	nets []*net.IPNet
}

// NewAllowlist builds an Allowlist over the given parsed networks. A nil slice
// yields an allowlist that trusts only loopback.
func NewAllowlist(nets []*net.IPNet) *Allowlist {
	return &Allowlist{nets: nets}
}

// Contains reports whether ip is trusted: loopback is always trusted; otherwise
// the IP must fall within a configured CIDR. A nil IP is never trusted.
func (a *Allowlist) Contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	return ipInAny(ip, a.nets)
}

// Policy bundles a trusted-network Allowlist with the trusted-proxy networks
// used to resolve the real client IP, so a request can be gated in one call. It
// is immutable after construction and safe for concurrent use.
type Policy struct {
	allow   *Allowlist
	proxies []*net.IPNet
}

// NewPolicy builds a Policy from CIDR strings, parsing and validating both
// lists. cidrs is the trusted-network allowlist (loopback is always implicitly
// trusted on top of these); proxies lists the reverse-proxy networks allowed to
// set X-Forwarded-For. An invalid CIDR in either list is a fatal error.
func NewPolicy(cidrs, proxies []string) (*Policy, error) {
	allowNets, err := ParseCIDRs(cidrs)
	if err != nil {
		return nil, fmt.Errorf("trusted_networks.cidrs: %w", err)
	}
	proxyNets, err := ParseCIDRs(proxies)
	if err != nil {
		return nil, fmt.Errorf("trusted_networks.trusted_proxies: %w", err)
	}
	return &Policy{allow: NewAllowlist(allowNets), proxies: proxyNets}, nil
}

// LoopbackOnly returns a default-closed Policy that trusts only loopback and
// trusts no proxy. It is the safe fallback when no trusted networks are
// configured.
func LoopbackOnly() *Policy {
	return &Policy{allow: NewAllowlist(nil)}
}

// ClientIP resolves the real client IP of r under this policy's trusted proxies.
func (p *Policy) ClientIP(r *http.Request) net.IP {
	return ClientIP(r, p.proxies)
}

// Trusted reports whether r originates from a trusted network. It resolves the
// real client IP (proxy-aware, spoof-resistant) and checks it against the
// allowlist (loopback implicitly trusted).
func (p *Policy) Trusted(r *http.Request) bool {
	return p.allow.Contains(p.ClientIP(r))
}
