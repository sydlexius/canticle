package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/web/templates"
)

// marksRig is the preview player with the mark actions wired (#1250): a real
// library root and audio file, a session, and the fake mark services.
type marksRig struct {
	mux  *http.ServeMux
	db   *sql.DB
	root string
	sess *http.Cookie
	im   *fakeMarks
	bl   *fakeBlocks
}

func newMarksRig(t *testing.T) *marksRig {
	t.Helper()
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	root := t.TempDir()
	if _, err := sqlDB.ExecContext(context.Background(), `INSERT INTO libraries (path, name) VALUES (?, 'lib')`, root); err != nil {
		t.Fatal(err)
	}
	r := &marksRig{mux: http.NewServeMux(), db: sqlDB, root: root, im: &fakeMarks{}, bl: &fakeBlocks{},
		sess: &http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)}}
	ui := NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB)))
	ui.AttachMarkActions(MarkDeps{DB: sqlDB, Instrumental: r.im, Blocks: r.bl, DBPath: "/data/x.db"})
	ui.Register(r.mux)
	return r
}

// track seeds one row with an audio file and, when lrc is true, a line-synced
// sidecar. extra is SQL assignments applied after the insert.
func (r *marksRig) track(t *testing.T, name string, lrc bool, outcome, extra string) int64 {
	t.Helper()
	audio := filepath.Join(r.root, name+".flac")
	if err := os.WriteFile(audio, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if lrc {
		if err := os.WriteFile(filepath.Join(r.root, name+".lrc"), []byte(pageLRC), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	if err := r.db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, source_path, outcome_type)
		VALUES ('Invented Artist', ?, 'ia', ?, 'Al', 'done', ?, NULLIF(?, '')) RETURNING id`, name, name, audio, outcome).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		if _, err := r.db.Exec(`UPDATE work_queue SET `+extra+` WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (r *marksRig) page(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "198.51.100.40:1"
	req.AddCookie(r.sess)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func (r *marksRig) post(path string, form url.Values, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.RemoteAddr = "198.51.100.40:1"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(r.sess)
	if tok != "" {
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func has(body string, subs ...string) (missing string) {
	for _, s := range subs {
		if !strings.Contains(body, s) {
			return s
		}
	}
	return ""
}

func TestPreviewMarksOfferedPerState(t *testing.T) {
	r := newMarksRig(t)
	lyric := r.track(t, "with-lyric", true, "synced", "")
	word := r.track(t, "word-synced", true, "synced", "sync_tier = 'word'")
	none := r.track(t, "no-lyric", false, "", "")
	manual := r.track(t, "manual", false, "", "manual_instrumental_at = '2026-01-01T00:00:00Z'")
	blocked := r.track(t, "blocked", false, "", "")
	if _, err := r.db.Exec(`INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES ('ia', 'blocked', 'fp')`); err != nil {
		t.Fatal(err)
	}

	// A lyric that is an unsynced .txt only, and one whose .lrc is not readable
	// (a symlink): both are lyrics by the row's record, as on the list screens.
	txtOnly := r.track(t, "plain-song", false, "unsynced", "")
	if err := os.WriteFile(filepath.Join(r.root, "plain-song.txt"), []byte("Plain words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := r.track(t, "linked-lrc", false, "synced", "")
	if err := os.Symlink(filepath.Join(r.root, "plain-song.txt"), filepath.Join(r.root, "linked-lrc.lrc")); err != nil {
		t.Fatal(err)
	}
	instr := r.track(t, "detected-instrumental", false, "instrumental", "")

	cases := []struct {
		name         string
		id           int64
		want, absent []string
		lyricPanel   bool
	}{
		{"lyric", lyric, []string{`id="mx-mark-instrumental"`, `id="mx-mark-wrong"`}, []string{`mx-mark-unblock`, `mx-mark-instrumental-undo`}, true},
		{"word-synced read-only", word, []string{`id="mx-mark-instrumental"`, `id="mx-mark-wrong"`}, []string{`mx-mark-unblock`}, true},
		{"txt-only unsynced", txtOnly, []string{`id="mx-mark-instrumental"`, `id="mx-mark-wrong"`, `no synced lyric to play along with`}, []string{`No lyric is written`, `mx-mark-unblock`}, false},
		{"unreadable lrc symlink", linked, []string{`id="mx-mark-wrong"`, `no synced lyric to play along with`}, []string{`No lyric is written`}, false},
		{"instrumental outcome", instr, []string{`id="mx-mark-instrumental"`, `No lyric is written for this track.`}, []string{`mx-mark-wrong`}, false},
		{"no lyric", none, []string{`id="mx-mark-instrumental"`, `No lyric is written for this track.`}, []string{`mx-mark-wrong`, `mx-mark-unblock`}, false},
		{"marked instrumental", manual, []string{`id="mx-mark-instrumental-undo"`, `Marked instrumental by hand.`}, []string{`mx-mark-wrong`, `id="mx-mark-instrumental"`}, false},
		{"blocked", blocked, []string{`id="mx-mark-unblock"`, `These lyrics are blocked.`}, []string{`mx-mark-wrong`}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := r.page("/preview/" + strconv.FormatInt(c.id, 10))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if m := has(body, c.want...); m != "" {
				t.Errorf("body missing %q", m)
			}
			for _, a := range c.absent {
				if strings.Contains(body, a) {
					t.Errorf("body has %q", a)
				}
			}
			if got := strings.Contains(body, `id="mx-preview-lyrics"`); got != c.lyricPanel {
				t.Errorf("lyric panel present = %v, want %v", got, c.lyricPanel)
			}
			if !strings.Contains(body, `<audio id="mx-preview-audio"`) {
				t.Error("audio player missing")
			}
			// The audio error path (box, script) is on every variant; only the
			// no-lyric page declares that its lyric list is expected to be absent.
			for _, need := range []string{`id="mx-preview-audio-error"`, `<script src="/static/js/preview.js" defer></script>`} {
				if !strings.Contains(body, need) {
					t.Errorf("body missing %q", need)
				}
			}
			if got := strings.Contains(body, ` data-no-lyric=""`); got == c.lyricPanel {
				t.Errorf("data-no-lyric present = %v, want %v", got, !c.lyricPanel)
			}
			// The marks are links to the shared confirm pages, never the editor's controls.
			if strings.Contains(body, `id="mx-marks"`) && strings.Contains(body, `<button type="button" id="mx-mark`) {
				t.Error("mark controls rendered as editor-style buttons")
			}
		})
	}
}

func TestPreviewMarksLinksReturnToThePlayer(t *testing.T) {
	r := newMarksRig(t)
	id := strconv.FormatInt(r.track(t, "song", true, "synced", ""), 10)
	body := r.page("/preview/" + id + "?from=settled&mark=ignored&files=9").Body.String()
	want := `href="/queue/` + id + `/wrong?return=%2Fpreview%2F` + id + `%3Ffrom%3Dsettled"`
	if !strings.Contains(body, want) {
		t.Errorf("wrong link does not return to the player (want %s)", want)
	}
	// Only validated state rides along: junk keys, an oversized cursor, and the
	// one-shot mark/files parameters never reach the card's links.
	body = r.page("/preview/" + id + "?from=settled&q=abc&junk=zzz&after=" + strings.Repeat("9", 40) + "&mark=done&files=3").Body.String()
	card := body[strings.Index(body, `id="mx-marks"`):]
	for _, bad := range []string{"junk", "zzz", "mark%3D", "files%3D", "after%3D", "999999"} {
		if strings.Contains(card, bad) {
			t.Errorf("mark links carry %q", bad)
		}
	}
	if !strings.Contains(card, "q%3Dabc") {
		t.Error("validated search not carried into the mark links")
	}
	// Without a known bucket the return is the bare player path.
	body = r.page("/preview/" + id + "?from=bogus").Body.String()
	if !strings.Contains(body, `/wrong?return=%2Fpreview%2F`+id+`"`) {
		t.Error("bare return missing for an unknown origin")
	}
}

func TestPreviewMarksStatusLineAfterAction(t *testing.T) {
	r := newMarksRig(t)
	id := strconv.FormatInt(r.track(t, "manual", false, "", "manual_instrumental_at = '2026-01-01T00:00:00Z'"), 10)
	body := r.page("/preview/" + id + "?mark=" + templates.MarkInstrumentalDone).Body.String()
	if !strings.Contains(body, `class="mx-row-status"`) {
		t.Error("no status line after the action")
	}
	if strings.Contains(r.page("/preview/"+id+"?mark=%3Cb%3E").Body.String(), `class="mx-row-status"`) {
		t.Error("crafted mark value rendered a status line")
	}
}

func TestPreviewMarksUnwiredRendersNoSection(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", pageLRC)
	if strings.Contains(f.page(itoa(id)).Body.String(), `id="mx-marks"`) {
		t.Error("marks section rendered without the mark routes wired")
	}
}

func TestPreviewPageNoLyricStillNeedsAudioUnderARoot(t *testing.T) {
	r := newMarksRig(t)
	outside := filepath.Join(t.TempDir(), "x.flac")
	if err := os.WriteFile(outside, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := r.db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, source_path)
		VALUES ('A', 'T', 'a', 't', 'Al', 'done', ?) RETURNING id`, outside).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if rec := r.page("/preview/" + strconv.FormatInt(id, 10)); rec.Code != http.StatusNotFound {
		t.Errorf("no-lyric row with audio outside every root = %d, want 404", rec.Code)
	}
}

// The player reuses the queue mark routes with a return target back to the
// player. These prove each route is guarded on THAT path: no session, no
// CSRF token, and a cross-origin POST are all refused before any service runs,
// and a good POST lands back on the player with the result code.
func TestPreviewMarkRoutesGuardedOnThePlayerPath(t *testing.T) {
	r := newMarksRig(t)
	id := strconv.FormatInt(r.track(t, "song", true, "synced", ""), 10)
	ret := "/preview/" + id + "?from=settled"
	tok := strings.Repeat("a", 64)
	for _, p := range markPaths {
		t.Run(p, func(t *testing.T) {
			path := "/queue/" + id + "/" + p
			// No session: bounced to login.
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"csrf_token": {tok}, "return": {ret}}.Encode()))
			req.RemoteAddr = "198.51.100.40:1"
			rec := httptest.NewRecorder()
			r.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
				t.Errorf("no session = %d %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
			}
			// Session but a missing / mismatched token.
			if rec := r.post(path, url.Values{"return": {ret}}, ""); rec.Code != http.StatusForbidden {
				t.Errorf("no token = %d, want 403", rec.Code)
			}
			if rec := r.post(path, url.Values{"csrf_token": {strings.Repeat("b", 64)}, "return": {ret}}, tok); rec.Code != http.StatusForbidden {
				t.Errorf("mismatched token = %d, want 403", rec.Code)
			}
			// Cross-origin.
			req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"csrf_token": {tok}, "return": {ret}}.Encode()))
			req.RemoteAddr = "198.51.100.40:1"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "http://evil.example")
			req.AddCookie(r.sess)
			req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
			rec = httptest.NewRecorder()
			r.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-origin = %d, want 403", rec.Code)
			}
		})
	}
	if r.im.calls+r.bl.calls != 0 {
		t.Error("a service ran for a refused request")
	}
	for _, p := range markPaths {
		rec := r.post("/queue/"+id+"/"+p, url.Values{"csrf_token": {tok}, "return": {ret}}, tok)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/preview/"+id+"?") || !strings.Contains(loc, "from=settled") || !strings.Contains(loc, "mark=") {
			t.Errorf("POST %s = %d -> %q, want a redirect back to the player with a status", p, rec.Code, loc)
		}
	}
	// The confirm page's Cancel and form both carry the player as the return.
	body := r.page("/queue/" + id + "/wrong?return=" + url.QueryEscape(ret)).Body.String()
	if !strings.Contains(body, `name="return" value="/preview/`+id+`?from=settled"`) {
		t.Error("confirm page does not carry the player return")
	}
}
