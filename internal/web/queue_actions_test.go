package web

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

const reviveSentinel = "miss limit reached"

const revivePath = "/queue/unavailable/revive"

type reviveFixture struct {
	sqlDB      *sql.DB
	mux        *http.ServeMux
	libA, libB int64
	// Retired rows: A-only, shared (A and B), B-only; plainDeferred is never touched.
	rowA1, rowA2, rowShared, rowB1, plainDeferred int64
	n                                             int
}

// reviveMux serves the UI with the given queue actions attached.
func reviveMux(sqlDB *sql.DB, qa QueueActions) *http.ServeMux {
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	ui.AttachQueueActions(qa)
	ui.Register(mux)
	return mux
}

func (f *reviveFixture) insert(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := f.sqlDB.QueryRow(q+" RETURNING id", args...).Scan(&id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

// addRow inserts a work_queue row linked to one scan_result per library.
func (f *reviveFixture) addRow(t *testing.T, status, lastErr string, libs ...int64) int64 {
	t.Helper()
	f.n++
	key := "k" + strconv.Itoa(f.n)
	id := f.insert(t, `INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error)
	                VALUES ('Secret Artist', 'Secret Title '||?, ?, ?, 'Album', ?, ?)`, key, key, key, status, lastErr)
	for _, lib := range libs {
		sr := f.insert(t, `INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
		                VALUES (?, 'Secret Artist', 'Secret Title', ?, 'out', 'song.lrc', 'done')`,
			lib, "/secret/path/"+key+"-"+strconv.FormatInt(lib, 10)+".flac")
		if _, err := f.sqlDB.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	return id
}

func seedRevive(t *testing.T) *reviveFixture {
	t.Helper()
	f := &reviveFixture{sqlDB: openReportsTestDB(t)}
	f.libA = f.insert(t, `INSERT INTO libraries (path, name) VALUES ('/lib-a', 'Alpha')`)
	f.libB = f.insert(t, `INSERT INTO libraries (path, name) VALUES ('/lib-b', 'Bravo')`)
	f.rowA1 = f.addRow(t, "unavailable", reviveSentinel, f.libA)
	f.rowA2 = f.addRow(t, "unavailable", reviveSentinel, f.libA)
	f.rowShared = f.addRow(t, "unavailable", reviveSentinel, f.libA, f.libB)
	f.rowB1 = f.addRow(t, "unavailable", reviveSentinel, f.libB)
	f.plainDeferred = f.addRow(t, "deferred", "", f.libA)
	f.mux = reviveMux(f.sqlDB, queue.NewDBQueue(f.sqlDB))
	return f
}

// assertStatuses fails unless every id has the wanted work_queue status.
func (f *reviveFixture) assertStatuses(t *testing.T, want string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		var s string
		if err := f.sqlDB.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("status of %d: %v", id, err)
		}
		if s != want {
			t.Errorf("row %d status = %q, want %q", id, s, want)
		}
	}
}

// pendingScans counts a library's scan_results reset to pending.
func (f *reviveFixture) pendingScans(t *testing.T, lib int64) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM scan_results WHERE library_id = ? AND status = 'pending'`
	if err := f.sqlDB.QueryRow(q, lib).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *reviveFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	return getQueue(t, f.mux, target, false)
}

var csrfFieldRE = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)

// reviveToken GETs the preview and returns the form's token and cookie.
func reviveToken(t *testing.T, f *reviveFixture, target string) (string, *http.Cookie) {
	t.Helper()
	rec := f.get(t, target)
	m := csrfFieldRE.FindStringSubmatch(rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName && m != nil {
			return m[1], c
		}
	}
	t.Fatalf("no csrf field or cookie in preview: %s", rec.Body.String())
	return "", nil
}

func postRevive(f *reviveFixture, library, expected, token string, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	form := url.Values{"library": {library}, "expected": {expected}}
	if token != "" {
		form.Set("csrf_token", token)
	}
	req := httptest.NewRequest(http.MethodPost, revivePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func TestRevivePreview(t *testing.T) {
	f := seedRevive(t)
	rec := f.get(t, revivePath)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, Cache-Control = %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	for _, want := range []string{
		"All libraries: 4 tracks, 1 shared by more than one",
		"0 not linked",
		"This will revive 4 tracks for all libraries",
		"<td>Alpha</td>", "<td>Bravo</td>",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("preview missing %q", want)
		}
	}
	// Alpha holds 3 (one shared); Bravo holds 2 (the same shared one).
	if body := f.get(t, revivePath+"?library="+strconv.FormatInt(f.libB, 10)).Body.String(); !strings.Contains(body, "This will revive 2 tracks for Bravo, 1 of them shared") {
		t.Errorf("library scope text missing: %s", body)
	}
	// The confirm form carries the previewed count.
	if body := f.get(t, revivePath+"?library="+strconv.FormatInt(f.libA, 10)).Body.String(); !strings.Contains(body, `name="expected" value="3"`) {
		t.Errorf("confirm form lacks the previewed count: %s", body)
	}
	for _, lib := range []string{"abc", "-1", "0"} {
		if rec := f.get(t, revivePath+"?library="+lib); rec.Code != http.StatusBadRequest {
			t.Errorf("library=%s status = %d, want 400", lib, rec.Code)
		}
	}
	f.assertStatuses(t, "unavailable", f.rowA1, f.rowA2, f.rowShared, f.rowB1) // GET never mutates
	if p := f.pendingScans(t, f.libA); p != 0 {
		t.Errorf("GET reset %d scan_results", p)
	}
}

func TestReviveLinkOnlyOnUnavailable(t *testing.T) {
	f := seedRevive(t)
	for b, want := range map[string]bool{"unavailable": true, "failed": false, "deferred": false, "pending": false} {
		if got := strings.Contains(f.get(t, "/queue/"+b).Body.String(), `href="`+revivePath+`"`); got != want {
			t.Errorf("%s view revive link = %v, want %v", b, got, want)
		}
	}
}

func TestRevivePostRefusals(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, revivePath)
	cases := []struct {
		name            string
		library, expect string
		token           string
		cookie          *http.Cookie
		hdr             map[string]string
		want            int
	}{
		{"no token and no cookie", "all", "4", "", nil, nil, http.StatusForbidden},
		{"cookie but no field", "all", "4", "", cookie, nil, http.StatusForbidden},
		{"mismatched token", "all", "4", strings.Repeat("a", 64), cookie, nil, http.StatusForbidden},
		{"cross-site", "all", "4", token, cookie, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-origin header", "all", "4", token, cookie, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"bad library", "nope", "4", token, cookie, nil, http.StatusBadRequest},
		{"zero library", "0", "4", token, cookie, nil, http.StatusBadRequest},
		{"empty expected", "all", "", token, cookie, nil, http.StatusBadRequest},
		{"non-numeric expected", "all", "x", token, cookie, nil, http.StatusBadRequest},
		{"negative expected", "all", "-1", token, cookie, nil, http.StatusBadRequest},
	}
	for _, c := range cases {
		if rec := postRevive(f, c.library, c.expect, c.token, c.cookie, c.hdr); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
	}
	f.assertStatuses(t, "unavailable", f.rowA1, f.rowA2, f.rowShared, f.rowB1)
}

func TestRevivePostScopedToLibrary(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, revivePath)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := postRevive(f, strconv.FormatInt(f.libA, 10), "3", token, cookie, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revived 3 tracks (Alpha)") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	f.assertStatuses(t, "deferred", f.rowA1, f.rowA2, f.rowShared, f.plainDeferred)
	f.assertStatuses(t, "unavailable", f.rowB1)
	if p := f.pendingScans(t, f.libB); p != 0 { // the shared row's B copy stays done
		t.Errorf("library B has %d pending scan_results, want 0", p)
	}

	audit := buf.String()
	if !strings.Contains(audit, "revive_retired") || !strings.Contains(audit, "revived=3") {
		t.Errorf("audit line missing: %q", audit)
	}
	for _, leak := range []string{"Secret", "Alpha", "/lib-a", "/secret"} {
		if strings.Contains(audit, leak) {
			t.Errorf("audit log leaks %q: %q", leak, audit)
		}
	}
}

func TestRevivePostAllRevivesEverything(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, revivePath)
	rec := postRevive(f, "all", "4", token, cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revived 4 tracks (all libraries)") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	f.assertStatuses(t, "deferred", f.rowA1, f.rowA2, f.rowShared, f.rowB1)
	if !strings.Contains(rec.Body.String(), "No retired tracks to revive") {
		t.Error("post-revive page should show the empty state")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("POST Cache-Control = %q, want no-store", got)
	}
}

func TestReviveUnwiredIs503(t *testing.T) {
	rec := getQueue(t, newReportsUIServer(t, openReportsTestDB(t)), revivePath, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET status = %d, want 503", rec.Code)
	}
}

func TestReviveStaleCountDoesNotRevive(t *testing.T) {
	f := seedRevive(t)
	lib := strconv.FormatInt(f.libA, 10)
	token, cookie := reviveToken(t, f, revivePath+"?library="+lib)
	f.addRow(t, "unavailable", reviveSentinel, f.libA) // population moves after the GET
	rec := postRevive(f, lib, "3", token, cookie, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "The counts changed since you loaded this page") {
		t.Fatalf("stale notice missing (status %d): %s", rec.Code, body)
	}
	if strings.Contains(body, "Revived ") {
		t.Error("stale POST reported a revive")
	}
	if !strings.Contains(body, "This will revive 4 tracks for Alpha") {
		t.Errorf("fresh count not shown: %s", body)
	}
	f.assertStatuses(t, "unavailable", f.rowA1, f.rowA2, f.rowShared)
}

func TestReviveUnlistedLibraryRerendersWithNotice(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, revivePath)
	// Revive Alpha, then replay the confirm: Alpha drops out of the listing.
	lib := strconv.FormatInt(f.libA, 10)
	if rec := postRevive(f, lib, "3", token, cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("first revive status = %d", rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"POST": postRevive(f, lib, "3", token, cookie, nil),
		"GET":  f.get(t, revivePath+"?library="+lib),
	} {
		body := rec.Body.String()
		if rec.Code != http.StatusOK ||
			!strings.Contains(body, "That library has no retired tracks left; the list has been refreshed.") ||
			!strings.Contains(body, "mx-status-error") {
			t.Errorf("%s: status %d, notice or error class missing: %s", name, rec.Code, body)
		}
	}
	f.assertStatuses(t, "unavailable", f.rowB1)
}

// busyOnce fails the first RecheckRetired with a real SQLITE_BUSY, then defers
// to the real queue.
type busyOnce struct {
	QueueActions
	busy  error
	calls int
}

func (b *busyOnce) RecheckRetired(ctx context.Context, id *int64) (int64, error) {
	b.calls++
	if b.calls == 1 {
		return 0, b.busy
	}
	return b.QueueActions.RecheckRetired(ctx, id)
}

// realBusy forces a genuine SQLITE_BUSY (the same recipe as the db package's own
// retry tests) so db.IsSQLiteBusy recognizes it.
func realBusy(t *testing.T) error {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "busy.db") + "?_pragma=busy_timeout(0)"
	open1 := func() *sql.DB {
		d, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		d.SetMaxOpenConns(1)
		return d
	}
	a, b := open1(), open1()
	ctx := context.Background()
	if _, err := a.ExecContext(ctx, "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	tx, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	_, berr := b.ExecContext(ctx, "INSERT INTO t (id) VALUES (2)")
	if berr == nil || !db.IsSQLiteBusy(berr) {
		t.Fatalf("could not provoke SQLITE_BUSY: %v", berr)
	}
	return berr
}

func TestReviveRetriesOnSQLiteBusy(t *testing.T) {
	f := seedRevive(t)
	flaky := &busyOnce{QueueActions: queue.NewDBQueue(f.sqlDB), busy: realBusy(t)}
	f.mux = reviveMux(f.sqlDB, flaky)

	token, cookie := reviveToken(t, f, revivePath)
	rec := postRevive(f, "all", "4", token, cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revived 4 tracks") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if flaky.calls != 2 {
		t.Errorf("RecheckRetired calls = %d, want 2 (one busy, one retry)", flaky.calls)
	}
}
