package web

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/instrumentalmark"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/web/templates"
)

type fakeMarks struct {
	calls, dry int
	// err, when set, is returned by every call.
	err error
}

func (f *fakeMarks) note(dry bool) {
	if dry {
		f.dry++
		return
	}
	f.calls++
}

func (f *fakeMarks) Mark(_ context.Context, _ int64, o instrumentalmark.Options) (instrumentalmark.Result, error) {
	f.note(o.DryRun)
	if f.err != nil {
		return instrumentalmark.Result{}, f.err
	}
	return instrumentalmark.Result{Outcome: instrumentalmark.OutcomeMarked, FilesBackedUp: 2}, nil
}

func (f *fakeMarks) Unmark(_ context.Context, _ int64, o instrumentalmark.Options) (instrumentalmark.Result, error) {
	f.note(o.DryRun)
	if f.err != nil {
		return instrumentalmark.Result{}, f.err
	}
	return instrumentalmark.Result{Outcome: instrumentalmark.OutcomeUnmarked}, nil
}

type fakeBlocks struct {
	fakeMarks
	// none makes Unblock report that no block was removed.
	none bool
}

func (f *fakeBlocks) Mark(_ context.Context, r lyricblock.MarkRequest) (lyricblock.MarkResult, error) {
	f.note(r.DryRun)
	if f.err != nil {
		return lyricblock.MarkResult{}, f.err
	}
	return lyricblock.MarkResult{Files: 3}, nil
}

func (f *fakeBlocks) Unblock(_ context.Context, r lyricblock.UnblockRequest) (lyricblock.UnblockResult, error) {
	f.note(r.DryRun)
	if f.err != nil {
		return lyricblock.UnblockResult{}, f.err
	}
	if f.none {
		return lyricblock.UnblockResult{}, nil
	}
	return lyricblock.UnblockResult{Removed: 1}, nil
}

var markPaths = []string{"instrumental", "instrumental/undo", "wrong", "unblock"}

type markRig struct {
	mux   *http.ServeMux
	sess  *http.Cookie
	im    *fakeMarks
	bl    *fakeBlocks
	rowID string
}

