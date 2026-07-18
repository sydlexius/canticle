package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/webauth"
)

// sameOriginGuardCSRFToken is the fixed CSRF value injected into
// TestSameOriginGuard requests for /login and /setup. The cross-site cases are
// rejected by the same-origin check before CSRF runs; the same-origin "pass"
// cases need a valid double-submit token so they are not rejected by CSRF instead.
// Must be exactly csrfTokenLen (64) chars to satisfy the length guard.
const sameOriginGuardCSRFToken = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

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

	// /login and /setup require a matching CSRF cookie+field in the request after
	// same-origin passes; cross-site cases are 403 before CSRF is checked. For
	// /logout no CSRF token is needed (logout has no CSRF validation beyond same-origin).
	endpoints := []string{"/setup", "/login", "/logout"}

	for _, ep := range endpoints {
		for _, tc := range cases {
			ep, tc := ep, tc
			t.Run(ep+" "+tc.name, func(t *testing.T) {
				// A fresh fixture per case so a passing /setup does not close the page
				// for the next case.
				f := newTestOnboarding(t, trustnet.LoopbackOnly())

				form := url.Values{
					"username": {"admin"},
					"password": {"correct-horse-battery"},
					"confirm":  {"correct-horse-battery"},
				}
				if ep != "/logout" {
					form.Set("csrf_token", sameOriginGuardCSRFToken)
				}
				req := httptest.NewRequest(http.MethodPost, ep, strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Host = "example.com" // matches the same-origin Origin case
				req.RemoteAddr = loopbackPeer
				if ep != "/logout" {
					req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: sameOriginGuardCSRFToken})
				}
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

	// A cross-site POST /setup must not create an admin: rejected by the
	// same-origin check before CSRF or any state change runs.
	t.Run("rejected POST /setup creates no admin", func(t *testing.T) {
		crossSiteForm := url.Values{
			"username": {"admin"},
			"password": {"correct-horse-battery"},
			"confirm":  {"correct-horse-battery"},
		}
		f := newTestOnboarding(t, trustnet.LoopbackOnly())
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(crossSiteForm.Encode()))
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

// newTestAuthForCSRF builds an Auth over a real webauth.Service for CSRF tests.
func newTestAuthForCSRF(t *testing.T) *Auth {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "csrf.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	if _, err := svc.Setup(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup admin: %v", err)
	}
	return NewAuth(svc, trustnet.LoopbackOnly(), "vtest", withSleep(func(time.Duration) {}))
}

// TestCSRFTokenGETSetsTokenAndField verifies that GET /login and GET /setup
// (a) set the CSRF cookie and (b) embed the same value as a hidden form field.
func TestCSRFTokenGETSetsTokenAndField(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())

	for _, path := range []string{"/login", "/setup"} {
		t.Run("GET "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = loopbackPeer
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}

			// CSRF cookie must be present.
			var csrfCookie *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == CSRFCookieName {
					csrfCookie = c
					break
				}
			}
			if csrfCookie == nil {
				t.Fatalf("GET %s: no %s cookie in response", path, CSRFCookieName)
			}
			if csrfCookie.Value == "" {
				t.Errorf("GET %s: CSRF cookie value is empty", path)
			}

			// The hidden form field must carry the same token.
			body := rec.Body.String()
			wantField := `name="csrf_token" value="` + csrfCookie.Value + `"`
			if !strings.Contains(body, wantField) {
				t.Errorf("GET %s: body missing hidden CSRF field matching cookie value %q", path, csrfCookie.Value)
			}
		})
	}
}

// TestCSRFTokenCookieAttributes verifies the CSRF cookie security attributes.
func TestCSRFTokenCookieAttributes(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = loopbackPeer
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			csrfCookie = c
			break
		}
	}
	if csrfCookie == nil {
		t.Fatalf("no %s cookie in GET /login response", CSRFCookieName)
	}
	if !csrfCookie.HttpOnly {
		t.Error("CSRF cookie HttpOnly = false, want true")
	}
	if csrfCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("CSRF cookie SameSite = %v, want Lax", csrfCookie.SameSite)
	}
	if csrfCookie.Path != "/" {
		t.Errorf("CSRF cookie Path = %q, want /", csrfCookie.Path)
	}
	if csrfCookie.Secure {
		t.Error("CSRF cookie Secure = true on a plain-HTTP request, want false")
	}
	if csrfCookie.MaxAge <= 0 {
		t.Errorf("CSRF cookie MaxAge = %d, want positive", csrfCookie.MaxAge)
	}
}

