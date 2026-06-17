package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
	"github.com/sydlexius/mxlrcgo-svc/internal/webauth"
)

const loopbackPeer = "127.0.0.1:5500"

// onboardingFixture bundles a full UI mux wired with auth + onboarding over a
// real migrated in-memory SQLite DB (repo rule: real SQLite, not mocks), plus
// the underlying service and secret store for assertions.
type onboardingFixture struct {
	mux   *http.ServeMux
	svc   *webauth.Service
	store *secrets.SQLStore
}

// newTestOnboarding builds the fixture with NO admin yet (first-run state). The
// secret store uses a fixed 32-byte test key; the login backoff is stubbed.
func newTestOnboarding(t *testing.T, policy *trustnet.Policy) onboardingFixture {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "onboard.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	store := secrets.NewSQLStore(sqlDB, bytes.Repeat([]byte{0x42}, 32))
	auth := NewAuth(svc, policy, "vtest", withSleep(func(time.Duration) {}))
	onb := NewOnboarding(svc, store, auth, policy, "vtest")

	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(auth), WithOnboarding(onb)).Register(mux)
	return onboardingFixture{mux: mux, svc: svc, store: store}
}

// testCSRFSetupToken is the fixed CSRF value used by postSetup. Must be exactly
// csrfTokenLen (64) chars to satisfy the enforceCSRFToken length guard.
const testCSRFSetupToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

// postSetup submits the onboarding form from the given peer with the given form
// values (plus a matching CSRF cookie+field) and returns the recorder.
func postSetup(t *testing.T, mux *http.ServeMux, peer string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	// Clone to avoid mutating the caller's values.
	v := make(url.Values)
	for k, vals := range form {
		v[k] = vals
	}
	v.Set("csrf_token", testCSRFSetupToken)
	req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = peer
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFSetupToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestFirstRunRedirectsToSetup proves the UI page routes redirect to /setup
// while no admin exists, then serve (or session-gate) once one does.
func TestFirstRunRedirectsToSetup(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())

	for _, path := range []string{"/", "/config", "/reports"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.9:1" // untrusted: must still be sent to setup, not served
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET %s first-run = %d, want 303", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/setup" {
			t.Errorf("GET %s first-run Location = %q, want /setup", path, loc)
		}
	}

	// Once an admin exists, the first-run redirect stops and the normal session
	// gate applies (unauthenticated -> /login, not /setup).
	if _, err := f.svc.Setup(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.RemoteAddr = "198.51.100.9:1"
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /config post-admin = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("GET /config post-admin Location = %q, want /login...", loc)
	}
}

// TestSetupRendersTokenHelpLink verifies the onboarding form carries the
// "how to obtain a Musixmatch token" help link, opening the published docs in a
// new tab with a safe rel (issue #204, lane 4 UAT).
func TestSetupRendersTokenHelpLink(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	req.RemoteAddr = loopbackPeer
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"https://sydlexius.github.io/mxlrcgo-svc/GETTING_STARTED/#get-a-musixmatch-token",
		`target="_blank"`,
		`rel="noopener noreferrer"`,
		"How do I get a token?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing token-help fragment %q", want)
		}
	}
}

// TestSetupGating covers who may reach /setup: loopback (and configured CIDRs)
// may; everyone else gets 404; a spoofed X-Forwarded-For cannot forge trust.
func TestSetupGating(t *testing.T) {
	// Allowlist a CIDR but trust NO proxies, so XFF is always ignored.
	policy, err := trustnet.NewPolicy([]string{"192.0.2.0/24"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	f := newTestOnboarding(t, policy)

	t.Run("loopback allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = loopbackPeer
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /setup from loopback = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Create the admin account") {
			t.Error("setup page body missing expected heading")
		}
	})

	t.Run("trusted CIDR allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = "192.0.2.40:1"
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /setup from trusted CIDR = %d, want 200", rec.Code)
		}
	})

	t.Run("untrusted refused with 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = "198.51.100.7:1"
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /setup from untrusted = %d, want 404", rec.Code)
		}
	})

	t.Run("spoofed XFF cannot forge trust", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = "198.51.100.8:1"               // untrusted peer
		req.Header.Set("X-Forwarded-For", "192.0.2.40") // forged trusted source
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /setup with spoofed XFF = %d, want 404 (header ignored)", rec.Code)
		}
	})

	t.Run("POST from untrusted refused with 404", func(t *testing.T) {
		form := url.Values{"username": {"x"}, "password": {"correct-horse"}, "confirm": {"correct-horse"}}
		rec := postSetup(t, f.mux, "198.51.100.9:1", form)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST /setup from untrusted = %d, want 404", rec.Code)
		}
		// And it must not have created an admin.
		if has, _ := f.svc.HasUsers(context.Background()); has {
			t.Error("untrusted POST created an admin")
		}
	})
}

