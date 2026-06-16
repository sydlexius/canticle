package trustnet

import (
	"net"
	"net/http"
	"testing"
)

// mustParseCIDRs parses CIDRs for test setup, failing the test on any error.
func mustParseCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	nets, err := ParseCIDRs(cidrs)
	if err != nil {
		t.Fatalf("ParseCIDRs(%v) error: %v", cidrs, err)
	}
	return nets
}

// req builds a GET request with the given RemoteAddr and optional
// X-Forwarded-For header.
func req(remoteAddr, xff string) *http.Request {
	r := httptestNewRequest(remoteAddr)
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

// httptestNewRequest builds a minimal *http.Request with RemoteAddr set,
// avoiding a dependency on net/http/httptest in this primitive package.
func httptestNewRequest(remoteAddr string) *http.Request {
	return &http.Request{
		Method:     http.MethodGet,
		Header:     make(http.Header),
		RemoteAddr: remoteAddr,
	}
}

func TestParseCIDRs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		nets, err := ParseCIDRs([]string{"192.168.1.0/24", " 10.0.0.0/8 ", "::1/128"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nets) != 3 {
			t.Fatalf("got %d networks, want 3", len(nets))
		}
	})

	t.Run("empty and blank entries", func(t *testing.T) {
		nets, err := ParseCIDRs(nil)
		if err != nil || nets != nil {
			t.Fatalf("nil input: got (%v, %v), want (nil, nil)", nets, err)
		}
		nets, err = ParseCIDRs([]string{"", "   "})
		if err != nil {
			t.Fatalf("blank entries error: %v", err)
		}
		if len(nets) != 0 {
			t.Fatalf("blank entries: got %d networks, want 0", len(nets))
		}
	})

	t.Run("invalid fails fast", func(t *testing.T) {
		_, err := ParseCIDRs([]string{"192.168.1.0/24", "not-a-cidr", "10.0.0.0/8"})
		if err == nil {
			t.Fatal("expected error for invalid CIDR, got nil")
		}
	})

	t.Run("bare IP without mask is invalid", func(t *testing.T) {
		// net.ParseCIDR requires a prefix length; a bare IP is rejected.
		if _, err := ParseCIDRs([]string{"192.168.1.5"}); err == nil {
			t.Fatal("expected error for bare IP, got nil")
		}
	})
}

func TestClientIP(t *testing.T) {
	proxies := mustParseCIDRs(t, "10.0.0.0/8")

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		proxies    []*net.IPNet
		want       string // expected client IP, "" means nil
	}{
		{
			name:       "no proxies configured ignores XFF (spoof fails)",
			remoteAddr: "203.0.113.5:443",
			xff:        "192.168.1.50",
			proxies:    nil,
			want:       "203.0.113.5",
		},
		{
			name:       "untrusted remote ignores XFF even with proxies set (spoof fails)",
			remoteAddr: "203.0.113.5:443",
			xff:        "192.168.1.50",
			proxies:    proxies,
			want:       "203.0.113.5", // direct attacker cannot forge 192.168.1.50
		},
		{
			name:       "trusted proxy honors XFF",
			remoteAddr: "10.0.0.1:443",
			xff:        "198.51.100.7",
			proxies:    proxies,
			want:       "198.51.100.7",
		},
		{
			name:       "trusted proxy chain walked right-to-left skipping proxies",
			remoteAddr: "10.0.0.1:443",
			xff:        "198.51.100.7, 10.0.0.9, 10.0.0.1",
			proxies:    proxies,
			want:       "198.51.100.7", // both 10.x hops skipped, real client returned
		},
		{
			name:       "forged leftmost entry cannot be trusted",
			remoteAddr: "10.0.0.1:443",
			xff:        "1.2.3.4, 198.51.100.7, 10.0.0.1",
			proxies:    proxies,
			// Walking right-to-left, 10.0.0.1 is a proxy (skip), 198.51.100.7 is
			// the first non-proxy and is returned; the attacker-injected leftmost
			// 1.2.3.4 is never reached.
			want: "198.51.100.7",
		},
		{
			name:       "IPv6 client via trusted proxy",
			remoteAddr: "10.0.0.1:443",
			xff:        "2001:db8::1234",
			proxies:    proxies,
			want:       "2001:db8::1234",
		},
		{
			name:       "malformed XFF entries skipped",
			remoteAddr: "10.0.0.1:443",
			xff:        "garbage, 198.51.100.7",
			proxies:    proxies,
			want:       "198.51.100.7",
		},
		{
			name:       "empty XFF from trusted proxy falls back to proxy addr",
			remoteAddr: "10.0.0.1:443",
			xff:        "",
			proxies:    proxies,
			want:       "10.0.0.1",
		},
		{
			name:       "all-proxy XFF chain falls back to proxy addr",
			remoteAddr: "10.0.0.1:443",
			xff:        "10.0.0.2, 10.0.0.3",
			proxies:    proxies,
			want:       "10.0.0.1",
		},
		{
			name:       "bare RemoteAddr without port",
			remoteAddr: "203.0.113.5",
			xff:        "192.168.1.50",
			proxies:    proxies,
			want:       "203.0.113.5",
		},
		{
			name:       "unparsable RemoteAddr yields nil",
			remoteAddr: "not-an-ip",
			xff:        "",
			proxies:    proxies,
			want:       "",
		},
		{
			name:       "port-bearing XFF entry parsed correctly",
			remoteAddr: "10.0.0.1:443",
			xff:        "203.0.113.7:443",
			proxies:    proxies,
			want:       "203.0.113.7",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientIP(req(tc.remoteAddr, tc.xff), tc.proxies)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			want := net.ParseIP(tc.want)
			if !got.Equal(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// TestRemoteAddrIPZoneID verifies that remoteAddrIP correctly strips an IPv6
// zone identifier (the %iface suffix) before parsing, so link-local addresses
// like fe80::1%eth0 are not rejected by net.ParseIP.
func TestRemoteAddrIPZoneID(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string // expected IP string, "" means nil
	}{
		{"bracketed zone addr", "[fe80::1%eth0]:8080", "fe80::1"},
		{"bare zone addr", "fe80::1%eth0", "fe80::1"},
		{"bracketed no zone", "[fe80::1]:8080", "fe80::1"},
		{"bare no zone", "fe80::1", "fe80::1"},
		{"ipv4 port", "192.0.2.1:8080", "192.0.2.1"},
		{"bare ipv4", "192.0.2.1", "192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remoteAddrIP(tc.addr)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("remoteAddrIP(%q) = %v, want nil", tc.addr, got)
				}
				return
			}
			want := net.ParseIP(tc.want)
			if !got.Equal(want) {
				t.Fatalf("remoteAddrIP(%q) = %v, want %v", tc.addr, got, want)
			}
		})
	}
}

