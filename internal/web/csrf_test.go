package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
)

// TestSameOriginGuard proves the same-origin CSRF guard on all three
// state-changing POST endpoints (/setup, /login, /logout): a cross-site
// Sec-Fetch-Site or a mismatched Origin is refused with 403 before any state
// change, while a same-origin request and a header-less (non-browser) request
// are allowed through (they do not return 403).
func TestSameOriginGuard(t *testing.T) {
	// A header set on the request and the status the guard must produce. "pass"
	// asserts the guard did NOT short-circuit (any non-403 code).
	type headerCase struct {
		name    string
		headers map[string]string
		want403 bool
	}
	cases := []headerCase{
		{name: "cross-site Sec-Fetch-Site", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, want403: true},
		{name: "same-site Sec-Fetch-Site", headers: map[string]string{"Sec-Fetch-Site": "same-site"}, want403: true},
		{name: "same-origin Sec-Fetch-Site", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, want403: false},
		{name: "none Sec-Fetch-Site", headers: map[string]string{"Sec-Fetch-Site": "none"}, want403: false},
		{name: "mismatched Origin", headers: map[string]string{"Origin": "http://evil.example"}, want403: true},
		{name: "same-origin Origin", headers: map[string]string{"Origin": "http://example.com"}, want403: false},
		{name: "no headers", headers: nil, want403: false},
	}

	form := url.Values{
		"username": {"admin"},
		"password": {"correct-horse-battery"},
		"confirm":  {"correct-horse-battery"},
	}
	endpoints := []string{"/setup", "/login", "/logout"}

	for _, ep := range endpoints {
		for _, tc := range cases {
			t.Run(ep+" "+tc.name, func(t *testing.T) {
				// A fresh fixture per case so a passing /setup does not close the page
				// for the next case.
				f := newTestOnboarding(t, trustnet.LoopbackOnly())
				req := httptest.NewRequest(http.MethodPost, ep, strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Host = "example.com" // matches the same-origin Origin case
				req.RemoteAddr = loopbackPeer
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				f.mux.ServeHTTP(rec, req)

				if tc.want403 && rec.Code != http.StatusForbidden {
					t.Fatalf("POST %s %s = %d, want 403", ep, tc.name, rec.Code)
				}
				if !tc.want403 && rec.Code == http.StatusForbidden {
					t.Fatalf("POST %s %s = 403, want the guard to allow it through", ep, tc.name)
				}
			})
		}
	}

	// A cross-site POST /setup must not create an admin (rejected before any state
	// change).
	t.Run("rejected POST /setup creates no admin", func(t *testing.T) {
		f := newTestOnboarding(t, trustnet.LoopbackOnly())
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.RemoteAddr = loopbackPeer
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-site POST /setup = %d, want 403", rec.Code)
		}
		if has, _ := f.svc.HasUsers(req.Context()); has {
			t.Error("cross-site POST /setup created an admin")
		}
	})
}

