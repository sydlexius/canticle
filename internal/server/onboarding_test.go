package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/web"
	"github.com/sydlexius/canticle/internal/webauth"
)

// TestWithOnboardingWiring verifies the server-level wiring: WithWebUIAuth +
// WithOnboarding mounts the /setup endpoint and redirects the UI pages to /setup
// while no admin exists. The route-level behavior is covered in internal/web;
// this asserts the handler-construction path (WithOnboarding + AttachOnboarding).
func TestWithOnboardingWiring(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "onboard.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	policy := trustnet.LoopbackOnly()
	webAuth := web.NewAuth(svc, policy, "vtest")
	onboarding := web.NewOnboarding(svc, nil, webAuth, policy, "vtest")

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUIAuth(config.Config{}, "vtest", webAuth),
		WithOnboarding(onboarding),
	)

	t.Run("GET /setup is mounted and reachable from loopback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /setup = %d, want 200", rec.Code)
		}
	})

	t.Run("UI page redirects to /setup before an admin exists", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.RemoteAddr = "127.0.0.1:5001"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET /config first-run = %d, want 303", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/setup" {
			t.Errorf("Location = %q, want /setup", loc)
		}
	})
}
