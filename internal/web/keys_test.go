package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
)

// errTest is a sentinel returned by errKeyManager to drive the handler error
// branches.
var errTest = errors.New("test error")

// keysTestUI builds a UI with the given key manager wired and returns the mounted
// handler. Built without auth, so routes are public and the same-origin check
// passes for a header-less test request.
func keysTestUI(t *testing.T, km KeyManager) http.Handler {
	t.Helper()
	u := NewUI(config.Config{}, "v0", WithKeyManager(km))
	mux := http.NewServeMux()
	u.Register(mux)
	return mux
}

// seedKey creates a key in the manager and returns its raw value and metadata.
func seedKey(t *testing.T, km KeyManager, name string, scopes ...auth.Scope) auth.CreatedKey {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []auth.Scope{auth.ScopeWebhook}
	}
	created, err := km.CreateKey(context.Background(), name, scopes)
	if err != nil {
		t.Fatalf("seed CreateKey: %v", err)
	}
	return created
}

func newKeyManager() KeyManager {
	return auth.NewService(auth.NewMemoryStore())
}

// postKeys issues a POST to path with a valid double-submit CSRF token.
func postKeys(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf_token", testCSRFToken)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWebhookKeysListNeverExposesSecrets is the core security assertion: the
// masked list page renders metadata but never the raw key and never the full
// PBKDF2 hash.
func TestWebhookKeysListNeverExposesSecrets(t *testing.T) {
	km := newKeyManager()
	created := seedKey(t, km, "lidarr-main")

	h := keysTestUI(t, km)
	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/keys status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, created.Raw) {
		t.Error("list page leaked the raw key")
	}
	if strings.Contains(body, created.Key.Hash) {
		t.Error("list page leaked the full key hash")
	}
	// The masked public ID (and the key name) are expected metadata.
	if !strings.Contains(body, "lidarr-main") {
		t.Error("list page is missing the key name")
	}
	if !strings.Contains(body, created.Key.ID[:keyIDDisplayLen]) {
		t.Error("list page is missing the truncated key ID")
	}
	// The Created cell uses the dashboard viewer-local pattern: a <time
	// data-tz="pending"> element keys.js reformats, not a bare UTC string.
	if !strings.Contains(body, `data-tz="pending"`) {
		t.Error("Created timestamp is not the viewer-local <time data-tz=pending> element")
	}
	if strings.Contains(body, "@keyTime") {
		t.Error("list page leaked a literal templ component call")
	}
}