// TestIsSameOriginRequest unit-tests the predicate directly (the header-only
// logic, independent of any handler).
func TestIsSameOriginRequest(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		want    bool
	}{
		{name: "no headers (non-browser)", host: "example.com", want: true},
		{name: "sec-fetch same-origin", host: "example.com", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, want: true},
		{name: "sec-fetch none", host: "example.com", headers: map[string]string{"Sec-Fetch-Site": "none"}, want: true},
		{name: "sec-fetch cross-site", host: "example.com", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: false},
		{name: "sec-fetch same-site", host: "example.com", headers: map[string]string{"Sec-Fetch-Site": "same-site"}, want: false},
		{name: "sec-fetch wins over matching origin", host: "example.com", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"}, want: false},
		{name: "origin match", host: "example.com:8080", headers: map[string]string{"Origin": "http://example.com:8080"}, want: true},
		{name: "origin host mismatch", host: "example.com", headers: map[string]string{"Origin": "http://evil.example"}, want: false},
		{name: "origin port mismatch", host: "example.com:8080", headers: map[string]string{"Origin": "http://example.com:9090"}, want: false},
		{name: "unparsable origin", host: "example.com", headers: map[string]string{"Origin": "://nohost"}, want: false},
		{name: "origin with empty host", host: "example.com", headers: map[string]string{"Origin": "not-a-url"}, want: false},
		// Referer fallback (no Sec-Fetch-Site, no Origin).
		{name: "referer host match", host: "example.com", headers: map[string]string{"Referer": "http://example.com/setup"}, want: true},
		{name: "referer port match", host: "example.com:8080", headers: map[string]string{"Referer": "http://example.com:8080/login"}, want: true},
		{name: "referer host mismatch", host: "example.com", headers: map[string]string{"Referer": "http://evil.example/x"}, want: false},
		{name: "referer port mismatch", host: "example.com:8080", headers: map[string]string{"Referer": "http://example.com:9090/x"}, want: false},
		{name: "referer empty host", host: "example.com", headers: map[string]string{"Referer": "not-a-url"}, want: false},
		{name: "origin wins over referer", host: "example.com", headers: map[string]string{"Origin": "http://evil.example", "Referer": "http://example.com/x"}, want: false},
		// Case-insensitive host comparison (RFC 7230: host is case-insensitive).
		{name: "origin host uppercase allowed", host: "example.com", headers: map[string]string{"Origin": "http://Example.COM"}, want: true},
		{name: "referer host uppercase allowed", host: "example.com", headers: map[string]string{"Referer": "http://Example.COM/setup"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := isSameOriginRequest(req); got != tc.want {
				t.Errorf("isSameOriginRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsSameOriginRequestSessionFallback covers the security fix: with no
// provenance headers (no Sec-Fetch-Site, Origin, or Referer), a request
// carrying a session cookie is treated as untrusted (the CSRF bypass that used
// to allow it), while a request with no session cookie is allowed (a
// non-browser client with no CSRF vector).
func TestIsSameOriginRequestSessionFallback(t *testing.T) {
	cases := []struct {
		name   string
		cookie *http.Cookie
		want   bool
	}{
		{name: "no headers, session cookie present (the closed bypass)", cookie: &http.Cookie{Name: SessionCookieName, Value: "some-session-token"}, want: false},
		{name: "no headers, empty session cookie value", cookie: &http.Cookie{Name: SessionCookieName, Value: ""}, want: true},
		{name: "no headers, unrelated cookie only", cookie: &http.Cookie{Name: "other", Value: "x"}, want: true},
		{name: "no headers, no cookie (non-browser)", cookie: nil, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/logout", nil)
			req.Host = "example.com"
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			if got := isSameOriginRequest(req); got != tc.want {
				t.Errorf("isSameOriginRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnforceSameOriginSessionFallback proves the fallback rejection surfaces as
// a 403 through enforceSameOrigin (the single entry point wired into the
// state-changing POST handlers).
func TestEnforceSameOriginSessionFallback(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		cookie  *http.Cookie
		want403 bool
	}{
		{name: "referer match allowed", headers: map[string]string{"Referer": "http://example.com/logout"}, want403: false},
		{name: "referer cross-host rejected", headers: map[string]string{"Referer": "http://evil.example/logout"}, want403: true},
		{name: "no headers + session cookie rejected", cookie: &http.Cookie{Name: SessionCookieName, Value: "some-session-token"}, want403: true},
		{name: "no headers, no session allowed", want403: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/logout", nil)
			req.Host = "example.com"
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			rec := httptest.NewRecorder()
			ok := enforceSameOrigin(rec, req)
			if tc.want403 {
				if ok {
					t.Fatalf("enforceSameOrigin allowed %s, want rejected", tc.name)
				}
				if rec.Code != http.StatusForbidden {
					t.Fatalf("enforceSameOrigin %s = %d, want 403", tc.name, rec.Code)
				}
			} else {
				if !ok {
					t.Fatalf("enforceSameOrigin rejected %s, want allowed", tc.name)
				}
			}
		})
	}
}
