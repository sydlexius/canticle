package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/web"
	"github.com/sydlexius/canticle/internal/webauth"
)

// captureDebugLog routes the default slog logger to a buffer at debug level for
// the test's duration.
func captureDebugLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestSearchTextNeverReachesRequestLog pins #1234: the queue search text rides
// in the query string and must not appear in any log line, whether the page is
// served directly or the request is bounced to /login?next=<page incl. q>.
func TestSearchTextNeverReachesRequestLog(t *testing.T) {
	const token = "zqxj7uniquesearchtoken"

	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "log.db"))
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
	h := NewHandler(&fakeAuth{err: auth.ErrInvalidKey}, &fakeQueue{}, "lyrics",
		WithWebUIAuth(config.Config{}, "vtest", web.NewAuth(svc, policy, "vtest")),
		WithReportsDB(sqlDB),
		WithTrustedNetworks(policy),
	)

	cases := []struct {
		name, remote, uri string
		wantCode          int
	}{
		{"served directly", "192.0.2.50:6000", "/queue/pending?q=" + token, http.StatusOK},
		{"login redirect", "198.51.100.9:6000", "/queue/pending?q=" + token, http.StatusSeeOther},
		{"login page carrying next", "198.51.100.9:6000", "/login?next=%2Fqueue%2Fpending%3Fq%3D" + token, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDebugLog(t)
			req := httptest.NewRequest(http.MethodGet, tc.uri, nil)
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("GET %s = %d, want %d", tc.uri, rec.Code, tc.wantCode)
			}
			if tc.name == "login redirect" && strings.Contains(rec.Header().Get("Location"), token) == false {
				t.Fatalf("redirect Location lacks the return path; the case no longer exercises next: %q", rec.Header().Get("Location"))
			}
			out := buf.String()
			if !strings.Contains(out, "http request") {
				t.Fatalf("no request log captured (vacuous pass): %q", out)
			}
			if strings.Contains(out, token) {
				t.Errorf("search text leaked into the log: %s", out)
			}
		})
	}
}