// TestSetupCreatesAdminAndSecrets is the happy path: a valid submission creates
// the admin, writes the optional secrets under the correct names, issues a
// session cookie, and redirects into the UI.
func TestSetupCreatesAdminAndSecrets(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())

	form := url.Values{
		"username":         {"admin"},
		"password":         {"correct-horse-battery"},
		"confirm":          {"correct-horse-battery"},
		"musixmatch_token": {"mxtok-123"},
		"webhook_api_key":  {"mxlrc_hook-456"},
	}
	rec := postSetup(t, f.mux, loopbackPeer, form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/config" {
		t.Errorf("Location = %q, want /config", loc)
	}

	// A session cookie was issued so onboarding flows straight into the UI.
	cookie := findCookie(t, rec.Result().Cookies())
	if cookie.Value == "" {
		t.Fatal("no session cookie issued after setup")
	}
	if _, err := f.svc.ValidateSession(context.Background(), cookie.Value); err != nil {
		t.Errorf("issued session does not validate: %v", err)
	}

	// Admin exists and the credentials work.
	if _, err := f.svc.Login(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Errorf("login with new admin creds failed: %v", err)
	}

	// Secrets were written under the canonical names.
	if v, ok, err := f.store.Get(context.Background(), secrets.NameMusixmatchToken); err != nil || !ok || v != "mxtok-123" {
		t.Errorf("musixmatch secret = (%q, %v, %v), want (mxtok-123, true, nil)", v, ok, err)
	}
	if v, ok, err := f.store.Get(context.Background(), secrets.NameWebhookAPIKey); err != nil || !ok || v != "mxlrc_hook-456" {
		t.Errorf("webhook secret = (%q, %v, %v), want (mxlrc_hook-456, true, nil)", v, ok, err)
	}
}

// TestSetupBlankSecretsLeaveStoreUntouched proves a blank optional field is
// skipped (it never overwrites, and never writes an empty value).
func TestSetupBlankSecretsLeaveStoreUntouched(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())
	// Pre-existing webhook secret that must survive an onboarding with a blank field.
	if err := f.store.Set(context.Background(), secrets.NameWebhookAPIKey, "preexisting"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	form := url.Values{
		"username": {"admin"},
		"password": {"correct-horse-battery"},
		"confirm":  {"correct-horse-battery"},
		// no secret fields submitted
	}
	rec := postSetup(t, f.mux, loopbackPeer, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup = %d, want 303", rec.Code)
	}

	// The pre-existing webhook secret is untouched...
	if v, ok, _ := f.store.Get(context.Background(), secrets.NameWebhookAPIKey); !ok || v != "preexisting" {
		t.Errorf("webhook secret = (%q, %v), want (preexisting, true)", v, ok)
	}
	// ...and no musixmatch secret was written.
	if _, ok, _ := f.store.Get(context.Background(), secrets.NameMusixmatchToken); ok {
		t.Error("musixmatch secret written from a blank field")
	}
}