// TestClientIPZoneIDRemoteAddr verifies that a request whose RemoteAddr is a
// bracketed IPv6 address with a zone identifier is handled correctly end-to-end
// by ClientIP (the zone suffix must be stripped so the IP is parseable and can
// match a CIDR).
func TestClientIPZoneIDRemoteAddr(t *testing.T) {
	// fe80::/10 covers all link-local IPv6 addresses.
	proxies := mustParseCIDRs(t, "fe80::/10")

	r := httptestNewRequest("[fe80::1%eth0]:8080")
	r.Header.Set("X-Forwarded-For", "203.0.113.5")

	// fe80::1 falls inside fe80::/10, so it is a trusted proxy and XFF is used.
	got := ClientIP(r, proxies)
	want := net.ParseIP("203.0.113.5")
	if !got.Equal(want) {
		t.Fatalf("ClientIP with zone-id RemoteAddr: got %v, want %v", got, want)
	}
}

// TestClientIPMultiLineXFFAdversarial verifies that an attacker who sends a
// forged X-Forwarded-For header line cannot spoof a trusted IP when the
// upstream proxy appends the real client IP as a SEPARATE header line (not
// comma-coalesced). The right-to-left walk must consider all header lines.
func TestClientIPMultiLineXFFAdversarial(t *testing.T) {
	proxies := mustParseCIDRs(t, "10.0.0.0/8")
	// RemoteAddr is a trusted proxy.
	r := httptestNewRequest("10.0.0.1:443")
	// Line 1 (first): attacker-controlled forged entry claiming a loopback address.
	r.Header.Add("X-Forwarded-For", "127.0.0.1")
	// Line 2 (second): the real client IP appended by the upstream proxy.
	r.Header.Add("X-Forwarded-For", "203.0.113.99")

	got := ClientIP(r, proxies)
	want := net.ParseIP("203.0.113.99")
	if !got.Equal(want) {
		t.Fatalf("multi-line XFF spoof: got %v, want %v (attacker forged line must not win)", got, want)
	}
}

