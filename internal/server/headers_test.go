package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// TestSecurityHeadersOnEveryResponse confirms the baseline security headers are
// applied to every serve-mode response, across a JSON API route (/healthz), a
// UI page route (/login), and a static asset (/static/...). The assertions go
// through a real Handler.ServeHTTP round-trip rather than calling
// setSecurityHeaders in isolation, so the load-bearing ordering (headers set
// before dispatch) is exercised end to end.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	// Reference the single authoritative policy, not a duplicated literal.
	const wantCSP = contentSecurityPolicy

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithWebUI(config.Config{}, "vtest"))

	routes := []struct {
		name string
		path string
	}{
		{"api route", "/healthz"},
		{"ui route", "/login"},
		{"static asset", "/static/css/output.css"},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("Content-Security-Policy = %q, want %q", got, wantCSP)
			}
		})
	}
}
