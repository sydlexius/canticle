package innertube

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCheckRedirect_RefusesCrossHost pins the SSRF guard, mirroring
// internal/petitlyrics.TestCheckRedirect_RefusesCrossHost.
func TestCheckRedirect_RefusesCrossHost(t *testing.T) {
	const base = "https://music.youtube.com"

	same, _ := http.NewRequest(http.MethodGet, "https://music.youtube.com/x", nil)
	if err := checkRedirect(base, same, nil); err != nil {
		t.Errorf("same-host redirect should be allowed: %v", err)
	}
	other, _ := http.NewRequest(http.MethodGet, "https://evil.example/x", nil)
	if err := checkRedirect(base, other, nil); err == nil {
		t.Error("cross-host redirect must be refused")
	}
	deep := make([]*http.Request, 10)
	if err := checkRedirect(base, same, deep); err == nil {
		t.Error("redirect chains must be capped at 10 hops")
	}
}

// newGuardedClient wires checkRedirect into a plain http.Client pinned to
// baseURL, which is how the transport slice consumes this guard: through a
// closure, so a base URL chosen after construction (an httptest listener) is
// still the one the guard reads at redirect time.
func newGuardedClient(baseURL string) *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRedirect(baseURL, req, via)
		},
	}
}

// TestCheckRedirect_FollowsSameHostRedirect is the positive control for the
// two refusal tests below: a same-origin redirect must actually be FOLLOWED,
// so a guard that refuses everything cannot pass this file.
func TestCheckRedirect_FollowsSameHostRedirect(t *testing.T) {
	var hitFinal atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		hitFinal.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := newGuardedClient(srv.URL).Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("same-host redirect should have been followed: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if !hitFinal.Load() {
		t.Error("same-host redirect should have reached the final handler")
	}
}

// TestCheckRedirect_RefusesCrossHostRedirectOnTheWire proves the refusal
// against real listeners, not only against the predicate in isolation: the
// cross-host target must never be reached at all.
func TestCheckRedirect_RefusesCrossHostRedirectOnTheWire(t *testing.T) {
	var evilHit atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		evilHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evil.Close)

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(primary.Close)

	res, err := newGuardedClient(primary.URL).Get(primary.URL)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("want an error when the server attempts a cross-host redirect")
	}
	if evilHit.Load() {
		t.Error("the client must never have reached the cross-host target")
	}
}

// TestCheckRedirect_RefusesSchemeDowngradeOnTheWire is 854-F1's required
// end-to-end proof: a real https origin redirecting to a real plain-http
// target must be refused before the http target is ever reached.
func TestCheckRedirect_RefusesSchemeDowngradeOnTheWire(t *testing.T) {
	var httpHit atomic.Bool
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(httpTarget.Close)

	httpsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL+"/x", http.StatusFound)
	}))
	t.Cleanup(httpsOrigin.Close)

	client := newGuardedClient(httpsOrigin.URL)
	client.Transport = httpsOrigin.Client().Transport

	res, err := client.Get(httpsOrigin.URL)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("want an error when the server attempts an https->http scheme downgrade")
	}
	if httpHit.Load() {
		t.Error("the client must never have followed the downgraded http redirect")
	}
}

// TestCheckRedirect_HostAndSchemeMatrix is the full regression matrix from
// the hostile review of 854-F1: every host-shaped attack the guard already
// handled correctly, the two scheme-downgrade cases F1 fixes, and the
// default-port normalization F6 adds. Table-driven so a future regression on
// any single case is immediately attributable.
func TestCheckRedirect_HostAndSchemeMatrix(t *testing.T) {
	const base = "https://music.youtube.com"

	cases := []struct {
		name    string
		target  string
		allowed bool
	}{
		{"baseline same-host https", "https://music.youtube.com/x", true},
		{"scheme downgrade to http", "http://music.youtube.com/x", false},
		{"scheme downgrade to ftp", "ftp://music.youtube.com/x", false},
		{"suffix host evil-music.youtube.com", "https://evil-music.youtube.com/x", false},
		{"subdomain evil.music.youtube.com", "https://evil.music.youtube.com/x", false},
		{"prefix-match music.youtube.com.evil.tld", "https://music.youtube.com.evil.tld/x", false},
		// explicit default port normalizes to the implicit form (854-F6).
		{"explicit default port 443", "https://music.youtube.com:443/x", true},
		{"explicit non-default port 8443", "https://music.youtube.com:8443/x", false},
		{"userinfo host-looking user", "https://music.youtube.com@evil.example/x", false},
		{"userinfo before an on-host target", "https://evil.example@music.youtube.com/x", true},
		{"uppercase host", "https://MUSIC.YOUTUBE.COM/x", false},
		{"mixed case host", "https://Music.YouTube.com/x", false},
		{"trailing-dot FQDN", "https://music.youtube.com./x", false},
		{"punycode homograph", "https://xn--msic-0ra.youtube.com/x", false},
		{"unicode homograph (Cyrillic s)", "https://muѕic.youtube.com/x", false},
		{"IPv4 literal (IMDS)", "https://169.254.169.254/latest/meta-data/", false},
		{"IPv6 literal", "https://[::1]/x", false},
		{"localhost", "https://localhost/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatalf("parse target %q: %v", tc.target, err)
			}
			gotErr := checkRedirect(base, req, nil)
			allowed := gotErr == nil
			if allowed != tc.allowed {
				t.Errorf("%s: allowed=%v (err=%v), want allowed=%v", tc.target, allowed, gotErr, tc.allowed)
			}
		})
	}
}