func TestAllowlistContains(t *testing.T) {
	nets := mustParseCIDRs(t, "192.168.1.0/24")
	allow := NewAllowlist(nets)

	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4 implicitly trusted with allowlist", "127.0.0.1", true},
		{"loopback v6 implicitly trusted", "::1", true},
		{"loopback IPv4-mapped form implicitly trusted", "::ffff:127.0.0.1", true},
		{"in-allowlist IP trusted", "192.168.1.42", true},
		{"out-of-allowlist IP refused", "203.0.113.9", false},
		{"adjacent subnet refused", "192.168.2.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allow.Contains(net.ParseIP(tc.ip)); got != tc.want {
				t.Fatalf("Contains(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}

	t.Run("nil IP never trusted", func(t *testing.T) {
		if allow.Contains(nil) {
			t.Fatal("nil IP should not be trusted")
		}
	})

	t.Run("empty allowlist still trusts loopback", func(t *testing.T) {
		empty := NewAllowlist(nil)
		if !empty.Contains(net.ParseIP("127.0.0.1")) {
			t.Fatal("loopback should be trusted with empty allowlist")
		}
		if empty.Contains(net.ParseIP("192.168.1.1")) {
			t.Fatal("non-loopback should be refused with empty allowlist")
		}
	})
}

func TestPolicy(t *testing.T) {
	t.Run("invalid cidrs error", func(t *testing.T) {
		if _, err := NewPolicy([]string{"bad"}, nil); err == nil {
			t.Fatal("expected error for invalid cidrs")
		}
	})

	t.Run("invalid proxies error", func(t *testing.T) {
		if _, err := NewPolicy(nil, []string{"bad"}); err == nil {
			t.Fatal("expected error for invalid proxies")
		}
	})

	t.Run("loopback only default closed", func(t *testing.T) {
		p := LoopbackOnly()
		if !p.Trusted(req("127.0.0.1:5000", "")) {
			t.Fatal("loopback should be trusted")
		}
		if p.Trusted(req("203.0.113.1:5000", "")) {
			t.Fatal("remote should be refused with default-closed policy")
		}
	})

	t.Run("trusted via allowlist", func(t *testing.T) {
		p, err := NewPolicy([]string{"192.168.0.0/16"}, nil)
		if err != nil {
			t.Fatalf("NewPolicy error: %v", err)
		}
		if !p.Trusted(req("192.168.5.5:5000", "")) {
			t.Fatal("in-allowlist IP should be trusted")
		}
		if p.Trusted(req("203.0.113.1:5000", "")) {
			t.Fatal("out-of-allowlist IP should be refused")
		}
	})

	t.Run("spoofed XFF cannot forge trust without trusted proxy", func(t *testing.T) {
		p, err := NewPolicy([]string{"192.168.0.0/16"}, nil)
		if err != nil {
			t.Fatalf("NewPolicy error: %v", err)
		}
		// Direct attacker from a non-trusted IP sets XFF to an allowlisted IP.
		if p.Trusted(req("203.0.113.1:5000", "192.168.5.5")) {
			t.Fatal("spoofed XFF must not forge a trusted source")
		}
	})

	t.Run("trusted proxy resolves real client for allowlist", func(t *testing.T) {
		p, err := NewPolicy([]string{"192.168.0.0/16"}, []string{"10.0.0.0/8"})
		if err != nil {
			t.Fatalf("NewPolicy error: %v", err)
		}
		// Real client (allowlisted) behind a trusted proxy.
		if !p.Trusted(req("10.0.0.1:5000", "192.168.5.5")) {
			t.Fatal("client behind trusted proxy should be trusted")
		}
		// Non-allowlisted real client behind a trusted proxy.
		if p.Trusted(req("10.0.0.1:5000", "203.0.113.1")) {
			t.Fatal("non-allowlisted client behind proxy should be refused")
		}
	})
}

// TestPolicyFromTrustedProxy verifies FromTrustedProxy reports whether the
// immediate peer (RemoteAddr) is a configured trusted proxy, independent of any
// forwarding header (it is used to decide whether X-Forwarded-Proto may be
// believed for the Secure-cookie decision).
func TestPolicyFromTrustedProxy(t *testing.T) {
	p, err := NewPolicy(nil, []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("NewPolicy error: %v", err)
	}
	if !p.FromTrustedProxy(req("10.1.2.3:443", "")) {
		t.Error("peer inside trusted-proxy CIDR should report FromTrustedProxy=true")
	}
	if p.FromTrustedProxy(req("203.0.113.9:443", "")) {
		t.Error("peer outside trusted-proxy CIDR must report FromTrustedProxy=false")
	}
	// A spoofed header cannot make an untrusted peer count as a proxy.
	if p.FromTrustedProxy(req("203.0.113.9:443", "10.0.0.1")) {
		t.Error("X-Forwarded-For must not influence FromTrustedProxy")
	}
	// No proxies configured (default): nothing is a trusted proxy.
	if LoopbackOnly().FromTrustedProxy(req("10.1.2.3:443", "")) {
		t.Error("with no trusted proxies configured, FromTrustedProxy must be false")
	}
	// A nil request must not panic.
	if p.FromTrustedProxy(nil) {
		t.Error("FromTrustedProxy(nil) must return false, not panic")
	}
}
