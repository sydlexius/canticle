package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
)

// seedQueueRows inserts n work_queue rows in the given status with titles
// "<prefix> 001".."<prefix> NNN" and returns their ids in insert order.
func seedQueueRows(t *testing.T, sqlDB *sql.DB, status, prefix string, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 1; i <= n; i++ {
		title := fmt.Sprintf("%s %03d", prefix, i)
		res, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
             VALUES (?, ?, ?, ?, 'Invented Album', ?)`,
			"Invented Artist", title, "invented artist", strings.ToLower(title), status)
		if err != nil {
			t.Fatalf("seed %s row: %v", status, err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids
}

func getQueue(t *testing.T, mux http.Handler, target string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var queueTitleRE = regexp.MustCompile(`(?:Pend|Other|Done) \d{3}`)

func titlesIn(body string) []string { return queueTitleRE.FindAllString(body, -1) }

func TestQueueBucketListsOnlyItsRows(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedQueueRows(t, sqlDB, "pending", "Pend", 3)
	seedQueueRows(t, sqlDB, "deferred", "Other", 2)
	mux := newReportsUIServer(t, sqlDB)

	rec := getQueue(t, mux, "/queue/pending", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	if got := titlesIn(body); len(got) != 3 || got[0] != "Pend 001" {
		t.Errorf("titles = %v, want exactly the 3 pending rows", got)
	}
	for _, want := range []string{"<html", "<h1", "Pending", "Next attempt", "Libraries", "/static/css/queue.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("full page missing %q", want)
		}
	}
	if strings.Contains(body, "Show more") {
		t.Error("a bucket under one page must not offer Show more")
	}
}

func TestQueueBucketEmptyState(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	rec := getQueue(t, mux, "/queue/unavailable", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No rows in this bucket.") {
		t.Fatalf("status=%d, body missing empty state", rec.Code)
	}
}

func TestQueueBucketPastEndSaysNoMoreRows(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	body := getQueue(t, mux, "/queue/unavailable?after=999", false).Body.String()
	if !strings.Contains(body, "No more rows.") || !strings.Contains(body, "Back to start of list") {
		t.Fatalf("past-the-end page wording wrong: %s", body)
	}
}

func TestBuildQueueRowSettledHasNoNextAttempt(t *testing.T) {
	stale := "2026-06-17T10:00:00Z"
	for _, status := range []string{queue.StatusDone, queue.StatusUnavailable} {
		if got := buildQueueRow(reports.BucketRow{Status: status, NextAttemptAt: stale}).NextAttemptAt; got != "-" {
			t.Errorf("%s next attempt = %q, want - (stale retry time)", status, got)
		}
	}
	if got := buildQueueRow(reports.BucketRow{Status: queue.StatusDeferred, NextAttemptAt: stale}).NextAttemptAt; got == "-" {
		t.Errorf("deferred row lost its next attempt")
	}
}

func TestQueueBucketPagerWalksWithoutDuplicates(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	total := queuePageSize*2 + 7
	ids := seedQueueRows(t, sqlDB, "pending", "Pend", total)
	mux := newReportsUIServer(t, sqlDB)

	seen := map[string]int{}
	target := "/queue/pending"
	for page := 0; page < 10; page++ {
		rec := getQueue(t, mux, target, page > 0)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d", page, rec.Code)
		}
		body := rec.Body.String()
		if page > 0 && strings.Contains(body, "<html") {
			t.Fatalf("page %d: htmx request got a full page", page)
		}
		for _, title := range titlesIn(body) {
			seen[title]++
		}
		m := regexp.MustCompile(`hx-get="(/queue/pending\?after=\d+)"`).FindStringSubmatch(body)
		if m == nil {
			break
		}
		target = m[1]
		// The cursor is exactly the last row of the page just rendered.
		wantAfter := ids[(page+1)*queuePageSize-1]
		if target != fmt.Sprintf("/queue/pending?after=%d", wantAfter) {
			t.Fatalf("page %d: next = %q, want after=%d", page, target, wantAfter)
		}
	}
	if len(seen) != total {
		t.Fatalf("distinct titles = %d, want %d", len(seen), total)
	}
	for title, n := range seen {
		if n != 1 {
			t.Errorf("%s rendered %d times across pages, want once", title, n)
		}
	}
}

func TestQueueBucketExactPageHasNoShowMore(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedQueueRows(t, sqlDB, "pending", "Pend", queuePageSize)
	rec := getQueue(t, newReportsUIServer(t, sqlDB), "/queue/pending", false)
	if got := len(titlesIn(rec.Body.String())); got != queuePageSize {
		t.Fatalf("rows = %d, want %d", got, queuePageSize)
	}
	if strings.Contains(rec.Body.String(), "Show more") {
		t.Error("exactly one full page must not offer an empty Show more")
	}
}

func TestQueueBucketFragmentNoStoreAndNoLayout(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	ids := seedQueueRows(t, sqlDB, "pending", "Pend", 3)
	rec := getQueue(t, newReportsUIServer(t, sqlDB), fmt.Sprintf("/queue/pending?after=%d", ids[0]), true)
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("fragment Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	if got := titlesIn(body); len(got) != 2 || got[0] != "Pend 002" {
		t.Errorf("fragment titles = %v, want rows after the cursor", got)
	}
}

func TestQueueBucketErrors(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	cases := []struct {
		target string
		want   int
	}{
		{"/queue/nonsense", http.StatusNotFound},
		{"/queue/pending?after=abc", http.StatusBadRequest},
		{"/queue/pending?after=-4", http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := getQueue(t, mux, c.target, false)
		if rec.Code != c.want {
			t.Errorf("GET %s = %d, want %d", c.target, rec.Code, c.want)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store even on errors", c.target, cc)
		}
	}

	noRepo := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(noRepo)
	if rec := getQueue(t, noRepo, "/queue/pending", false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no reports repo = %d, want 503", rec.Code)
	}
}

func TestQueueBucketShowsAllLibrariesOfARow(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	ids := seedQueueRows(t, sqlDB, "unavailable", "Done", 2)
	var libs []int64
	for _, name := range []string{"Alpha Lib", "Beta Lib"} {
		res, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO libraries (path, name) VALUES (?, ?)`, "/m/"+name, name)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		libs = append(libs, id)
	}
	for i, lib := range libs {
		res, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, lib, fmt.Sprintf("/m/%d.flac", i))
		if err != nil {
			t.Fatal(err)
		}
		sr, _ := res.LastInsertId()
		if _, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, ids[0], sr); err != nil {
			t.Fatal(err)
		}
	}
	body := getQueue(t, newReportsUIServer(t, sqlDB), "/queue/unavailable", false).Body.String()
	if !strings.Contains(body, "Alpha Lib, Beta Lib") {
		t.Errorf("shared row must list both libraries; body lacks them")
	}
	if !strings.Contains(body, "<td>-</td>") {
		t.Error("an unlinked row must render a dash for libraries")
	}
}

