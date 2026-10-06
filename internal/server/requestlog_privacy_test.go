package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestRedactURIValidatesAllowlistedValues pins that an allowlisted key's value
// is logged only when it has the shape its handler accepts (#1234 review).
func TestRedactURIValidatesAllowlistedValues(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"after valid", "/queue/pending?after=42", "/queue/pending?after=42"},
		{"after invalid", "/queue/pending?after=SECRETARTIST", "/queue/pending?after=REDACTED"},
		{"after negative", "/queue/pending?after=-1", "/queue/pending?after=REDACTED"},
		{"after cursor null value", "/queue/pending?after=42:n", "/queue/pending?after=42%3An"},
		{"after cursor int value", "/queue/pending?after=42:i-3", "/queue/pending?after=42%3Ai-3"},
		{"after cursor text value is metadata", "/queue/pending?after=42:tSECRETARTIST", "/queue/pending?after=REDACTED"},
		{"sort valid", "/queue/pending?sort=next_attempt", "/queue/pending?sort=next_attempt"},
		{"sort invalid", "/queue/pending?sort=SECRETARTIST", "/queue/pending?sort=REDACTED"},
		{"sort injection", "/queue/pending?sort=id%3Bdrop", "/queue/pending?sort=REDACTED"},
		{"dir asc", "/queue/pending?dir=asc", "/queue/pending?dir=asc"},
		{"dir desc", "/queue/pending?dir=desc", "/queue/pending?dir=desc"},
		{"dir invalid", "/queue/pending?dir=sideways", "/queue/pending?dir=REDACTED"},
		{"ro sort valid", "/reports/recent-outcomes?ro_sort=completed", "/reports/recent-outcomes?ro_sort=completed"},
		{"ro sort invalid", "/reports/recent-outcomes?ro_sort=SECRETARTIST", "/reports/recent-outcomes?ro_sort=REDACTED"},
		{"fg dir invalid", "/reports/failure-group?fg_dir=sideways", "/reports/failure-group?fg_dir=REDACTED"},
		{"rq dir desc", "/reports/review-queue?rq_dir=desc", "/reports/review-queue?rq_dir=desc"},
		{"in sort valid", "/reports/instrumental-inventory?in_sort=detect", "/reports/instrumental-inventory?in_sort=detect"},
		{"from valid", "/preview/3?from=failed", "/preview/3?from=failed"},
		{"from invalid", "/preview/3?from=SECRETARTIST", "/preview/3?from=REDACTED"},
		{"from numeric is not a bucket", "/preview/3?from=7", "/preview/3?from=REDACTED"},
		{"status valid", "/x?status=deferred", "/x?status=deferred"},
		{"status invalid", "/x?status=SECRETARTIST", "/x?status=REDACTED"},
		{"library all", "/x?library=all", "/x?library=all"},
		{"library id", "/x?library=12", "/x?library=12"},
		{"library zero", "/x?library=0", "/x?library=REDACTED"},
		{"library text", "/x?library=SECRETARTIST", "/x?library=REDACTED"},
		{"queue library id", "/queue/settled?library=3", "/queue/settled?library=3"},
		{"queue library name is metadata", "/queue/settled?library=SECRETLIBRARY", "/queue/settled?library=REDACTED"},
		{"lane valid", "/queue/settled?lane=innertube", "/queue/settled?lane=innertube"},
		{"lane text", "/queue/settled?lane=SECRETARTIST", "/queue/settled?lane=REDACTED"},
		{"reason valid", "/queue/failed?reason=write", "/queue/failed?reason=write"},
		{"reason text", "/queue/failed?reason=SECRETARTIST", "/queue/failed?reason=REDACTED"},
		{"tier line", "/queue/settled?tier=line", "/queue/settled?tier=line"},
		{"tier word is not offered", "/queue/settled?tier=word", "/queue/settled?tier=REDACTED"},
		{"tier invalid", "/queue/settled?tier=SECRETARTIST", "/queue/settled?tier=REDACTED"},
		{"edited one", "/queue/settled?edited=1", "/queue/settled?edited=1"},
		{"edited text", "/queue/settled?edited=SECRETARTIST", "/queue/settled?edited=REDACTED"},
		{"missync one", "/queue/settled?missync=1", "/queue/settled?missync=1"},
		{"missync text", "/queue/settled?missync=SECRETARTIST", "/queue/settled?missync=REDACTED"},
		{"repeated validated independently", "/x?after=1&after=SECRETARTIST", "/x?after=1&after=REDACTED"},
		{"unlisted key", "/x?q=SECRETARTIST&apikey=k", "/x?apikey=REDACTED&q=REDACTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := redactURI(u); got != tc.want {
				t.Errorf("redactURI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