// TestSetupValidation covers the field-level rejections: mismatch, too-short,
// missing username. Each re-renders the form (not a redirect) and creates no admin.
func TestSetupValidation(t *testing.T) {
	cases := []struct {
		name       string
		form       url.Values
		wantInBody string
	}{
		{
			name:       "password mismatch",
			form:       url.Values{"username": {"admin"}, "password": {"correct-horse"}, "confirm": {"different-horse"}},
			wantInBody: "Passwords do not match",
		},
		{
			name:       "password too short",
			form:       url.Values{"username": {"admin"}, "password": {"short"}, "confirm": {"short"}},
			wantInBody: "at least 8 characters",
		},
		{
			name:       "missing username",
			form:       url.Values{"username": {"  "}, "password": {"correct-horse"}, "confirm": {"correct-horse"}},
			wantInBody: "Username is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestOnboarding(t, trustnet.LoopbackOnly())
			rec := postSetup(t, f.mux, loopbackPeer, tc.form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantInBody) {
				t.Errorf("body missing %q; got %q", tc.wantInBody, rec.Body.String())
			}
			if has, _ := f.svc.HasUsers(context.Background()); has {
				t.Error("invalid submission created an admin")
			}
			// No cookie on a rejected submission.
			if c := findCookieOptional(rec.Result().Cookies()); c != nil {
				t.Error("session cookie set on a rejected submission")
			}
		})
	}
}

// TestSetupClosedAfterAdminExists proves the page is one-shot: once an admin
// exists, both GET and a second POST redirect to /login and never create a
// second admin (race-safe double-setup rejection).
func TestSetupClosedAfterAdminExists(t *testing.T) {
	f := newTestOnboarding(t, trustnet.LoopbackOnly())
	if _, err := f.svc.Setup(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	t.Run("GET /setup redirects to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = loopbackPeer
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET /setup post-admin = %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want /login", loc)
		}
	})

	t.Run("second POST does not create a second admin", func(t *testing.T) {
		form := url.Values{"username": {"intruder"}, "password": {"another-password"}, "confirm": {"another-password"}}
		rec := postSetup(t, f.mux, loopbackPeer, form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("second POST /setup = %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want /login", loc)
		}
		// The original admin is intact and the intruder was never created.
		if _, err := f.svc.Login(context.Background(), "admin", "correct-horse-battery"); err != nil {
			t.Errorf("original admin login broken after second setup: %v", err)
		}
		if _, err := f.svc.Login(context.Background(), "intruder", "another-password"); err == nil {
			t.Error("intruder admin was created by a second setup")
		}
	})
}

// --- error-branch coverage via a fake service ---

// fakeOnboardingService injects deterministic errors/results so the handler's
// error branches (which a real, healthy service never reaches) can be covered.
type fakeOnboardingService struct {
	hasUsers    bool
	hasUsersErr error
	setupErr    error
	loginErr    error
}

func (f fakeOnboardingService) HasUsers(context.Context) (bool, error) {
	return f.hasUsers, f.hasUsersErr
}

func (f fakeOnboardingService) Setup(context.Context, string, string) (webauth.User, error) {
	return webauth.User{}, f.setupErr
}

func (f fakeOnboardingService) Login(context.Context, string, string) (string, error) {
	if f.loginErr != nil {
		return "", f.loginErr
	}
	return "tok", nil
}

// failingSecretSetter always errors, to cover the writeSecrets error-log branch.
type failingSecretSetter struct{}

func (failingSecretSetter) Set(context.Context, string, string) error {
	return errFakeSecret
}

var errFakeSecret = errFake("secret store unavailable")

type errFake string

func (e errFake) Error() string { return string(e) }

// newFakeOnboarding builds an Onboarding over a fake service (loopback policy)
// plus a real Auth (its cookie issuance is exercised on the success path).
func newFakeOnboarding(t *testing.T, svc fakeOnboardingService, setter SecretSetter) *Onboarding {
	t.Helper()
	// Auth needs a SessionService; the fake satisfies Login/ValidateSession/Logout
	// is not required here because we only call setSessionCookie via the success
	// path, so a minimal real Auth over the fake's Login is enough.
	auth := NewAuth(fakeSessionService{}, trustnet.LoopbackOnly(), "vtest", withSleep(func(time.Duration) {}))
	return NewOnboarding(svc, setter, auth, trustnet.LoopbackOnly(), "vtest")
}