func TestFormatQueueTime(t *testing.T) {
	if got := formatQueueTime(""); got != "-" {
		t.Errorf("empty = %q, want -", got)
	}
	if got := formatQueueTime("1970-01-01T00:00:00Z"); got != "-" {
		t.Errorf("epoch default = %q, want - (not a real retry time)", got)
	}
	if got := formatQueueTime("not a time"); got != "not a time" {
		t.Errorf("unparsable = %q, want verbatim", got)
	}
	const raw = "2026-06-17T10:00:00Z"
	instant := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)

	// TZ unset: the same UTC rendering every other report produces.
	t.Setenv("TZ", "")
	if got, want := formatQueueTime(raw), formatReportTime(instant, nil); got != want {
		t.Errorf("TZ unset = %q, want %q (UTC)", got, want)
	}

	// TZ valid: formatted in that zone, not the host's local zone.
	t.Setenv("TZ", "Pacific/Kiritimati")
	loc, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	got, want := formatQueueTime(raw), formatReportTime(instant, loc)
	if got != want {
		t.Errorf("TZ set = %q, want %q", got, want)
	}
	if got == formatReportTime(instant, nil) {
		t.Errorf("TZ set rendered identically to UTC: %q", got)
	}
}

// TestQueueRoutesAuthGuarded proves the session guard covers the queue view and
// that no row content reaches an unauthenticated caller.
func TestQueueRoutesAuthGuarded(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	seedQueueRows(t, sqlDB, "pending", "Pend", 2)
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB))).Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/queue/pending", nil)
	req.RemoteAddr = "198.51.100.30:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("unauthenticated = %d -> %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "Pend 001") {
		t.Error("row content leaked to an unauthenticated caller")
	}
	// An unknown bucket must not leak existence either: guard runs first.
	req = httptest.NewRequest(http.MethodGet, "/queue/nonsense", nil)
	req.RemoteAddr = "198.51.100.30:1"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated unknown bucket = %d, want 303 (guard before 404)", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/queue/pending", nil)
	req.RemoteAddr = "198.51.100.30:1"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pend 001") {
		t.Fatalf("authenticated = %d, want 200 with rows", rec.Code)
	}
}