// TestCSRFTokenSecureUnderTLS verifies the CSRF cookie sets Secure on a TLS request.
func TestCSRFTokenSecureUnderTLS(t *testing.T) {
	a := newTestAuthForCSRF(t)
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a)).Register(mux)

	req := httptest.NewRequest(http.MethodGet, "https://example.test/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			if !c.Secure {
				t.Error("CSRF cookie Secure = false on TLS request, want true")
			}
			return
		}
	}
	t.Fatalf("no %s cookie in GET /login response over TLS", CSRFCookieName)
}

// TestCSRFTokenPostLoginRejectsInvalidToken covers all CSRF rejection paths for
// POST /login: missing cookie, missing field, wrong-length field, and mismatched
// token all return 403 without attempting authentication or feeding the rate
// limiter. A token submitted via URL query param (not POST body) is also rejected.
func TestCSRFTokenPostLoginRejectsInvalidToken(t *testing.T) {
	// Must be exactly csrfTokenLen (64) chars to satisfy the length guard.
	const validToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

	cases := []struct {
		name       string
		cookie     string // empty = no CSRF cookie
		field      string // empty = no csrf_token field
		wantForbid bool
	}{
		{"missing cookie and field", "", "", true},
		{"missing cookie only", "", validToken, true},
		{"missing field only", validToken, "", true},
		{"wrong-length field", validToken, "short", true},
		{"mismatched token", "cookie-value", "different-field-value", true},
		{"matching token passes", validToken, validToken, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAuthForCSRF(t)

			form := url.Values{
				"username": {"admin"},
				"password": {"correct-horse-battery"},
			}
			if tc.field != "" {
				form.Set("csrf_token", tc.field)
			}
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			a.handleLogin(rec, req)

			if tc.wantForbid {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				// Must not set a session cookie on CSRF rejection.
				for _, c := range rec.Result().Cookies() {
					if c.Name == SessionCookieName {
						t.Error("session cookie issued on CSRF-rejected login")
					}
				}
				// Must not have touched the rate limiter: the counter is zero, so if
				// a subsequent valid request were rate-limited it would be a bug.
			} else {
				if rec.Code != http.StatusSeeOther {
					t.Fatalf("valid CSRF status = %d, want 303", rec.Code)
				}
			}
		})
	}
}

// TestCSRFTokenPostSetupRejectsInvalidToken verifies that POST /setup is
// rejected with 403 on missing/mismatched CSRF without creating an admin.
func TestCSRFTokenPostSetupRejectsInvalidToken(t *testing.T) {
	// Must be exactly csrfTokenLen (64) chars to satisfy the length guard.
	const validToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

	cases := []struct {
		name       string
		cookie     string
		field      string
		wantForbid bool
	}{
		{"missing cookie and field", "", "", true},
		{"missing cookie only", "", validToken, true},
		{"missing field only", validToken, "", true},
		{"wrong-length field", validToken, "short", true},
		{"mismatched token", "cookie-value", "other-field-value", true},
		{"matching token passes", validToken, validToken, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestOnboarding(t, trustnet.LoopbackOnly())

			form := url.Values{
				"username": {"admin"},
				"password": {"correct-horse-battery"},
				"confirm":  {"correct-horse-battery"},
			}
			if tc.field != "" {
				form.Set("csrf_token", tc.field)
			}
			req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = loopbackPeer
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)

			if tc.wantForbid {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				// Must not create an admin on CSRF rejection.
				if has, _ := f.svc.HasUsers(req.Context()); has {
					t.Error("admin created despite CSRF rejection")
				}
			} else {
				// A valid CSRF + valid form should succeed and redirect to /settings.
				if rec.Code != http.StatusSeeOther {
					t.Fatalf("valid CSRF status = %d, want 303", rec.Code)
				}
				if loc := rec.Header().Get("Location"); loc != "/settings" {
					t.Errorf("Location = %q, want /settings", loc)
				}
			}
		})
	}
}