func newMarkRig(t *testing.T, dbPath string) *markRig {
	t.Helper()
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	var id string
	if err := sqlDB.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
		VALUES ('Some Artist', 'Some Title', 'k', 'k', 'Al', 'done') RETURNING CAST(id AS TEXT)`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := &markRig{mux: http.NewServeMux(), im: &fakeMarks{}, bl: &fakeBlocks{}, rowID: id,
		sess: &http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)}}
	ui := NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB)))
	ui.AttachMarkActions(MarkDeps{DB: sqlDB, Instrumental: r.im, Blocks: r.bl, DBPath: dbPath})
	ui.Register(r.mux)
	return r
}

func (r *markRig) do(method, path string, form url.Values, session bool, csrf string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "198.51.100.40:1"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if session {
		req.AddCookie(r.sess)
		req.Header.Set("Origin", "http://example.com")
	}
	if csrf != "" {
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func TestMarkRoutesRefuseUnauthenticated(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	for _, p := range markPaths {
		t.Run(p, func(t *testing.T) {
			for _, m := range []string{http.MethodGet, http.MethodPost} {
				rec := r.do(m, "/queue/"+r.rowID+"/"+p, url.Values{}, false, "")
				if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
					t.Errorf("%s %s without a session = %d, want 303 /login", m, p, rec.Code)
				}
			}
		})
	}
	if r.im.calls+r.bl.calls+r.im.dry+r.bl.dry != 0 {
		t.Error("a service ran for an unauthenticated request")
	}
}

func TestMarkPostRefusesBadCSRF(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	tok := strings.Repeat("a", 64)
	for _, p := range markPaths {
		t.Run(p, func(t *testing.T) {
			path := "/queue/" + r.rowID + "/" + p
			cases := map[string]*httptest.ResponseRecorder{
				"no token":       r.do(http.MethodPost, path, url.Values{}, true, ""),
				"no cookie":      r.do(http.MethodPost, path, url.Values{"csrf_token": {tok}}, true, ""),
				"mismatch":       r.do(http.MethodPost, path, url.Values{"csrf_token": {strings.Repeat("b", 64)}}, true, tok),
				"short token":    r.do(http.MethodPost, path, url.Values{"csrf_token": {"abc"}}, true, "abc"),
				"cookie, no tok": r.do(http.MethodPost, path, url.Values{}, true, tok),
			}
			for name, rec := range cases {
				if rec.Code != http.StatusForbidden {
					t.Errorf("POST %s (%s) = %d, want 403", p, name, rec.Code)
				}
			}
		})
	}
	if r.im.calls+r.bl.calls != 0 {
		t.Error("a service ran without a valid CSRF token")
	}
}

func TestMarkPostRedirectsLocalOnly(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	tok := strings.Repeat("c", 64)
	for _, p := range markPaths {
		path := "/queue/" + r.rowID + "/" + p
		rec := r.do(http.MethodPost, path, url.Values{"csrf_token": {tok}, "return": {"/queue/finished?sort=title"}}, true, tok)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/queue/finished?") || !strings.Contains(loc, "mark=") || !strings.Contains(loc, "sort=title") {
			t.Errorf("POST %s = %d -> %q", p, rec.Code, loc)
		}
	}
	if r.im.calls != 2 || r.bl.calls != 2 {
		t.Errorf("service calls = %d/%d, want 2/2", r.im.calls, r.bl.calls)
	}
	for _, bad := range []string{"//evil.example/x", "https://evil.example/", "javascript:alert(1)", `/\evil.example`, ""} {
		rec := r.do(http.MethodPost, "/queue/"+r.rowID+"/unblock", url.Values{"csrf_token": {tok}, "return": {bad}}, true, tok)
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/queue?") {
			t.Errorf("return %q redirected to %q, want the /queue fallback", bad, loc)
		}
	}
}

func TestMarkRoutes404WithoutBackupLocation(t *testing.T) {
	r := newMarkRig(t, "")
	tok := strings.Repeat("d", 64)
	for _, p := range markPaths {
		path := "/queue/" + r.rowID + "/" + p
		if rec := r.do(http.MethodGet, path, nil, true, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
		if rec := r.do(http.MethodPost, path, url.Values{"csrf_token": {tok}}, true, tok); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestMarkConfirmPages(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	for _, p := range markPaths {
		rec := r.do(http.MethodGet, "/queue/"+r.rowID+"/"+p+"?return=/queue/finished&track=Forged+Name", nil, true, "")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, "Some Artist - Some Title") {
			t.Fatalf("GET %s = %d, track missing", p, rec.Code)
		}
		if strings.Contains(body, "Forged Name") {
			t.Errorf("%s printed a track name from the query string", p)
		}
		if !strings.Contains(body, `name="return" value="/queue/finished"`) || !strings.Contains(body, `name="csrf_token"`) {
			t.Errorf("%s form lacks return/csrf", p)
		}
	}
	wrong := r.do(http.MethodGet, "/queue/"+r.rowID+"/wrong", nil, true, "").Body.String()
	if !strings.Contains(wrong, "Lyric files that would be removed: 3.") || !strings.Contains(wrong, "never written for this track again") {
		t.Error("mark-wrong page lacks the dry-run count or the plain-words statement")
	}
	if r.bl.calls != 0 || r.im.calls != 0 {
		t.Error("a confirm page applied an action")
	}
	if rec := r.do(http.MethodGet, "/queue/999999/wrong", nil, true, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
}

func TestMarkPostRefusesCrossOrigin(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	tok := strings.Repeat("e", 64)
	signals := map[string][2]string{
		"Sec-Fetch-Site": {"Sec-Fetch-Site", "cross-site"},
		"Origin":         {"Origin", "http://evil.example"},
		"Referer":        {"Referer", "http://evil.example/page"},
	}
	for _, p := range markPaths {
		for name, h := range signals {
			t.Run(p+"/"+name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/queue/"+r.rowID+"/"+p, strings.NewReader(url.Values{"csrf_token": {tok}}.Encode()))
				req.RemoteAddr = "198.51.100.40:1"
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set(h[0], h[1])
				req.AddCookie(r.sess)
				req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
				rec := httptest.NewRecorder()
				r.mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Errorf("POST %s with cross-origin %s = %d, want 403", p, name, rec.Code)
				}
			})
		}
	}
	if r.im.calls+r.bl.calls != 0 {
		t.Error("a service ran for a cross-origin POST")
	}
}

func TestMarkUnblockNothingToDo(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	r.bl.none = true
	tok := strings.Repeat("f", 64)
	rec := r.do(http.MethodPost, "/queue/"+r.rowID+"/unblock", url.Values{"csrf_token": {tok}, "return": {"/queue"}}, true, tok)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.Contains(loc, "mark=nothing_to_do") {
		t.Errorf("POST unblock on an unblocked row = %d -> %q, want mark=nothing_to_do", rec.Code, loc)
	}
	get := r.do(http.MethodGet, "/queue/"+r.rowID+"/unblock", nil, true, "")
	body := get.Body.String()
	if get.Code != http.StatusOK || strings.Contains(body, `action="/queue/`+r.rowID+`/unblock"`) {
		t.Errorf("GET unblock on an unblocked row = %d, or it offers the unblock form", get.Code)
	}
	if !strings.Contains(body, "Nothing changed") {
		t.Error("GET unblock on an unblocked row lacks the nothing-to-do alert")
	}
}

func TestMarkConfirmNoStoreAndBadID(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	for _, p := range markPaths {
		rec := r.do(http.MethodGet, "/queue/"+r.rowID+"/"+p, nil, true, "")
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", p, cc)
		}
	}
	tok := strings.Repeat("9", 64)
	for _, id := range []string{"0", "-3", "abc"} {
		if rec := r.do(http.MethodGet, "/queue/"+id+"/wrong", nil, true, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET id %s = %d, want 404", id, rec.Code)
		}
		if rec := r.do(http.MethodPost, "/queue/"+id+"/wrong", url.Values{"csrf_token": {tok}}, true, tok); rec.Code != http.StatusNotFound {
			t.Errorf("POST id %s = %d, want 404", id, rec.Code)
		}
	}
	if r.bl.calls != 0 {
		t.Error("a service ran for a non-positive id")
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/queue/failed?x=1": "/queue/failed?x=1", "/dashboard": "/dashboard",
		"//h": "/queue", "http://h/": "/queue", "": "/queue", "queue": "/queue", "/a\nb": "/queue",
		`/\evil.example`: "/queue", `/\\evil.example`: "/queue", "//evil.example": "/queue",
		"/%5Cevil.example": "/%5Cevil.example", // encoded: never decoded into a host server-side, so local
		`/ok\x`:            "/queue", "/a\tb": "/queue", "/a\rb": "/queue", "/a\nb?x=1": "/queue",
		"/queue?filter=failed&page=2": "/queue?filter=failed&page=2",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMarkRoutesUseTheSessionGuard proves every route was registered through the
// guard wrapper (reg in UI.Register) that records its pattern, as the existing
// queue write actions are.
func TestMarkRoutesUseTheSessionGuard(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	ui := NewUI(config.Config{}, "vtest", WithAuth(a))
	ui.Register(http.NewServeMux())
	for _, p := range markPaths {
		for _, m := range []string{"GET", "POST"} {
			if pat := m + " /queue/{id}/" + p; !slices.Contains(ui.guardedRoutes, pat) {
				t.Errorf("%q is not registered through the session guard", pat)
			}
		}
	}
}

// TestMarkFailuresLeakNoPathOrTitle drives every action with a service error
// that embeds a temp path and a title (in a *fs.PathError and in plain text),
// then asserts neither reaches a log line, the redirect, or the confirm page.
func TestMarkFailuresLeakNoPathOrTitle(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "Hidden Secret Title.lrc")
	const secretTitle = "Hidden Secret Title"
	leaky := fmt.Errorf("work item 1: backup of %s failed: %w", secretTitle,
		&fs.PathError{Op: "open", Path: secretPath, Err: syscall.EACCES})
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	tok := strings.Repeat("7", 64)
	leaks := func(out string) bool {
		return strings.Contains(out, secretPath) || strings.Contains(out, secretTitle) || strings.Contains(out, filepath.Dir(secretPath))
	}

	for _, p := range markPaths {
		t.Run(p, func(t *testing.T) {
			r := newMarkRig(t, "/data/x.db")
			r.im.err, r.bl.err = leaky, leaky
			post := r.do(http.MethodPost, "/queue/"+r.rowID+"/"+p, url.Values{"csrf_token": {tok}, "return": {"/queue/finished"}}, true, tok)
			loc := post.Header().Get("Location")
			if post.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/queue/finished?") || !strings.Contains(loc, "mark="+templates.MarkFailed) {
				t.Errorf("POST %s = %d -> %q, want a redirect carrying mark=failed", p, post.Code, loc)
			}
			page := r.do(http.MethodGet, "/queue/"+r.rowID+"/"+p, nil, true, "").Body.String()
			if leaks(loc) || leaks(page) {
				t.Errorf("%s leaked the path or title to the browser", p)
			}
			if !strings.Contains(page, templates.MarkStatusText(templates.MarkFailed, 0)) {
				t.Errorf("GET %s did not say the action failed", p)
			}
		})
	}
	logs := buf.String()
	if !strings.Contains(logs, "error_class=") || !strings.Contains(logs, "permission denied") {
		t.Fatalf("no failure was logged with an errno class; the test observed nothing: %q", logs)
	}
	if leaks(logs) {
		t.Errorf("a log line leaked the path or title:\n%s", logs)
	}
}

// TestMarkOutcomesRedirectWithStatus covers each action's success, refusal and
// failure: the browser goes back to the local return target with a status, and
// the text shown for it never contains a path.
func TestMarkOutcomesRedirectWithStatus(t *testing.T) {
	pathy := &fs.PathError{Op: "open", Path: "/lib/Some Artist/Some Title.lrc", Err: syscall.ENOENT}
	busy := fmt.Errorf("x: %w", queue.ErrManualInstrumentalInFlight)
	tok := strings.Repeat("5", 64)
	cases := []struct {
		name, path, want string
		im, bl           error
	}{
		{"instrumental ok", "instrumental", templates.MarkInstrumentalDone, nil, nil},
		{"instrumental busy", "instrumental", templates.MarkInFlight, busy, nil},
		{"instrumental failed", "instrumental", templates.MarkFailed, pathy, nil},
		{"undo ok", "instrumental/undo", templates.MarkInstrumentalUndone, nil, nil},
		{"undo busy", "instrumental/undo", templates.MarkInFlight, busy, nil},
		{"undo failed", "instrumental/undo", templates.MarkFailed, pathy, nil},
		{"wrong ok", "wrong", templates.MarkWrongDone, nil, nil},
		{"wrong busy", "wrong", templates.MarkInFlight, nil, fmt.Errorf("x: %w", lyricblock.ErrBusy)},
		{"wrong failed", "wrong", templates.MarkFailed, nil, pathy},
		{"unblock ok", "unblock", templates.MarkUnblocked, nil, nil},
		{"unblock not found", "unblock", templates.MarkNotFound, nil, fmt.Errorf("x: %w", lyricblock.ErrNotFound)},
		{"unblock failed", "unblock", templates.MarkFailed, nil, pathy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newMarkRig(t, "/data/x.db")
			r.im.err, r.bl.err = c.im, c.bl
			rec := r.do(http.MethodPost, "/queue/"+r.rowID+"/"+c.path, url.Values{"csrf_token": {tok}, "return": {"/queue/failed?sort=title"}}, true, tok)
			loc := rec.Header().Get("Location")
			u, err := url.Parse(loc)
			if err != nil || rec.Code != http.StatusSeeOther || u.Path != "/queue/failed" || u.Query().Get("sort") != "title" {
				t.Fatalf("POST = %d -> %q, want a 303 back to /queue/failed?sort=title", rec.Code, loc)
			}
			code := u.Query().Get(templates.MarkStatusParam)
			if code != c.want {
				t.Errorf("status = %q, want %q", code, c.want)
			}
			if txt := templates.MarkStatusText(code, 0); txt == "" || strings.Contains(txt, "/") || strings.Contains(loc, "Some Title") || strings.Contains(loc, ".lrc") {
				t.Errorf("status text %q or redirect %q is empty or carries a path", txt, loc)
			}
		})
	}
}

// TestMarkStatusLineOnQueuePages: a known mark code renders its fixed line on
// the queue index and a bucket page; an unknown or absent code renders none, and
// a crafted value is never echoed.
func TestMarkStatusLineOnQueuePages(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	want := templates.MarkStatusText(templates.MarkInstrumentalDone, 0)
	for _, page := range []string{"/queue", "/queue/settled"} {
		for _, c := range []struct {
			q    string
			line bool
		}{
			{"?mark=" + templates.MarkInstrumentalDone, true},
			{"?mark=nonsense", false}, {"", false},
			{"?mark=%3Cscript%3Ealert(1)%3C/script%3E%2Fetc%2Fpasswd", false},
		} {
			body := r.do(http.MethodGet, page+c.q, nil, true, "").Body.String()
			if got := strings.Contains(body, `class="mx-row-status"`); got != c.line {
				t.Errorf("GET %s%s: status line present = %v, want %v", page, c.q, got, c.line)
			}
			if c.line && !strings.Contains(body, want) {
				t.Errorf("GET %s%s lacks %q", page, c.q, want)
			}
			if strings.Contains(body, "alert(1)") || strings.Contains(body, "/etc/passwd") {
				t.Errorf("GET %s%s echoed the crafted mark value", page, c.q)
			}
		}
	}
}

// TestMarkConfirmHonorsLocalReturn: a legitimate local return target, with or
// without its own query, reaches the form's hidden field unchanged.
func TestMarkConfirmHonorsLocalReturn(t *testing.T) {
	r := newMarkRig(t, "/data/x.db")
	for _, ret := range []string{"/queue/settled", "/queue/failed?sort=title&dir=desc"} {
		body := r.do(http.MethodGet, "/queue/"+r.rowID+"/wrong?return="+url.QueryEscape(ret), nil, true, "").Body.String()
		if want := `name="return" value="` + strings.ReplaceAll(ret, "&", "&amp;") + `"`; !strings.Contains(body, want) {
			t.Errorf("return %q not carried to the form (want %s)", ret, want)
		}
	}
}
