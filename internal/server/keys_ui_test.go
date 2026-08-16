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

// TestWithMusixmatchServingWiring verifies the server-level wiring for the
// Musixmatch attribution credit (#600, API Terms clause 2.1.5): the option must
// reach the mounted web UI and gate the rendered credit.
//
// This exists at the SERVER layer, not just in internal/web, because the defect
// CodeRabbit caught on PR #769 was a WIRING bug: the credit was derived from the
// wrong signal, and every unit test of the rendering layer passed while the
// assembled handler credited the wrong provider. A test that stops at the UI
// package cannot see a seam that drops the flag or takes it from the wrong source.
func TestWithMusixmatchServingWiring(t *testing.T) {
	body := func(t *testing.T, opts ...Option) string {
		t.Helper()
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
			append([]Option{WithWebUI(config.Config{}, "vtest")}, opts...)...)
		req := httptest.NewRequest(http.MethodGet, "/reports", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	t.Run("serving: the credit reaches the rendered page", func(t *testing.T) {
		got := body(t, WithMusixmatchServing(true))
		if !strings.Contains(got, "mx-provider-credit") {
			t.Error("credit missing; WithMusixmatchServing(true) did not reach the web UI")
		}
		if !strings.Contains(got, "https://www.musixmatch.com") {
			t.Error("credit rendered without the required link to the Site (clause 2.1.5)")
		}
	})

	t.Run("not serving: no credit", func(t *testing.T) {
		if got := body(t, WithMusixmatchServing(false)); strings.Contains(got, "mx-provider-credit") {
			t.Error("credit rendered while Musixmatch is not serving; that is a misattribution")
		}
	})

	// The DEFAULT matters as much as either explicit case. A caller that forgets
	// the option must get NO credit rather than a wrong one -- a missing credit is
	// a bug to fix, a credit naming the wrong provider is published
	// misattribution. This also pins the zero value, so a future refactor that
	// inverts the flag's sense fails here.
	t.Run("option omitted: defaults to no credit", func(t *testing.T) {
		if got := body(t); strings.Contains(got, "mx-provider-credit") {
			t.Error("credit rendered by default; the flag must be affirmatively set")
		}
	})
}
