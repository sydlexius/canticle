package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/web"
	"github.com/sydlexius/canticle/internal/webauth"
)

// TestAuthAndReadinessReadPastHeldWriter pins the #1652 wiring through the real
// handler: with the writer's single connection held, a signed-in UI request
// (session + user lookup) and GET /readyz are answered from the read pool at
// once. The writer-only control proves the hold starves those same requests, so
// the split case cannot pass for the wrong reason.
func TestAuthAndReadinessReadPastHeldWriter(t *testing.T) {
	ctx := context.Background()
	path := dbtest.Path(t)
	writer, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	pool, err := db.OpenReadPool(ctx, path, 2)
	if err != nil {
		t.Fatalf("db.OpenReadPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	seed := webauth.NewService(webauth.NewSQLUserStore(writer), webauth.NewSQLSessionStore(writer))
	if _, err := seed.Setup(ctx, "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := seed.Login(ctx, "admin", "correct-horse-battery")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// An untrusted source, so the request must present the session cookie.
	policy, err := trustnet.NewPolicy([]string{"192.0.2.0/24"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	build := func(svc *webauth.Service, ready Readiness) *Handler {
		return NewHandler(&fakeAuth{err: auth.ErrInvalidKey}, &fakeQueue{}, "lyrics",
			WithWebUIAuth(config.Config{}, "vtest", web.NewAuth(svc, policy, "vtest")),
			WithTrustedNetworks(policy),
			WithReadiness(ready),
		)
	}
	split := build(
		webauth.NewService(webauth.NewSQLUserStore(writer).WithReadDB(pool), webauth.NewSQLSessionStore(writer).WithReadDB(pool)),
		pool)
	writerOnly := build(seed, writer)

	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatalf("hold the writer's connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	const budget = time.Second
	get := func(h *Handler, target string, withCookie bool) (int, time.Duration) {
		rctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		req := httptest.NewRequestWithContext(rctx, http.MethodGet, target, nil)
		req.RemoteAddr = "198.51.100.9:6000"
		if withCookie {
			req.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: token})
		}
		rec := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(rec, req)
		return rec.Code, time.Since(start)
	}

	for _, tc := range []struct {
		name, target string
		cookie       bool
	}{
		{"signed-in UI page", "/settings", true},
		{"readyz", "/readyz", false},
	} {
		t.Run("split "+tc.name, func(t *testing.T) {
			code, took := get(split, tc.target, tc.cookie)
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 with the writer held", tc.target, code)
			}
			if took > budget/4 {
				t.Errorf("GET %s took %v with the writer held; want well under %v", tc.target, took, budget)
			}
		})
		t.Run("writer-only control "+tc.name, func(t *testing.T) {
			if code, _ := get(writerOnly, tc.target, tc.cookie); code == http.StatusOK {
				t.Fatalf("control: GET %s = 200 through the writer while it is held; the hold must starve it", tc.target)
			}
		})
	}
}
