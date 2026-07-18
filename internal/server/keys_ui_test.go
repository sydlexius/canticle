package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
)

// TestWithKeyManagerUIWiring verifies the server-level wiring: WithWebUI +
// WithKeyManagerUI mounts the webhook key management page in a manageable state
// (the create form renders), and omitting the option leaves it unavailable.
func TestWithKeyManagerUIWiring(t *testing.T) {
	km := auth.NewService(auth.NewMemoryStore())
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
		WithKeyManagerUI(km),
	)
	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Generate a new key") {
		t.Error("key management page is not manageable when WithKeyManagerUI is wired")
	}
}

func TestWithoutKeyManagerUIUnavailable(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
	)
	req := httptest.NewRequest(http.MethodGet, "/settings/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unavailable") {
		t.Error("expected the unavailable notice when no key manager is wired")
	}
}