var routeParamRE = regexp.MustCompile(`\{[^}]*\}`)

// TestEveryRegisteredRouteIsGuarded enumerates the patterns Register put
// behind the guard (via the same reg closure every route uses) and requires a
// session-less request to each to be refused. It proves the reg refactor kept
// the guard on every route, including ones added later from their own files.
func TestEveryRegisteredRouteIsGuarded(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB)))
	ui.Register(mux)

	// The pre-refactor guarded set, plus the queue route: a floor so an empty or
	// shrunken enumeration cannot pass vacuously.
	want := []string{
		"GET /{$}", "GET /dashboard", "GET /reports", "GET /reports/{key}", "GET /config",
		"GET /settings", "POST /settings/field", "POST /settings/section",
		"GET /settings/keys", "POST /settings/keys", "POST /settings/keys/revoke",
		"GET /queue/{bucket}", "GET /reports/failure-group", "GET /preview/{id}/audio",
		"GET /queue/unavailable/revive", "POST /queue/unavailable/revive",
	}
	have := map[string]bool{}
	for _, p := range ui.guardedRoutes {
		have[p] = true
	}
	for _, p := range want {
		if !have[p] {
			t.Errorf("route %q is not registered through the guard", p)
		}
	}

	for _, pattern := range ui.guardedRoutes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("malformed pattern %q", pattern)
		}
		path = strings.ReplaceAll(path, "{$}", "")
		path = routeParamRE.ReplaceAllString(path, "pending")
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "198.51.100.31:1"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("%s without a session = %d -> %q, want 303 to /login", pattern, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestQueuePreviewHref(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  reports.BucketRow
		want string
	}{
		{"word tier", reports.BucketRow{ID: 7, Status: queue.StatusDone, SyncTier: "word"}, "/preview/7"},
		{"line tier", reports.BucketRow{ID: 8, Status: queue.StatusDone, SyncTier: "line"}, "/preview/8"},
		{"unsynced tier", reports.BucketRow{ID: 9, Status: queue.StatusDone, SyncTier: "unsynced"}, ""},
		{"unclassified", reports.BucketRow{ID: 10, Status: queue.StatusDone}, ""},
		{"not settled", reports.BucketRow{ID: 11, Status: queue.StatusPending, SyncTier: "line"}, ""},
	} {
		if got := queuePreviewHref(tc.row); got != tc.want {
			t.Errorf("%s: href = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestQueueBucketPreviewLinkOnlyOnSyncedRows(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	ids := seedQueueRows(t, sqlDB, "done", "Done", 3)
	for i, tier := range []string{"word", "line", "unsynced"} {
		if _, err := sqlDB.ExecContext(context.Background(),
			`UPDATE work_queue SET outcome_type = 'synced', sync_tier = ? WHERE id = ?`, tier, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	mux := newReportsUIServer(t, sqlDB)
	finished := getQueue(t, mux, "/queue/finished", false).Body.String()
	if !strings.Contains(finished, fmt.Sprintf(`href="/preview/%d"`, ids[0])) {
		t.Errorf("finished (word) row lacks its preview link")
	}
	settled := getQueue(t, mux, "/queue/settled", false).Body.String()
	if !strings.Contains(settled, fmt.Sprintf(`href="/preview/%d"`, ids[1])) {
		t.Errorf("settled line-synced row lacks its preview link")
	}
	if strings.Contains(settled, fmt.Sprintf(`href="/preview/%d"`, ids[2])) {
		t.Errorf("unsynced row must not link to the player")
	}
}