// TestWebhookKeysCreateShowsRawOnce asserts the create response reveals the raw
// key exactly once, and that a subsequent plain list render never shows it.
func TestWebhookKeysCreateShowsRawOnce(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)

	rec := postKeys(t, h, "/settings/keys", url.Values{"name": {"created-in-ui"}, "scope": {"webhook"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The create response carries the one-time raw key - it must never be cached.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("create Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()

	// The raw key appears in the create response (it is the one-time reveal).
	keys, err := km.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 key after create, got %d", len(keys))
	}
	if !strings.Contains(body, auth.KeyPrefix) {
		t.Error("create response does not contain the raw key prefix")
	}
	if strings.Contains(body, keys[0].Hash) {
		t.Error("create response leaked the full key hash")
	}

	// A fresh list render (GET) must NOT contain any raw key material.
	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	listBody := rec2.Body.String()
	// The raw key is prefix + 64 hex chars; the prefix may appear in form
	// placeholders, but a full raw key value must not. Assert the readonly reveal
	// input is absent from the plain list.
	if strings.Contains(listBody, `id="mx-newkey-value"`) {
		t.Error("plain list render still shows the one-time raw key field")
	}
	if strings.Contains(listBody, keys[0].Hash) {
		t.Error("plain list render leaked the full key hash")
	}
}

// TestWebhookKeysRevoke marks a key revoked through the handler and confirms the
// re-rendered panel reflects the revoked status.
func TestWebhookKeysRevoke(t *testing.T) {
	km := newKeyManager()
	created := seedKey(t, km, "to-revoke")
	h := keysTestUI(t, km)

	rec := postKeys(t, h, "/settings/keys/revoke", url.Values{"id": {created.Key.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("revoke Cache-Control = %q, want no-store", cc)
	}
	revokeBody := rec.Body.String()
	if !strings.Contains(revokeBody, "mx-keys-status-revoked") {
		t.Error("revoke response does not show the revoked status")
	}
	// The revoked timestamp must render as a real <time> element (viewer-local
	// reformat), not the literal templ call text - guards the inline-@component
	// regression where "Revoked @keyTime(...)" emitted as plain text.
	if !strings.Contains(revokeBody, "<time ") {
		t.Error("revoke response is missing the <time> timestamp element")
	}
	if strings.Contains(revokeBody, "@keyTime") {
		t.Error("revoke response leaked a literal templ component call")
	}

	keys, err := km.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("key was not revoked in the store: %+v", keys)
	}
}

// TestWebhookKeysRevokeUnknownIDConverges asserts revoking a missing id is not a
// hard error: the panel re-renders with a soft notice.
func TestWebhookKeysRevokeUnknownID(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	rec := postKeys(t, h, "/settings/keys/revoke", url.Values{"id": {"deadbeefdeadbeef"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke unknown status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no longer exists") {
		t.Error("revoke unknown id should render the converge notice")
	}
}

// TestWebhookKeysCreateCSRFRejected asserts a create POST without a valid CSRF
// token is rejected and creates nothing.
func TestWebhookKeysCreateCSRFRejected(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)

	// No cookie -> 403.
	req := httptest.NewRequest(http.MethodPost, "/settings/keys",
		strings.NewReader(url.Values{"csrf_token": {testCSRFToken}, "scope": {"webhook"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF cookie status = %d, want 403", rec.Code)
	}
	keys, err := km.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("a key was created despite the CSRF failure: %+v", keys)
	}
}

// TestWebhookKeysUnavailable asserts that without a key manager wired the page
// renders the unavailable notice and the POSTs return 503.
func TestWebhookKeysUnavailable(t *testing.T) {
	u := NewUI(config.Config{}, "v0") // no WithKeyManager
	mux := http.NewServeMux()
	u.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unavailable") {
		t.Error("expected an unavailable notice when no key manager is wired")
	}

	rec2 := postKeys(t, mux, "/settings/keys", url.Values{"scope": {"webhook"}})
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("create with no manager status = %d, want 503", rec2.Code)
	}
}

// errKeyManager is a KeyManager whose operations fail, exercising the handler
// error branches.
type errKeyManager struct {
	listErr, createErr, revokeErr error
}

func (e errKeyManager) ListKeys(context.Context) ([]auth.Key, error) {
	return nil, e.listErr
}

func (e errKeyManager) CreateKey(context.Context, string, []auth.Scope) (auth.CreatedKey, error) {
	return auth.CreatedKey{}, e.createErr
}

func (e errKeyManager) RevokeKeyByID(context.Context, string) (auth.Key, error) {
	return auth.Key{}, e.revokeErr
}

// TestWebhookKeysListError asserts a List failure surfaces a soft error on the
// page rather than a 500 or a misleading empty list.
func TestWebhookKeysListError(t *testing.T) {
	h := keysTestUI(t, errKeyManager{listErr: errTest})
	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not be loaded") {
		t.Error("expected a list-load error notice")
	}
}

// TestWebhookKeysCreateBackendError asserts a CreateKey failure returns 500.
func TestWebhookKeysCreateBackendError(t *testing.T) {
	h := keysTestUI(t, errKeyManager{createErr: errTest})
	rec := postKeys(t, h, "/settings/keys", url.Values{"scope": {"webhook"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestWebhookKeysRevokeBackendError asserts a non-"missing" revoke failure 500s.
func TestWebhookKeysRevokeBackendError(t *testing.T) {
	h := keysTestUI(t, errKeyManager{revokeErr: errTest})
	rec := postKeys(t, h, "/settings/keys/revoke", url.Values{"id": {"abc"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestWebhookKeysCrossSiteRejected asserts a cross-site POST is blocked before
// any mutation, for both create and revoke.
func TestWebhookKeysCrossSiteRejected(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	for _, path := range []string{"/settings/keys", "/settings/keys/revoke"} {
		form := url.Values{"csrf_token": {testCSRFToken}, "scope": {"webhook"}, "id": {"x"}}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s cross-site status = %d, want 403", path, rec.Code)
		}
	}
}

// TestWebhookKeysRevokeMissingID asserts a revoke with no id is a 400.
func TestWebhookKeysRevokeMissingID(t *testing.T) {
	h := keysTestUI(t, newKeyManager())
	rec := postKeys(t, h, "/settings/keys/revoke", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestWebhookKeysCreateInvalidScope asserts a forged unknown scope is rejected.
func TestWebhookKeysCreateInvalidScope(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	rec := postKeys(t, h, "/settings/keys", url.Values{"scope": {"root"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	keys, _ := km.ListKeys(context.Background())
	if len(keys) != 0 {
		t.Errorf("a key was created for an invalid scope: %+v", keys)
	}
}

// TestWebhookKeysAdminScope asserts the admin scope checkbox creates an
// admin-scoped key.
func TestWebhookKeysAdminScope(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	rec := postKeys(t, h, "/settings/keys", url.Values{"scope": {"webhook", "admin"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	keys, _ := km.ListKeys(context.Background())
	if len(keys) != 1 || !auth.HasScope(keys[0].Scopes, auth.ScopeAdmin) {
		t.Fatalf("admin scope not applied: %+v", keys)
	}
}

// TestWebhookKeysCreateRejectsOverlongName asserts a forged over-length name is
// refused server-side (friendly re-render) and no key is created, even though the
// client form caps it.
func TestWebhookKeysCreateRejectsOverlongName(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	longName := strings.Repeat("a", maxKeyNameLen+1)
	rec := postKeys(t, h, "/settings/keys", url.Values{"name": {longName}, "scope": {"webhook"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (friendly re-render)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too long") {
		t.Error("expected an over-long-name notice")
	}
	keys, _ := km.ListKeys(context.Background())
	if len(keys) != 0 {
		t.Errorf("a key was created despite the over-long name: %+v", keys)
	}
	// A name exactly at the limit is accepted.
	rec2 := postKeys(t, h, "/settings/keys", url.Values{"name": {strings.Repeat("a", maxKeyNameLen)}, "scope": {"webhook"}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("at-limit name status = %d, want 200", rec2.Code)
	}
	keys2, _ := km.ListKeys(context.Background())
	if len(keys2) != 1 {
		t.Fatalf("at-limit name should have been created: %+v", keys2)
	}
}

// TestKeyRowHelpers covers the small display-mapping helpers directly.
func TestKeyRowHelpers(t *testing.T) {
	if got := truncateKeyID("abc"); got != "abc" {
		t.Errorf("truncateKeyID short = %q, want abc", got)
	}
	if got := truncateKeyID("0123456789abcdef"); got != "01234567…" {
		t.Errorf("truncateKeyID = %q", got)
	}
	if got := keyDisplayName("   "); got != "(unnamed)" {
		t.Errorf("keyDisplayName blank = %q", got)
	}
	if got := keyDisplayName("x"); got != "x" {
		t.Errorf("keyDisplayName = %q", got)
	}
	if got := scopesDisplay(nil); got != "(none)" {
		t.Errorf("scopesDisplay nil = %q", got)
	}
	if got := scopesDisplay([]auth.Scope{auth.ScopeWebhook, auth.ScopeAdmin}); got != "webhook, admin" {
		t.Errorf("scopesDisplay = %q", got)
	}
}

// TestWebhookKeysCreateDefaultsToWebhookScope asserts an empty scope selection
// defaults to the webhook scope rather than erroring.
func TestWebhookKeysCreateDefaultsToWebhookScope(t *testing.T) {
	km := newKeyManager()
	h := keysTestUI(t, km)
	rec := postKeys(t, h, "/settings/keys", url.Values{"name": {"no-scope"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	keys, err := km.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 || len(keys[0].Scopes) != 1 || keys[0].Scopes[0] != auth.ScopeWebhook {
		t.Fatalf("default scope not applied: %+v", keys)
	}
}
