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

const (
	testAdminUser = "admin"
	testAdminPass = "correct-horse-battery"

	// testCSRFToken is a fixed CSRF value used in handler-level tests where we
	// call handleLogin directly (no GET round-trip). Must be exactly csrfTokenLen
	// (64) chars to satisfy the enforceCSRFToken length guard.
	testCSRFToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
)

// loginForm returns a url.Values suitable for a POST /login test, with a
// matching CSRF token included. Callers must also add the CSRF cookie via
// addLoginCSRF.
func loginForm(extra ...url.Values) url.Values {
	v := url.Values{
		"username":   {testAdminUser},
		"password":   {testAdminPass},
		"csrf_token": {testCSRFToken},
	}
	for _, extra := range extra {
		for k, vals := range extra {
			v[k] = vals
		}
	}
	return v
}

// addLoginCSRF adds the matching CSRF cookie to r so double-submit validation passes.
func addLoginCSRF(r *http.Request) {
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
}

// newTestAuth builds an Auth backed by a real webauth.Service over a migrated
// in-memory SQLite DB (repo rule: real SQLite, not mocks), with a single admin
// already set up and the login backoff sleep stubbed out so tests do not block.
func newTestAuth(t *testing.T, policy *trustnet.Policy) (*Auth, *webauth.Service) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	if _, err := svc.Setup(context.Background(), testAdminUser, testAdminPass); err != nil {
		t.Fatalf("Setup admin: %v", err)
	}
	// Stub the login backoff so failure-path tests do not block on real sleeps.
	return NewAuth(svc, policy, "vtest", withSleep(func(time.Duration) {})), svc
}

// loginToken performs a real login and returns the raw session token.
func loginToken(t *testing.T, svc *webauth.Service) string {
	t.Helper()
	token, err := svc.Login(context.Background(), testAdminUser, testAdminPass)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return token
}

// okHandler is a sentinel wrapped by RequireSession; a 200 body proves the
// request reached the protected handler.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("protected"))
	})
}

func TestRequireSession(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	guarded := a.RequireSession(okHandler())

	t.Run("valid session passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != "protected" {
			t.Errorf("body = %q, want protected", rec.Body.String())
		}
	})

	t.Run("missing cookie redirects to login with return path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/reports", nil)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rec.Code)
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/login?next=") {
			t.Fatalf("Location = %q, want /login?next=...", loc)
		}
		if got := mustQueryNext(t, loc); got != "/reports" {
			t.Errorf("next = %q, want /reports", got)
		}
	})

	// An invalid token and an expired token both surface as webauth.ErrInvalidSession
	// from ValidateSession, so the middleware handles them through this same branch;
	// the expiry mechanics themselves are covered by the webauth package tests.
	t.Run("invalid cookie redirects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "not-a-real-token"})
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rec.Code)
		}
	})

	t.Run("XHR gets 401 not redirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("JSON accept gets 401 not redirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestRequireSessionTrustedBypass(t *testing.T) {
	policy, err := trustnet.NewPolicy([]string{"192.0.2.0/24"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	a, _ := newTestAuth(t, policy)
	guarded := a.RequireSession(okHandler())

	t.Run("trusted IP bypasses session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.RemoteAddr = "192.0.2.50:1234" // inside the trusted CIDR, no cookie
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("trusted IP status = %d, want 200 (bypass)", rec.Code)
		}
	})

	t.Run("untrusted IP still needs session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.RemoteAddr = "198.51.100.7:1234" // outside the trusted CIDR
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("untrusted IP status = %d, want 303", rec.Code)
		}
	})
}

func TestHandleLoginSuccess(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())

	form := loginForm(url.Values{"next": {"/reports"}})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addLoginCSRF(req)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/reports" {
		t.Errorf("Location = %q, want /reports", loc)
	}
	cookie := findCookie(t, rec.Result().Cookies())
	if cookie.Value == "" {
		t.Fatal("session cookie has empty value")
	}
	if !cookie.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
	if cookie.Secure {
		t.Error("cookie Secure set on a plain-HTTP request")
	}
	wantMaxAge := int(webauth.DefaultSessionTTL.Seconds())
	if cookie.MaxAge != wantMaxAge {
		t.Errorf("cookie MaxAge = %d, want %d", cookie.MaxAge, wantMaxAge)
	}
}