// TestCheckRedirect_HTTPBaseAllowsHTTPRedirect documents the deliberate
// fail-open half of checkRedirect's scheme comparison (854-F1 follow-up): a
// caller whose base is already plain http gains no exposure from following
// an http redirect, because the channel was cleartext to begin with. This is
// the shape every test in this package actually relies on (an
// httptest.Server base), and the assertion here is what would catch a
// regression that accidentally hardcoded "https" into checkRedirect.
func TestCheckRedirect_HTTPBaseAllowsHTTPRedirect(t *testing.T) {
	const base = "http://127.0.0.1:9"

	same, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/x", nil)
	if err := checkRedirect(base, same, nil); err != nil {
		t.Errorf("an http base should allow a same-scheme http redirect: %v", err)
	}
}

// TestCheckRedirect_HTTPBaseRefusesHTTPSUpgrade documents the other half:
// a caller whose base is plain http refuses a redirect that switches to
// https. This is fail-closed and harmless (the caller merely loses a
// redirect it could safely have followed), called out explicitly so a
// future reader does not "fix" the asymmetry by allowing an upgrade, which
// would legitimize scheme MISMATCH in general rather than only the
// no-exposure http-to-http case above.
func TestCheckRedirect_HTTPBaseRefusesHTTPSUpgrade(t *testing.T) {
	const base = "http://127.0.0.1:9"

	upgraded, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1:9/x", nil)
	if err := checkRedirect(base, upgraded, nil); err == nil {
		t.Error("an http base should refuse a same-host https upgrade (scheme mismatch), not just a downgrade")
	}
}

// TestCheckRedirect_SchemeComparisonIsCaseInsensitive guards the casing
// question directly: url.Parse lowercases the scheme it returns regardless
// of the input's casing, so an uppercase or mixed-case scheme in a redirect
// Location cannot be used to slip past the scheme comparison by case.
func TestCheckRedirect_SchemeComparisonIsCaseInsensitive(t *testing.T) {
	const base = "https://music.youtube.com"

	for _, raw := range []string{
		"https://music.youtube.com/x",
		"HTTPS://music.youtube.com/x",
		"Https://music.youtube.com/x",
	} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if req.URL.Scheme != "https" {
			t.Fatalf("url.Parse did not lowercase scheme for %q: got %q", raw, req.URL.Scheme)
		}
		if err := checkRedirect(base, req, nil); err != nil {
			t.Errorf("%q: same-origin redirect should be allowed regardless of scheme casing: %v", raw, err)
		}
	}
}

// TestCheckRedirect_ErrorNamesTheHostOnly pins the refusal message's CONTENT,
// not just that a refusal happened. url.URL.String() renders userinfo
// verbatim and carries the query, so a hostile Location could write
// credentials into a log and, further up, a work_queue failure reason -- and
// an innertube search URL's query holds the library's private artist and
// title. Nothing else asserts on the error text, so without this a
// "clearer" message citing the full URL would ship green (854-R5F1).
func TestCheckRedirect_ErrorNamesTheHostOnly(t *testing.T) {
	const base = "https://music.youtube.com"
	req := &http.Request{URL: mustParseURL(t, "https://admin:hunter2@evil.example/steal?artist=Private&title=Track")}

	err := checkRedirect(base, req, nil)
	if err == nil {
		t.Fatal("a cross-origin redirect must be refused")
	}
	got := err.Error()
	for _, leak := range []string{"admin", "hunter2", "artist=", "title=", "Private", "Track", "/steal"} {
		if strings.Contains(got, leak) {
			t.Errorf("refusal message leaks %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "evil.example") {
		t.Errorf("refusal message must still name the refused host, got: %s", got)
	}
}

// TestCheckRedirect_UnparsableBaseFailsClosed pins the base-URL parse-error
// branch, which no test reached: deleting it left the suite green. It is
// reachable because checkRedirect takes the base as a STRING and reads it at
// redirect time, so a later slice that makes the base configurable can feed
// it an unparsable value. A guard that fails OPEN there would follow the
// redirect it exists to refuse (854-R5F2).
func TestCheckRedirect_UnparsableBaseFailsClosed(t *testing.T) {
	req := &http.Request{URL: mustParseURL(t, "https://music.youtube.com/watch")}

	for _, base := range []string{
		"https://exa mple.com/\x7f", // unparsable: control character
		"",                          // empty: no scheme, no host
		"music.youtube.com",         // scheme-less, so nothing can match
	} {
		if err := checkRedirect(base, req, nil); err == nil {
			t.Errorf("base %q: must refuse rather than fail open", base)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