// TestCSRFTokenDoesNotFeedRateLimiter proves that CSRF-rejected login attempts
// do not consume rate-limiter capacity (the check fires before the limiter).
func TestCSRFTokenDoesNotFeedRateLimiter(t *testing.T) {
	a := newTestAuthForCSRF(t)
	const peer = "203.0.113.99:9000"

	// Fire many CSRF-invalid requests (well above the lockout threshold).
	badForm := url.Values{"username": {"admin"}, "password": {"wrong"}}
	for i := 0; i < defaultMaxFailures+5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(badForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = peer
		// No CSRF cookie -> 403 before the rate limiter.
		a.handleLogin(httptest.NewRecorder(), req)
	}

	// A valid CSRF + valid credentials from the same IP must still succeed (not 429).
	good := url.Values{
		"username":   {"admin"},
		"password":   {"correct-horse-battery"},
		"csrf_token": {testCSRFToken},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(good.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = peer
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	if rec.Code == http.StatusTooManyRequests {
		t.Error("CSRF-rejected attempts fed the rate limiter; valid login was locked out")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid login after CSRF-only failures = %d, want 303", rec.Code)
	}
}

// TestCSRFTokenQueryParamRejected proves that a CSRF token supplied only as a
// URL query parameter (not in the POST body) is rejected. enforceCSRFToken uses
// PostFormValue, which reads only the request body, not the URL query string.
func TestCSRFTokenQueryParamRejected(t *testing.T) {
	a := newTestAuthForCSRF(t)
	// A real-length token so the length guard is not what fires; the rejection
	// must come from PostFormValue returning "" (query param ignored in body read).
	token := testCSRFToken

	req := httptest.NewRequest(http.MethodPost, "/login?csrf_token="+token, nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: token})
	// POST body is empty: no csrf_token in the body.
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("CSRF token in URL query param = %d, want 403 (PostFormValue must not read query string)", rec.Code)
	}
}

// TestCSRFTokenGETReusesExistingCookie proves the multi-tab fix: a GET /login
// or GET /setup request that carries a valid mxlrc_csrf cookie must reuse the
// existing token (return it in the hidden field, set no new cookie) so a second
// tab opened while the first is still open does not invalidate the first tab's
// token.
func TestCSRFTokenGETReusesExistingCookie(t *testing.T) {
	for _, path := range []string{"/login", "/setup"} {
		t.Run("GET "+path, func(t *testing.T) {
			f := newTestOnboarding(t, trustnet.LoopbackOnly())

			// First GET: no cookie -> server mints a fresh token and sets the cookie.
			req1 := httptest.NewRequest(http.MethodGet, path, nil)
			req1.RemoteAddr = loopbackPeer
			rec1 := httptest.NewRecorder()
			f.mux.ServeHTTP(rec1, req1)
			if rec1.Code != http.StatusOK {
				t.Fatalf("first GET %s = %d, want 200", path, rec1.Code)
			}
			var originalToken string
			for _, c := range rec1.Result().Cookies() {
				if c.Name == CSRFCookieName {
					originalToken = c.Value
					break
				}
			}
			if originalToken == "" {
				t.Fatalf("first GET %s: no CSRF cookie set", path)
			}

			// Second GET: send the existing cookie. The server must NOT overwrite it.
			req2 := httptest.NewRequest(http.MethodGet, path, nil)
			req2.RemoteAddr = loopbackPeer
			req2.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: originalToken})
			rec2 := httptest.NewRecorder()
			f.mux.ServeHTTP(rec2, req2)
			if rec2.Code != http.StatusOK {
				t.Fatalf("second GET %s = %d, want 200", path, rec2.Code)
			}

			// No new CSRF cookie must appear in the second response.
			for _, c := range rec2.Result().Cookies() {
				if c.Name == CSRFCookieName {
					t.Errorf("second GET %s set a new CSRF cookie (%q), want reuse of existing (%q)",
						path, c.Value, originalToken)
				}
			}

			// The hidden form field must carry the original token.
			wantField := `name="csrf_token" value="` + originalToken + `"`
			if !strings.Contains(rec2.Body.String(), wantField) {
				t.Errorf("second GET %s: hidden CSRF field does not match original token", path)
			}
		})
	}
}