func TestHandleLoginSecureCookieUnderTLS(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())

	form := loginForm()
	req := httptest.NewRequest(http.MethodPost, "https://example.test/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addLoginCSRF(req)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	cookie := findCookie(t, rec.Result().Cookies())
	if !cookie.Secure {
		t.Error("cookie Secure not set on a TLS request")
	}
}

func TestHandleLoginSecureCookieBehindTrustedProxy(t *testing.T) {
	policy, err := trustnet.NewPolicy(nil, []string{"192.0.2.0/24"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	a, _ := newTestAuth(t, policy)

	form := loginForm()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.9:443" // a trusted proxy terminated TLS upstream
	req.Header.Set("X-Forwarded-Proto", "https")
	addLoginCSRF(req)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	cookie := findCookie(t, rec.Result().Cookies())
	if !cookie.Secure {
		t.Error("cookie Secure not set when a trusted proxy reports X-Forwarded-Proto: https")
	}

	// A spoofed XFP from an UNtrusted peer must NOT flip Secure.
	req2 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "198.51.100.4:443" // not a trusted proxy
	req2.Header.Set("X-Forwarded-Proto", "https")
	addLoginCSRF(req2)
	rec2 := httptest.NewRecorder()
	a.handleLogin(rec2, req2)
	if findCookie(t, rec2.Result().Cookies()).Secure {
		t.Error("cookie Secure set from a spoofed X-Forwarded-Proto on an untrusted peer")
	}
}

func TestHandleLoginFailureEnumerationSafe(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())

	cases := []struct {
		name, user, pass string
	}{
		{"wrong password", testAdminUser, "wrong-password"},
		{"unknown user", "ghost", "wrong-password"},
	}
	var statuses []int
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"username": {tc.user}, "password": {tc.pass}, "csrf_token": {testCSRFToken}}
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = "203.0.113.1:5000" // distinct IP per subtest to avoid cross-lockout
			if tc.name == "unknown user" {
				req.RemoteAddr = "203.0.113.2:5000"
			}
			addLoginCSRF(req)
			rec := httptest.NewRecorder()
			a.handleLogin(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), defaultLoginError) {
				t.Errorf("body missing generic error %q", defaultLoginError)
			}
			// No session cookie should be set on a failed login.
			if c := findCookieOptional(rec.Result().Cookies()); c != nil {
				t.Error("session cookie set on a failed login")
			}
			statuses = append(statuses, rec.Code)
		})
	}
	if statuses[0] != statuses[1] {
		t.Errorf("status differs between wrong-password (%d) and unknown-user (%d): enumeration oracle", statuses[0], statuses[1])
	}
}

func TestHandleLoginLockout(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	const peer = "203.0.113.55:7000"

	form := url.Values{"username": {testAdminUser}, "password": {"wrong"}, "csrf_token": {testCSRFToken}}
	for i := 0; i < defaultMaxFailures; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = peer
		addLoginCSRF(req)
		a.handleLogin(httptest.NewRecorder(), req)
	}

	// The next attempt from the same IP is hard-locked.
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = peer
	addLoginCSRF(req)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after %d failures", rec.Code, defaultMaxFailures)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
}

func TestHandleLoginLockoutResetsOnSuccess(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	const peer = "203.0.113.77:8000"

	// A handful of failures, short of the hard lockout.
	bad := url.Values{"username": {testAdminUser}, "password": {"wrong"}, "csrf_token": {testCSRFToken}}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(bad.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = peer
		addLoginCSRF(req)
		a.handleLogin(httptest.NewRecorder(), req)
	}

	// A success clears the counter.
	good := loginForm()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(good.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = peer
	addLoginCSRF(req)
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login after failures status = %d, want 303", rec.Code)
	}

	// Now even many failures stay throttled-but-allowed (never instantly 429),
	// proving the counter reset rather than carrying over.
	for i := 0; i < defaultMaxFailures-1; i++ {
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(bad.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = peer
		addLoginCSRF(r)
		rr := httptest.NewRecorder()
		a.handleLogin(rr, r)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("locked out after %d post-reset failures, counter did not reset", i+1)
		}
	}
}

