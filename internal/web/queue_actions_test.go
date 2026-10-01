package web

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

const reviveSentinel = "miss limit reached"

type reviveFixture struct {
	sqlDB      *sql.DB
	mux        *http.ServeMux
	libA, libB int64
	// retiredA is two A-only rows plus the shared one; retiredB is the B-only row.
	rowA1, rowA2, rowShared, rowB1 int64
	// plainDeferred is a non-retired row that must never be touched.
	plainDeferred int64
}

func seedRevive(t *testing.T) reviveFixture {
	t.Helper()
	ctx := context.Background()
	sqlDB := openReportsTestDB(t)
	f := reviveFixture{sqlDB: sqlDB}
	mustScan := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := sqlDB.QueryRowContext(ctx, q+" RETURNING id", args...).Scan(&id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	f.libA = mustScan(`INSERT INTO libraries (path, name) VALUES ('/lib-a', 'Alpha')`)
	f.libB = mustScan(`INSERT INTO libraries (path, name) VALUES ('/lib-b', 'Bravo')`)
	n := 0
	row := func(status, lastErr string, libs ...int64) int64 {
		t.Helper()
		n++
		key := "k" + strconv.Itoa(n)
		id := mustScan(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error)
		                VALUES ('Secret Artist', 'Secret Title '||?, ?, ?, 'Album', ?, ?)`, key, key, key, status, lastErr)
		for _, lib := range libs {
			sr := mustScan(`INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
			                VALUES (?, 'Secret Artist', 'Secret Title', ?, 'out', 'song.lrc', 'done')`,
				lib, "/secret/path/"+key+"-"+strconv.FormatInt(lib, 10)+".flac")
			if _, err := sqlDB.ExecContext(ctx,
				`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, sr); err != nil {
				t.Fatalf("link: %v", err)
			}
		}
		return id
	}
	f.rowA1 = row("unavailable", reviveSentinel, f.libA)
	f.rowA2 = row("unavailable", reviveSentinel, f.libA)
	f.rowShared = row("unavailable", reviveSentinel, f.libA, f.libB)
	f.rowB1 = row("unavailable", reviveSentinel, f.libB)
	f.plainDeferred = row("deferred", "", f.libA)

	f.mux = http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	ui.AttachQueueActions(queue.NewDBQueue(sqlDB))
	ui.Register(f.mux)
	return f
}

func (f reviveFixture) status(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := f.sqlDB.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("status of %d: %v", id, err)
	}
	return s
}

func (f reviveFixture) scanStatuses(t *testing.T, lib int64) (pending, done int) {
	t.Helper()
	rows, err := f.sqlDB.Query(`SELECT status FROM scan_results WHERE library_id = ?`, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == "pending" {
			pending++
		} else {
			done++
		}
	}
	return pending, done
}

var csrfFieldRE = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)

// reviveToken GETs the preview and returns the form's token and cookie.
func reviveToken(t *testing.T, f reviveFixture, target string) (string, *http.Cookie) {
	t.Helper()
	rec := getQueue(t, f.mux, target, false)
	m := csrfFieldRE.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no csrf field in preview: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			return m[1], c
		}
	}
	t.Fatal("no csrf cookie set")
	return "", nil
}

func postRevive(f reviveFixture, library, token string, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	form := url.Values{"library": {library}}
	if token != "" {
		form.Set("csrf_token", token)
	}
	req := httptest.NewRequest(http.MethodPost, "/queue/unavailable/revive", strings.NewReader(form.Encode()))
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

func TestRevivePreviewCountsMatchSeed(t *testing.T) {
	f := seedRevive(t)
	rec := getQueue(t, f.mux, "/queue/unavailable/revive", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"All libraries: 4 tracks, 1 shared by more than one",
		"0 not linked",
		"This will revive 4 tracks for all libraries",
		"<td>Alpha</td>", "<td>Bravo</td>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q", want)
		}
	}
	// Alpha holds 3 (one shared); Bravo holds 2 (the same shared one).
	rec = getQueue(t, f.mux, "/queue/unavailable/revive?library="+strconv.FormatInt(f.libB, 10), false)
	if !strings.Contains(rec.Body.String(), "This will revive 2 tracks for Bravo, 1 of them shared") {
		t.Errorf("library scope text missing: %s", rec.Body.String())
	}
}

