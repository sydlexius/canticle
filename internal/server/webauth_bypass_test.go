package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/web"
	"github.com/sydlexius/canticle/internal/webauth"
)

// TestTrustedBypassNeverWeakensAPIKey is the contract test for the auth
// interaction matrix (issue #204, Area 2): a request from a trusted network
// bypasses the interactive session requirement on the web UI, but that bypass
// NEVER removes the API-key requirement on the webhook/admin endpoints. Both
// must still be rejected without a key, even from the same trusted source IP.
func TestTrustedBypassNeverWeakensAPIKey(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bypass.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	if _, err := svc.Setup(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	policy, err := trustnet.NewPolicy([]string{"192.0.2.0/24"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	webAuth := web.NewAuth(svc, policy, "vtest")

	// fakeAuth rejects every key, standing in for a request that presents none.
	rejecting := &fakeAuth{err: auth.ErrInvalidKey}
	h := NewHandler(rejecting, &fakeQueue{}, "lyrics",
		WithWebUIAuth(config.Config{}, "vtest", webAuth),
		WithTrustedNetworks(policy),
	)

	const trustedIP = "192.0.2.50:6000"

	t.Run("web UI is bypassed from a trusted IP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.RemoteAddr = trustedIP // trusted, no session cookie
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /settings from trusted IP = %d, want 200 (session bypass)", rec.Code)
		}
	})

	t.Run("admin status still requires a key from a trusted IP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.RemoteAddr = trustedIP // trusted source, but no API key
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET /api/v1/status from trusted IP without key = %d, want 401", rec.Code)
		}
	})

	t.Run("webhook still requires a key from a trusted IP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr", nil)
		req.RemoteAddr = trustedIP // trusted source, but no API key
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST webhook from trusted IP without key = %d, want 401", rec.Code)
		}
	})

	t.Run("web UI redirects an untrusted IP to login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req.RemoteAddr = "198.51.100.9:6000" // untrusted, no cookie
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET /settings from untrusted IP = %d, want 303", rec.Code)
		}
	})
}