func TestHandleLogoutRevokesAndClears(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	token := loginToken(t, svc)

	// Sanity: the session is valid before logout.
	if _, err := svc.ValidateSession(context.Background(), token); err != nil {
		t.Fatalf("session invalid before logout: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	// A real browser logout carries Fetch Metadata; without a provenance header a
	// session-bearing POST is now refused as a CSRF (see internal/web/csrf.go).
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	a.handleLogout(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
	// Server-side revocation: the token no longer validates.
	if _, err := svc.ValidateSession(context.Background(), token); err == nil {
		t.Error("session still valid after logout (not revoked server-side)")
	}
	// Cookie cleared (negative Max-Age).
	cookie := findCookie(t, rec.Result().Cookies())
	if cookie.MaxAge >= 0 {
		t.Errorf("logout cookie MaxAge = %d, want negative (cleared)", cookie.MaxAge)
	}
}

// TestUIWithAuthRoutes exercises the full mux wiring: WithAuth gates the page
// routes and registers the login/logout endpoints. It covers the GET /login
// form render and the already-authenticated redirect that the handler-level
// tests do not.
func TestUIWithAuthRoutes(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a)).Register(mux)

	t.Run("GET /login renders the form", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.RemoteAddr = "198.51.100.20:1" // untrusted so it does not bypass
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /login = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"Sign in", `action="/login"`, `name="password"`, "vtest"} {
			if !strings.Contains(body, want) {
				t.Errorf("login page missing %q", want)
			}
		}
	})

	t.Run("guarded page redirects when unauthenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.RemoteAddr = "198.51.100.21:1"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET /settings unauthenticated = %d, want 303", rec.Code)
		}
	})

	t.Run("guarded page renders with a valid session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.RemoteAddr = "198.51.100.22:1"
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /settings authenticated = %d, want 200", rec.Code)
		}
	})

	t.Run("GET /login with a valid session redirects away", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/login?next=/reports", nil)
		req.RemoteAddr = "198.51.100.23:1"
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET /login authenticated = %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/reports" {
			t.Errorf("Location = %q, want /reports", loc)
		}
	})
}

// TestClientKeyNilPolicyStripsPort verifies that the lockout key is stable for
// the same IP regardless of the ephemeral source port when no proxy policy is
// set. Without port-stripping an attacker can bypass the login lockout simply by
// reconnecting (new ephemeral port -> different key -> counter resets).
func TestClientKeyNilPolicyStripsPort(t *testing.T) {
	// Auth with NO policy: clientKey must fall back to bare-IP extraction.
	a, _ := newTestAuth(t, nil)

	req1 := httptest.NewRequest(http.MethodPost, "/login", nil)
	req1.RemoteAddr = "203.0.113.9:51000"
	req2 := httptest.NewRequest(http.MethodPost, "/login", nil)
	req2.RemoteAddr = "203.0.113.9:51001"

	key1 := a.clientKey(req1)
	key2 := a.clientKey(req2)
	if key1 != key2 {
		t.Errorf("clientKey mismatch across ports: %q vs %q (lockout bypass possible)", key1, key2)
	}
	if key1 != "203.0.113.9" {
		t.Errorf("clientKey = %q, want bare IP %q", key1, "203.0.113.9")
	}
}

func TestSafeNext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/reports", "/reports"},
		{"/config?x=1", "/config?x=1"},
		{"", "/dashboard"},
		{"//evil.example", "/dashboard"},
		{"/\\evil.example", "/dashboard"},
		{"https://evil.example/path", "/dashboard"},
		{"http://evil.example", "/dashboard"},
		{"javascript:alert(1)", "/dashboard"},
		{"  /reports  ", "/reports"},
		{"/path\r\nSet-Cookie: x", "/dashboard"},
	}
	for _, tc := range cases {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- helpers ---

func findCookie(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	if c := findCookieOptional(cookies); c != nil {
		return c
	}
	t.Fatalf("cookie %q not found", SessionCookieName)
	return nil
}

func findCookieOptional(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Name == SessionCookieName {
			return c
		}
	}
	return nil
}

func mustQueryNext(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	return u.Query().Get("next")
}