// fakeSessionService is a no-op SessionService for the Auth backing the fake
// onboarding (the onboarding handler uses its own service for Login/Setup).
type fakeSessionService struct{}

func (fakeSessionService) Login(context.Context, string, string) (string, error) {
	return "tok", nil
}
func (fakeSessionService) ValidateSession(context.Context, string) (*webauth.User, error) {
	return &webauth.User{}, nil
}
func (fakeSessionService) Logout(context.Context, string) error { return nil }

func TestSetupHasUsersErrorReturns500(t *testing.T) {
	onb := newFakeOnboarding(t, fakeOnboardingService{hasUsersErr: errFake("db down")}, nil)

	t.Run("GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = loopbackPeer
		rec := httptest.NewRecorder()
		onb.handleSetupForm(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("GET /setup HasUsers error = %d, want 500", rec.Code)
		}
	})

	t.Run("POST", func(t *testing.T) {
		const csrfToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
		form := url.Values{"csrf_token": {csrfToken}}
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = loopbackPeer
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfToken})
		rec := httptest.NewRecorder()
		onb.handleSetup(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("POST /setup HasUsers error = %d, want 500", rec.Code)
		}
	})

	t.Run("FirstRunGate", func(t *testing.T) {
		gate := onb.FirstRunGate(okHandler())
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.RemoteAddr = loopbackPeer
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("FirstRunGate HasUsers error = %d, want 500", rec.Code)
		}
	})
}

// postFakeSetup submits a valid form directly to a fake-backed onboarding
// handler, including a matching CSRF cookie+field so the double-submit check passes.
func postFakeSetup(onb *Onboarding) *httptest.ResponseRecorder {
	const csrfToken = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	form := url.Values{
		"username":         {"admin"},
		"password":         {"correct-horse-battery"},
		"confirm":          {"correct-horse-battery"},
		"musixmatch_token": {"tok"},
		"csrf_token":       {csrfToken},
	}
	req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = loopbackPeer
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfToken})
	rec := httptest.NewRecorder()
	onb.handleSetup(rec, req)
	return rec
}

func TestSetupServiceErrorBranches(t *testing.T) {
	t.Run("Setup ErrUserExists redirects to login", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{setupErr: webauth.ErrUserExists}, nil)
		rec := postFakeSetup(onb)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Fatalf("ErrUserExists -> %d %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("Setup ErrUserExists latches adminExists", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{setupErr: webauth.ErrUserExists}, nil)
		postFakeSetup(onb)
		if !onb.adminExists.Load() {
			t.Fatal("ErrUserExists race path did not set adminExists latch")
		}
	})

	t.Run("Setup ErrPasswordTooShort re-renders 400", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{setupErr: webauth.ErrPasswordTooShort}, nil)
		rec := postFakeSetup(onb)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("ErrPasswordTooShort -> %d, want 400", rec.Code)
		}
	})

	t.Run("Setup generic error renders 500", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{setupErr: errFake("disk full")}, nil)
		rec := postFakeSetup(onb)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("generic Setup error -> %d, want 500", rec.Code)
		}
	})

	t.Run("auto-login failure falls back to /login", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{loginErr: errFake("login broke")}, nil)
		rec := postFakeSetup(onb)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Fatalf("auto-login failure -> %d %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
		}
	})
}

func TestWriteSecretsBranches(t *testing.T) {
	t.Run("nil store with submitted fields warns and proceeds", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{}, nil) // nil setter
		rec := postFakeSetup(onb)
		// Admin created + auto-login succeeds -> redirect to /config.
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/config" {
			t.Fatalf("nil-store success -> %d %q, want 303 /config", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("secret store error is logged but does not fail setup", func(t *testing.T) {
		onb := newFakeOnboarding(t, fakeOnboardingService{}, failingSecretSetter{})
		rec := postFakeSetup(onb)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/config" {
			t.Fatalf("secret-error success -> %d %q, want 303 /config", rec.Code, rec.Header().Get("Location"))
		}
	})
}