func TestRevivePreviewRejectsBadLibrary(t *testing.T) {
	f := seedRevive(t)
	for _, lib := range []string{"abc", "-1", "0", "999"} {
		if rec := getQueue(t, f.mux, "/queue/unavailable/revive?library="+lib, false); rec.Code != http.StatusBadRequest {
			t.Errorf("library=%s status = %d, want 400", lib, rec.Code)
		}
	}
}

func TestReviveGetNeverMutates(t *testing.T) {
	f := seedRevive(t)
	getQueue(t, f.mux, "/queue/unavailable/revive", false)
	getQueue(t, f.mux, "/queue/unavailable/revive?library="+strconv.FormatInt(f.libA, 10), false)
	if s := f.status(t, f.rowA1); s != "unavailable" {
		t.Errorf("GET changed row status to %q", s)
	}
	if p, _ := f.scanStatuses(t, f.libA); p != 0 {
		t.Errorf("GET reset %d scan_results", p)
	}
}

func TestReviveLinkOnlyOnUnavailable(t *testing.T) {
	f := seedRevive(t)
	if !strings.Contains(getQueue(t, f.mux, "/queue/unavailable", false).Body.String(), `href="/queue/unavailable/revive"`) {
		t.Error("unavailable view lacks the revive link")
	}
	for _, b := range []string{"failed", "deferred", "pending"} {
		if strings.Contains(getQueue(t, f.mux, "/queue/"+b, false).Body.String(), "/revive") {
			t.Errorf("%s view offers revive", b)
		}
	}
}

func TestRevivePostRefusals(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, "/queue/unavailable/revive")

	cases := []struct {
		name   string
		rec    *httptest.ResponseRecorder
		wantSt int
	}{
		{"no token and no cookie", postRevive(f, "all", "", nil, nil), http.StatusForbidden},
		{"cookie but no field", postRevive(f, "all", "", cookie, nil), http.StatusForbidden},
		{"mismatched token", postRevive(f, "all", strings.Repeat("a", 64), cookie, nil), http.StatusForbidden},
		{"cross-site", postRevive(f, "all", token, cookie, map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		{"cross-origin header", postRevive(f, "all", token, cookie, map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		{"bad library", postRevive(f, "nope", token, cookie, nil), http.StatusBadRequest},
		{"zero library", postRevive(f, "0", token, cookie, nil), http.StatusBadRequest},
	}
	for _, c := range cases {
		if c.rec.Code != c.wantSt {
			t.Errorf("%s: status = %d, want %d", c.name, c.rec.Code, c.wantSt)
		}
	}
	for _, id := range []int64{f.rowA1, f.rowA2, f.rowShared, f.rowB1} {
		if s := f.status(t, id); s != "unavailable" {
			t.Errorf("refused POST mutated row %d to %q", id, s)
		}
	}
}

func TestRevivePostScopedToLibrary(t *testing.T) {
	f := seedRevive(t)
	token, cookie := reviveToken(t, f, "/queue/unavailable/revive")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := postRevive(f, strconv.FormatInt(f.libA, 10), token, cookie, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Revived 3 tracks (Alpha)") {
		t.Errorf("result line missing: %s", rec.Body.String())
	}
	for _, id := range []int64{f.rowA1, f.rowA2, f.rowShared} {
		if s := f.status(t, id); s != "deferred" {
			t.Errorf("library A row %d status = %q, want deferred", id, s)
		}
	}
	if s := f.status(t, f.rowB1); s != "unavailable" {
		t.Errorf("B-only row status = %q, want untouched", s)
	}
	if s := f.status(t, f.plainDeferred); s != "deferred" {
		t.Errorf("plain deferred row status = %q", s)
	}
	// Library B's scan_results stay done, including the shared row's B copy.
	if p, d := f.scanStatuses(t, f.libB); p != 0 || d != 2 {
		t.Errorf("library B scan_results pending=%d done=%d, want 0/2", p, d)
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
	token, cookie := reviveToken(t, f, "/queue/unavailable/revive")
	rec := postRevive(f, "all", token, cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revived 4 tracks (all libraries)") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	for _, id := range []int64{f.rowA1, f.rowA2, f.rowShared, f.rowB1} {
		if s := f.status(t, id); s != "deferred" {
			t.Errorf("row %d status = %q", id, s)
		}
	}
	if !strings.Contains(rec.Body.String(), "No retired tracks to revive") {
		t.Error("post-revive page should show the empty state")
	}
}

func TestReviveUnwiredIs503(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)
	if rec := getQueue(t, mux, "/queue/unavailable/revive", false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET status = %d, want 503", rec.Code)
	}
}
