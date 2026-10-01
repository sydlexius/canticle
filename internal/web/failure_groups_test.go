package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/web/templates"
)

func seedFailureRow(t *testing.T, title, status, reason string, exec func(string, ...any) error) {
	t.Helper()
	if err := exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error, attempts, miss_count)
         VALUES ('Invented Artist', ?, 'invented artist', ?, 'Invented Album', ?, ?, 1, 1)`,
		title, strings.ToLower(title), status, reason); err != nil {
		t.Fatalf("seed %s row: %v", title, err)
	}
}

func getFailureGroup(t *testing.T, mux http.Handler, status, signature string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"status": {status}, "signature": {signature}}
	req := httptest.NewRequest(http.MethodGet, "/reports/failure-group?"+q.Encode(), nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFailureGroupFragmentListsOnlyItsRows(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	seedFailureRow(t, "Alpha One", "failed", "worker: write item 1: permission denied", exec)
	seedFailureRow(t, "Beta Two", "failed", "musixmatch: unexpected matcher status_code 500", exec)
	// Distinct sentinel values so a cell dropped from the fragment cannot hide
	// behind a coincidental default (miss_count seeds as 1, next_attempt_at as epoch).
	if err := exec(`UPDATE work_queue SET miss_count = 7, attempts = 4, next_attempt_at = '2031-04-05T06:07:08Z' WHERE title = 'Beta Two'`); err != nil {
		t.Fatalf("set sentinels: %v", err)
	}
	mux := newReportsUIServer(t, sqlDB)

	rec := getFailureGroup(t, mux, "failed", "musixmatch: unexpected matcher status_code 500")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Beta Two") || strings.Contains(body, "Alpha One") {
		t.Errorf("fragment must hold only the requested group; body: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment must not render the full layout")
	}
	for _, want := range []string{
		`<td class="mx-cell-mono">7</td>`,                                               // miss_count
		`<td class="mx-cell-mono">4</td>`,                                               // attempts
		`<td class="mx-cell-mono">` + formatQueueTime("2031-04-05T06:07:08Z") + `</td>`, // next_attempt_at
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %q; body: %s", want, body)
		}
	}
}

func TestFailureGroupFragmentEmptyAndErrors(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	rec := getFailureGroup(t, mux, "failed", "no such signature")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No rows in this group.") {
		t.Errorf("unknown signature = %d %q, want 200 empty state", rec.Code, rec.Body.String())
	}
	for _, status := range []string{"", "pending", "done"} {
		rec := getFailureGroup(t, mux, status, "x")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %q = %d, want 400", status, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("status %q: Cache-Control = %q on an error, want no-store", status, cc)
		}
	}
}

func TestFailureGroupFragmentNoRepo(t *testing.T) {
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(mux)
	if rec := getFailureGroup(t, mux, "failed", "x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no repo = %d, want 503", rec.Code)
	}
}

func TestFailureGroupFragmentTruncates(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	for i := 0; i < failureGroupPageSize+1; i++ {
		seedFailureRow(t, fmt.Sprintf("Row %03d", i), "deferred", "no match", exec)
	}
	mux := newReportsUIServer(t, sqlDB)
	body := getFailureGroup(t, mux, "deferred", "no match").Body.String()
	if got := strings.Count(body, "<tr>") - 1; got != failureGroupPageSize {
		t.Errorf("rows = %d, want %d", got, failureGroupPageSize)
	}
	if !strings.Contains(body, templates.FailureTruncatedNote(failureGroupPageSize)) {
		t.Error("truncated expansion must say it is capped")
	}
}

func TestFailureGroupFragmentAuthGuarded(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	seedFailureRow(t, "Guarded Row", "deferred", "no match", exec)
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB))).Register(mux)

	target := "/reports/failure-group?status=deferred&signature=no+match"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "198.51.100.32:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("no session = %d -> %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "Guarded Row") {
		t.Error("row content leaked to an unauthenticated caller")
	}

	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "198.51.100.32:1"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Guarded Row") {
		t.Fatalf("session = %d, want 200 with the row", rec.Code)
	}
}

func TestGroupClass(t *testing.T) {
	for _, c := range []struct {
		status, reason, want string
	}{
		{"failed", "musixmatch: unexpected matcher status_code 500", "transient"},
		{"failed", "worker: write item 1: permission denied", "persistent"},
		{"deferred", "no match", ""},
		{"deferred", "musixmatch: unexpected matcher status_code 500", ""},
	} {
		if got := string(groupClass(c.status, c.reason)); got != c.want {
			t.Errorf("groupClass(%s, %q) = %q, want %q", c.status, c.reason, got, c.want)
		}
	}
}

// TestFailureReportRendersBadgeLinkAndExpander covers the group table itself:
// a badge for a failed group only, the /queue/unavailable pointer, and a
// per-group expander whose URL carries the signature as a query value.
func TestFailureReportRendersBadgeLinkAndExpander(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	seedFailureRow(t, "F", "failed", "musixmatch: unexpected matcher status_code 500", exec)
	seedFailureRow(t, "D", "deferred", "no match", exec)
	mux := newReportsUIServer(t, sqlDB)

	failed := getFragment(t, mux, "failure-analysis").Body.String()
	for _, want := range []string{
		`mx-class-badge mx-class-transient`,
		`href="/queue/unavailable"`,
		`hx-get="/reports/failure-group?signature=musixmatch%3a+unexpected+matcher+status_code+500&amp;status=failed"`,
		`id="mx-fg-0"`,
		`hx-target="#mx-fg-0" hx-swap="innerHTML"`, // adjacent: other controls also carry innerHTML
		`<button type="button" class="mx-run-button mx-fg-toggle"`,
		`aria-expanded="false"`,
		`aria-controls="mx-fg-0"`,
	} {
		if !strings.Contains(strings.ToLower(failed), strings.ToLower(want)) {
			t.Errorf("failure-analysis missing %q; body: %s", want, failed)
		}
	}
	// No non-htmx navigation: the expander is a button, never an anchor to the
	// bare fragment URL (a middle-click would open an unstyled fragment).
	if strings.Contains(failed, `href="/reports/failure-group`) {
		t.Errorf("expander must not carry an href to the fragment URL; body: %s", failed)
	}
	deferred := getFragment(t, mux, "deferred-misses").Body.String()
	if strings.Contains(deferred, "mx-class-badge") {
		t.Errorf("a deferred group must carry no class badge; body: %s", deferred)
	}
	if !strings.Contains(deferred, `href="/queue/unavailable"`) {
		t.Error("deferred-misses must also point at /queue/unavailable")
	}
}