// countingOnboardingService counts HasUsers calls so the latch (FIX-C) can be
// asserted: once an admin exists, the per-request gate must stop querying.
type countingOnboardingService struct {
	mu       sync.Mutex
	calls    int
	hasUsers bool
}

func (c *countingOnboardingService) HasUsers(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.hasUsers, nil
}

func (c *countingOnboardingService) Setup(context.Context, string, string) (webauth.User, error) {
	return webauth.User{}, nil
}

func (c *countingOnboardingService) Login(context.Context, string, string) (string, error) {
	return "tok", nil
}

func (c *countingOnboardingService) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestHasUsersLatchStopsQuerying proves the admin-exists latch (FIX-C): once
// HasUsers reports true, the per-request gate caches it and never queries the
// service again, while the pre-admin path keeps querying until the first admin
// appears.
func TestHasUsersLatchStopsQuerying(t *testing.T) {
	t.Run("queries every request until an admin exists", func(t *testing.T) {
		svc := &countingOnboardingService{hasUsers: false}
		auth := NewAuth(fakeSessionService{}, trustnet.LoopbackOnly(), "vtest", withSleep(func(time.Duration) {}))
		onb := NewOnboarding(svc, nil, auth, trustnet.LoopbackOnly(), "vtest")
		gate := onb.FirstRunGate(okHandler())

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, "/config", nil)
			req.RemoteAddr = loopbackPeer
			gate.ServeHTTP(httptest.NewRecorder(), req)
		}
		if got := svc.count(); got != 3 {
			t.Fatalf("pre-admin HasUsers calls = %d, want 3 (no caching of a false result)", got)
		}
	})

	t.Run("stops querying once latched", func(t *testing.T) {
		svc := &countingOnboardingService{hasUsers: true}
		auth := NewAuth(fakeSessionService{}, trustnet.LoopbackOnly(), "vtest", withSleep(func(time.Duration) {}))
		onb := NewOnboarding(svc, nil, auth, trustnet.LoopbackOnly(), "vtest")
		gate := onb.FirstRunGate(okHandler())

		for i := 0; i < 5; i++ {
			req := httptest.NewRequest(http.MethodGet, "/config", nil)
			req.RemoteAddr = loopbackPeer
			gate.ServeHTTP(httptest.NewRecorder(), req)
		}
		if got := svc.count(); got != 1 {
			t.Fatalf("post-latch HasUsers calls = %d, want 1 (query runs once, then the latch caches)", got)
		}
	})
}

// TestNewOnboardingPanicsOnNil verifies the constructor guards its required deps.
func TestNewOnboardingPanicsOnNil(t *testing.T) {
	t.Run("nil service", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on nil service")
			}
		}()
		NewOnboarding(nil, nil, &Auth{}, trustnet.LoopbackOnly(), "v")
	})
	t.Run("nil auth", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on nil auth")
			}
		}()
		NewOnboarding(fakeOnboardingService{}, nil, nil, trustnet.LoopbackOnly(), "v")
	})
}

// TestNewOnboardingDefaultsNilPolicyToLoopback verifies a nil policy is treated
// as loopback-only (safe default) rather than open or panicking.
func TestNewOnboardingDefaultsNilPolicyToLoopback(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "nilpol.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	auth := NewAuth(svc, nil, "vtest")
	onb := NewOnboarding(svc, nil, auth, nil, "vtest")

	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(auth), WithOnboarding(onb)).Register(mux)

	// Loopback is allowed.
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	req.RemoteAddr = loopbackPeer
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup loopback (nil policy) = %d, want 200", rec.Code)
	}
	// A non-loopback peer is refused.
	req2 := httptest.NewRequest(http.MethodGet, "/setup", nil)
	req2.RemoteAddr = "192.0.2.50:1"
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("GET /setup non-loopback (nil policy) = %d, want 404", rec2.Code)
	}
}
