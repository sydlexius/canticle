package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

const editLRC = "[ti:x]\n[00:01.00]one\n[00:05.00]two\n[00:09.00]three\n"

var editToken = strings.Repeat("ab", csrfTokenLen/2)

// editEnv is one library root with a line-synced row whose audio has a known
// 30 s duration, served by a UI with the lyric editor attached.
type editEnv struct {
	*previewFixture
	ui    *UI
	q     *queue.DBQueue
	rowID int64
	id    string
	lrcP  string
}

func newEditEnv(t *testing.T) *editEnv {
	t.Helper()
	f := newPreviewFixture(t)
	e := &editEnv{previewFixture: f, q: queue.NewDBQueue(f.db)}
	e.ui = NewUI(config.Config{}, "v-test", WithReports(reports.New(f.db)))
	durs := audiodur.New(f.db, "test")
	e.ui.AttachLyricEditor(EditDeps{Queue: e.q, Durations: durs})
	f.mux = http.NewServeMux()
	e.ui.Register(f.mux)
	audio := f.writeFile(t, f.root, "song.flac")
	fi, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	if err := durs.Record(context.Background(), audio, fi.ModTime().UnixNano(), fi.Size(), 30); err != nil {
		t.Fatal(err)
	}
	e.lrcP = filepath.Join(f.root, "song.lrc")
	f.put(t, "song.lrc", editLRC)
	e.rowID = e.seedTier(t, audio, "line")
	e.id = itoa(e.rowID)
	return e
}

func (e *editEnv) seedTier(t *testing.T, audio, tier string) int64 {
	t.Helper()
	id := e.row(t, audio)
	if _, err := e.db.ExecContext(context.Background(),
		`UPDATE work_queue SET outcome_type = 'synced', sync_tier = ? WHERE id = ?`, tier, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *editEnv) mtime(t *testing.T) string {
	t.Helper()
	fi, err := os.Stat(e.lrcP)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

func (e *editEnv) lrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.lrcP)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *editEnv) hasOrig() bool {
	_, err := os.Lstat(e.lrcP + ".orig")
	return err == nil
}

func (e *editEnv) postWith(target string, vals url.Values, csrf bool, hdr ...string) *httptest.ResponseRecorder {
	if csrf {
		vals.Set("csrf_token", editToken)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: editToken})
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *editEnv) post(target string, vals url.Values) *httptest.ResponseRecorder {
	return e.postWith(target, vals, true)
}

func TestPreviewEditSaveAndRevert(t *testing.T) {
	e := newEditEnv(t)
	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var resp struct {
		OffsetMS    int   `json:"offset_ms"`
		MTime       int64 `json:"mtime"`
		CreatedOrig bool  `json:"created_orig"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if resp.OffsetMS != 600 || !resp.CreatedOrig || strconv.FormatInt(resp.MTime, 10) != e.mtime(t) {
		t.Errorf("response = %+v, want offset 600, created_orig, mtime %s", resp, e.mtime(t))
	}
	if strings.Contains(rec.Body.String(), e.root) || strings.Contains(rec.Body.String(), "song") {
		t.Errorf("response leaks a path: %s", rec.Body)
	}
	if !strings.Contains(e.lrc(t), "[00:01.60]one") || !strings.Contains(e.lrc(t), "[ti:x]") || !e.hasOrig() {
		t.Fatalf("save did not shift or back up:\n%s", e.lrc(t))
	}
	if off, edited, _ := e.q.LyricEdit(context.Background(), e.rowID); !edited || off != 600 {
		t.Errorf("row edit = %d/%v, want 600/true", off, edited)
	}

	// The returned mtime lets the page save again without a reload, and the
	// second offset applies to the original, not on top of the first.
	rec = e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"-500"}, "mtime": {strconv.FormatInt(resp.MTime, 10)}})
	if rec.Code != http.StatusOK || !strings.Contains(e.lrc(t), "[00:00.50]one") {
		t.Fatalf("second save = %d %s, lrc:\n%s", rec.Code, rec.Body, e.lrc(t))
	}

	rec = e.post("/preview/"+e.id+"/revert", url.Values{"mtime": {e.mtime(t)}})
	if rec.Code != http.StatusOK || !strings.Contains(e.lrc(t), "[00:01.00]one") || !e.hasOrig() {
		t.Fatalf("revert = %d %s, lrc:\n%s", rec.Code, rec.Body, e.lrc(t))
	}
	if _, edited, _ := e.q.LyricEdit(context.Background(), e.rowID); edited {
		t.Error("revert left the edit mark")
	}
}

func TestPreviewEditRefusals(t *testing.T) {
	e := newEditEnv(t)
	path := "/preview/" + e.id + "/offset"
	for _, tc := range []struct {
		name string
		vals url.Values
		csrf bool
		hdr  []string
		want int
	}{
		{"cross origin", url.Values{"offset_ms": {"100"}, "mtime": {e.mtime(t)}}, true, []string{"Origin", "https://evil.example"}, http.StatusForbidden},
		{"no csrf", url.Values{"offset_ms": {"100"}, "mtime": {e.mtime(t)}}, false, nil, http.StatusForbidden},
		{"offset not a number", url.Values{"offset_ms": {"abc"}, "mtime": {e.mtime(t)}}, true, nil, http.StatusBadRequest},
		{"offset too large", url.Values{"offset_ms": {"700000"}, "mtime": {e.mtime(t)}}, true, nil, http.StatusBadRequest},
		{"offset too small", url.Values{"offset_ms": {"-600001"}, "mtime": {e.mtime(t)}}, true, nil, http.StatusBadRequest},
		{"no mtime", url.Values{"offset_ms": {"100"}}, true, nil, http.StatusBadRequest},
		{"stale mtime", url.Values{"offset_ms": {"100"}, "mtime": {"1"}}, true, nil, http.StatusConflict},
		{"timing", url.Values{"offset_ms": {"60000"}, "mtime": {e.mtime(t)}}, true, nil, http.StatusUnprocessableEntity},
	} {
		if got := e.postWith(path, tc.vals, tc.csrf, tc.hdr...).Code; got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := e.post("/preview/"+e.id+"/revert", url.Values{"mtime": {e.mtime(t)}}); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "not_edited") {
		t.Errorf("revert of an unedited row: %d %s, want 409 not_edited", got.Code, got.Body)
	}
	if e.hasOrig() || e.lrc(t) != editLRC {
		t.Error("a refused save touched the file or created .lrc.orig")
	}
	if _, edited, _ := e.q.LyricEdit(context.Background(), e.rowID); edited {
		t.Error("a refused save marked the row edited")
	}

	// Non-editable rows are a bare 404, the same as an unknown id.
	wordID := e.seedTier(t, filepath.Join(e.root, "song.flac"), "word")
	remediated := e.seedTier(t, filepath.Join(e.root, "song.flac"), "line")
	if _, err := e.db.ExecContext(context.Background(),
		`UPDATE work_queue SET timing_outcome = 'mis_synced' WHERE id = ?`, remediated); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"word tier": itoa(wordID), "timing remediated": itoa(remediated), "unknown": "999999", "bad id": "x"} {
		for _, route := range []string{"offset", "revert"} {
			if got := e.post("/preview/"+id+"/"+route, url.Values{"offset_ms": {"100"}, "mtime": {e.mtime(t)}}).Code; got != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", name, route, got)
			}
		}
	}
}

// TestPreviewEditSerializesPerRow pins the per-row lock: while another save of
// the same row holds it, a request waits rather than racing ApplyEdit's mtime
// check (two concurrent rewrites of one .lrc would otherwise both pass it).
func TestPreviewEditSerializesPerRow(t *testing.T) {
	e := newEditEnv(t)
	unlock := e.ui.editLocks.lock(e.rowID)
	done := make(chan int, 1)
	go func() {
		done <- e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"100"}, "mtime": {e.mtime(t)}}).Code
	}()
	select {
	case code := <-done:
		unlock()
		t.Fatalf("save ran (%d) while another edit of the row held its lock", code)
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("save after unlock = %d, want 200", code)
	}
	if n := len(e.ui.editLocks.m); n != 0 {
		t.Errorf("lock map holds %d entries after release, want 0", n)
	}
}

// TestPreviewEditHoldsTheSidecarEditLock (#1226): a save and a revert both
// wait on the per-sidecar edit lock the timing sweep takes before it moves a
// file, so the sweep's mark re-check never sees a half-done edit.
func TestPreviewEditHoldsTheSidecarEditLock(t *testing.T) {
	e := newEditEnv(t)
	for _, step := range []struct {
		route string
		vals  url.Values
	}{
		{"/offset", url.Values{"offset_ms": {"100"}}},
		{"/revert", url.Values{}},
	} {
		unlock := lyrics.LockEditPath(e.lrcP)
		step.vals.Set("mtime", e.mtime(t))
		done := make(chan int, 1)
		go func() { done <- e.post("/preview/"+e.id+step.route, step.vals).Code }()
		select {
		case code := <-done:
			unlock()
			t.Fatalf("%s ran (%d) while the sidecar's edit lock was held", step.route, code)
		case <-time.After(150 * time.Millisecond):
		}
		unlock()
		if code := <-done; code != http.StatusOK {
			t.Fatalf("%s after unlock = %d, want 200", step.route, code)
		}
	}
}

// hookEditor wraps the real queue so a test can fail the mark or change the
// row at the moment the route touches it.
type hookEditor struct {
	*queue.DBQueue
	setErr      error
	onSet       func()
	onLyricEdit func()
}

func (h *hookEditor) SetLyricEdit(ctx context.Context, id int64, off int) error {
	if h.onSet != nil {
		h.onSet()
	}
	if h.setErr != nil {
		return h.setErr
	}
	return h.DBQueue.SetLyricEdit(ctx, id, off)
}

func (h *hookEditor) LyricEdit(ctx context.Context, id int64) (int, bool, error) {
	if h.onLyricEdit != nil {
		h.onLyricEdit()
	}
	return h.DBQueue.LyricEdit(ctx, id)
}

func captureLogs(t *testing.T) *lockedLog {
	t.Helper()
	logs := &lockedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// lockedLog is captureLogs' sink: a background goroutine may still be logging
// while the test reads, so every access takes the lock.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) do(f func()) { l.mu.Lock(); defer l.mu.Unlock(); f() }

func (l *lockedLog) Write(p []byte) (n int, err error) {
	l.do(func() { n, err = l.b.Write(p) })
	return n, err
}
func (l *lockedLog) String() (s string) { l.do(func() { s = l.b.String() }); return s }
func (l *lockedLog) Len() (n int)       { l.do(func() { n = l.b.Len() }); return n }
func (l *lockedLog) Reset()             { l.do(l.b.Reset) }

// TestPreviewEditRecordFailure pins the mark-first order: a save whose mark
// cannot be recorded writes nothing (no shift, no .orig), answers 500
// {"error":"record"}, and logs the row id but never a path.
func TestPreviewEditRecordFailure(t *testing.T) {
	e := newEditEnv(t)
	e.ui.editor.Queue = &hookEditor{DBQueue: e.q,
		setErr: &fs.PathError{Op: "set", Path: "/music/Artist/Title.lrc", Err: errors.New("disk I/O error")}}
	logs := captureLogs(t)

	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"record"`) {
		t.Fatalf("record failure = %d %s, want 500 record", rec.Code, rec.Body)
	}
	if e.lrc(t) != editLRC || e.hasOrig() {
		t.Errorf("an unrecorded save wrote the file (orig=%v):\n%s", e.hasOrig(), e.lrc(t))
	}
	got := logs.String()
	if !strings.Contains(got, "id="+e.id) || strings.Contains(got, "/music") || strings.Contains(got, e.root) {
		t.Errorf("log must carry the id and no path: %q", got)
	}
}

// TestPreviewEditFailureRestoresMark pins the rollback: a save marked first
// and then refused by the writer leaves the row's mark exactly as it was,
// whether the row was unedited or already carried an offset.
func TestPreviewEditFailureRestoresMark(t *testing.T) {
	for _, prior := range []int{0, 600} {
		e := newEditEnv(t)
		if prior != 0 {
			if rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {strconv.Itoa(prior)}, "mtime": {e.mtime(t)}}); rec.Code != http.StatusOK {
				t.Fatalf("seed save = %d %s", rec.Code, rec.Body)
			}
		}
		for _, tc := range []struct {
			name string
			vals url.Values
			want int
		}{
			{"stale mtime", url.Values{"offset_ms": {"100"}, "mtime": {"1"}}, http.StatusConflict},
			{"timing", url.Values{"offset_ms": {"60000"}, "mtime": {e.mtime(t)}}, http.StatusUnprocessableEntity},
		} {
			if got := e.post("/preview/"+e.id+"/offset", tc.vals).Code; got != tc.want {
				t.Fatalf("prior %d, %s: %d, want %d", prior, tc.name, got, tc.want)
			}
			off, edited, err := e.q.LyricEdit(context.Background(), e.rowID)
			if err != nil || edited != (prior != 0) || off != prior {
				t.Errorf("prior %d, %s: mark = %d/%v (%v), want it restored", prior, tc.name, off, edited, err)
			}
		}
	}
}

// TestPreviewEditRecheckRefusesAdmittedRow pins the re-check after the mark: a
// row a sweep admitted between the first check and the mark (here, flipped to
// word-recheck 'queued' at that moment) is refused 409 busy, nothing is
// written, and the mark is restored. A revert re-checks the same way.
func TestPreviewEditRecheckRefusesAdmittedRow(t *testing.T) {
	e := newEditEnv(t)
	admit := func() {
		if _, err := e.db.ExecContext(context.Background(),
			`UPDATE work_queue SET status = 'deferred', word_timing_state = 'queued' WHERE id = ?`, e.rowID); err != nil {
			t.Fatal(err)
		}
	}
	e.ui.editor.Queue = &hookEditor{DBQueue: e.q, onSet: admit}
	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("admitted row save = %d %s, want 409 busy", rec.Code, rec.Body)
	}
	if e.lrc(t) != editLRC || e.hasOrig() {
		t.Errorf("a refused save wrote the file:\n%s", e.lrc(t))
	}
	if _, edited, _ := e.q.LyricEdit(context.Background(), e.rowID); edited {
		t.Error("a refused save left the row marked")
	}

	// Revert: a recorded edit, then the row is admitted before the re-check.
	e2 := newEditEnv(t)
	if rec := e2.post("/preview/"+e2.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e2.mtime(t)}}); rec.Code != http.StatusOK {
		t.Fatalf("seed save = %d %s", rec.Code, rec.Body)
	}
	shifted := e2.lrc(t)
	e2.ui.editor.Queue = &hookEditor{DBQueue: e2.q, onLyricEdit: func() {
		if _, err := e2.db.ExecContext(context.Background(),
			`UPDATE work_queue SET status = 'processing' WHERE id = ?`, e2.rowID); err != nil {
			t.Fatal(err)
		}
	}}
	rec = e2.post("/preview/"+e2.id+"/revert", url.Values{"mtime": {e2.mtime(t)}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("admitted row revert = %d %s, want 409 busy", rec.Code, rec.Body)
	}
	if e2.lrc(t) != shifted {
		t.Errorf("a refused revert wrote the file:\n%s", e2.lrc(t))
	}
	if off, edited, _ := e2.q.LyricEdit(context.Background(), e2.rowID); !edited || off != 600 {
		t.Errorf("a refused revert changed the mark: %d/%v, want 600/true", off, edited)
	}
}

// TestPreviewEditDurationLookupLogIsPathFree pins finding 4: audiodur's lookup
// error embeds the queried audio path (not as a *fs.PathError), and the
// route's warning must still carry only the row id.
func TestPreviewEditDurationLookupLogIsPathFree(t *testing.T) {
	e := newEditEnv(t)
	closed, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	e.ui.editor.Durations = audiodur.New(closed, "test")
	logs := captureLogs(t)

	if rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}}); rec.Code != http.StatusOK {
		t.Fatalf("save with a failing duration store = %d %s, want 200 (fail open)", rec.Code, rec.Body)
	}
	got := logs.String()
	if !strings.Contains(got, "duration lookup failed") || !strings.Contains(got, "id="+e.id) {
		t.Fatalf("no id-tagged duration warning logged: %q", got)
	}
	if strings.Contains(got, e.root) || strings.Contains(got, "song") {
		t.Errorf("duration warning leaks the audio path: %q", got)
	}
}

func TestPreviewEditNotAttached(t *testing.T) {
	f := newPreviewFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/preview/1/offset", strings.NewReader("csrf_token="+editToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: editToken})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no editor attached: %d, want 404", rec.Code)
	}
}

func (e *editEnv) accept(lines, mtime string) *httptest.ResponseRecorder {
	return e.post("/preview/"+e.id+"/auto/accept", url.Values{"lines": {lines}, "mtime": {mtime}})
}

// mark is the row's edit mark as "<offset or null>/<edited 0|1>".
func (e *editEnv) mark(t *testing.T) (s string) {
	t.Helper()
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(lyric_offset_ms, 'null') || '/' || (lyric_edited_at IS NOT NULL) FROM work_queue WHERE id = ?`, e.rowID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPreviewAutoAccept pins the #1008 accept: posted line starts land in the
// .lrc with the generated marker, the original is backed up once, the row is
// marked with no offset, and no text in the request can reach the file.
func TestPreviewAutoAccept(t *testing.T) {
	e := newEditEnv(t)
	rec := e.post("/preview/"+e.id+"/auto/accept", url.Values{
		"lines": {"[1200,5400,9100]"}, "words": {"[[[0,1200]],[],[[0,9100]]]"},
		"mtime": {e.mtime(t)}, "text": {"smuggled"}, "offset_ms": {"777"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created_orig":true`) {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	const want = "[ti:x]\n[timing:canticle-aligner]\n[00:01.20]one\n[00:05.40]two\n[00:09.10]three\n"
	if got := e.lrc(t); got != want {
		t.Errorf("accepted file:\n%q\nwant:\n%q", got, want)
	}
	if b, err := os.ReadFile(e.lrcP + ".orig"); err != nil || string(b) != editLRC {
		t.Errorf(".orig = %q (%v), want the original bytes", b, err)
	}
	if got := e.mark(t); got != "null/1" {
		t.Errorf("mark = %s, want null/1 (edited, no offset)", got)
	}
	// A second accept keeps the first backup and a single marker.
	if rec := e.accept("[1000,5000,9000]", e.mtime(t)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created_orig":false`) {
		t.Fatalf("second accept = %d %s", rec.Code, rec.Body)
	}
	if got := e.lrc(t); strings.Count(got, "[timing:") != 1 || !strings.Contains(got, "[00:01.00]one") {
		t.Errorf("second accept:\n%s", got)
	}
	if b, _ := os.ReadFile(e.lrcP + ".orig"); string(b) != editLRC {
		t.Errorf(".orig overwritten: %q", b)
	}
}

// TestPreviewAutoAcceptRefusals pins each refusal's status and that it leaves
// the file, its mtime, the absent .orig and the row's mark exactly as found.
func TestPreviewAutoAcceptRefusals(t *testing.T) {
	const ok = "[1200,5400,9100]"
	for _, prior := range []string{"none", "offset", "retime"} {
		e := newEditEnv(t)
		switch prior {
		case "offset":
			if err := e.q.SetLyricEdit(context.Background(), e.rowID, 250); err != nil {
				t.Fatal(err)
			}
		case "retime":
			if err := e.q.SetLyricRetime(context.Background(), e.rowID); err != nil {
				t.Fatal(err)
			}
		}
		wantMark := e.mark(t)
		path, mt := "/preview/"+e.id+"/auto/accept", e.mtime(t)
		for _, tc := range []struct {
			name  string
			vals  url.Values
			csrf  bool
			hdr   []string
			setup func() (undo func())
			want  int
			body  string
		}{
			{name: "cross origin", vals: url.Values{"lines": {ok}}, csrf: true, hdr: []string{"Origin", "https://evil.example"}, want: http.StatusForbidden},
			{name: "no csrf", vals: url.Values{"lines": {ok}}, want: http.StatusForbidden},
			{name: "lines missing", vals: url.Values{}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "lines not numbers", vals: url.Values{"lines": {`["a",1,2]`}}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "wrong count", vals: url.Values{"lines": {"[1200,5400]"}}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "negative", vals: url.Values{"lines": {"[-1,5400,9100]"}}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "decreasing", vals: url.Values{"lines": {"[1200,900,9100]"}}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "words malformed", vals: url.Values{"lines": {ok}, "words": {"{"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "word token out of range", vals: url.Values{"lines": {ok}, "words": {"[[[1,1200]],[],[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "word time negative", vals: url.Values{"lines": {ok}, "words": {"[[[0,-4]],[],[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "null line", vals: url.Values{"lines": {"[null,5400,9100]"}}, csrf: true, want: http.StatusBadRequest, body: `"lines"`},
			{name: "null word pair", vals: url.Values{"lines": {ok}, "words": {"[[[0,1200]],null,[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "null word time", vals: url.Values{"lines": {ok}, "words": {"[[[0,null]],[],[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "words for too few lines", vals: url.Values{"lines": {ok}, "words": {"[[[0,1200]],[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "word token repeated", vals: url.Values{"lines": {ok}, "words": {"[[[0,1200],[0,1300]],[],[]]"}}, csrf: true, want: http.StatusBadRequest, body: `"words"`},
			{name: "no mtime", vals: url.Values{"lines": {ok}, "mtime": {""}}, csrf: true, want: http.StatusBadRequest, body: `"mtime"`},
			{name: "stale mtime", vals: url.Values{"lines": {ok}, "mtime": {"1"}}, csrf: true, want: http.StatusConflict, body: `"changed"`},
			{name: "timing guard", vals: url.Values{"lines": {"[1200,5400,95000]"}}, csrf: true, want: http.StatusUnprocessableEntity, body: `"timing"`},
			{name: "word companion beside it", vals: url.Values{"lines": {ok}}, csrf: true, want: http.StatusConflict, body: `"has_words"`, setup: func() func() {
				p := filepath.Join(e.root, "song.elrc")
				if err := os.WriteFile(p, []byte("[by:someone]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() { _ = os.Remove(p) }
			}},
		} {
			undo := func() {}
			if tc.setup != nil {
				undo = tc.setup()
			}
			if _, set := tc.vals["mtime"]; !set {
				tc.vals.Set("mtime", mt)
			}
			rec := e.postWith(path, tc.vals, tc.csrf, tc.hdr...)
			undo()
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("prior %s, %s: %d %s, want %d %s", prior, tc.name, rec.Code, rec.Body, tc.want, tc.body)
			}
			if e.lrc(t) != editLRC || e.mtime(t) != mt || e.hasOrig() {
				t.Fatalf("prior %s, %s: a refused accept touched the file (orig=%v)", prior, tc.name, e.hasOrig())
			}
			if got := e.mark(t); got != wantMark {
				t.Errorf("prior %s, %s: mark = %s, want it restored to %s", prior, tc.name, got, wantMark)
			}
		}
	}

	// Rows the offset editor refuses are the same bare 404 here, and a file
	// that already carries inline word marks is a 409.
	e := newEditEnv(t)
	wordID := e.seedTier(t, filepath.Join(e.root, "song.flac"), "word")
	for name, id := range map[string]string{"word tier": itoa(wordID), "unknown": "999999", "bad id": "x"} {
		if got := e.post("/preview/"+id+"/auto/accept", url.Values{"lines": {ok}, "mtime": {e.mtime(t)}}).Code; got != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, got)
		}
	}
	inline := "[00:01.00]<00:01.00>one <00:01.40>more\n[00:05.00]two\n[00:09.00]three\n"
	e.put(t, "song.lrc", inline)
	e.put(t, "song.lrc.orig", editLRC) // a wordless backup: only the current file's marks can refuse
	if rec := e.accept(ok, e.mtime(t)); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "has_words") {
		t.Errorf("inline words: %d %s, want 409 has_words", rec.Code, rec.Body)
	}
	if e.lrc(t) != inline {
		t.Error("an accept over inline word marks touched the file")
	}
}

// TestPreviewAutoAcceptStaleBackup pins that a leftover .orig plays no part in
// an accept: the starts are validated against, and land on, the CURRENT file's
// cues by position, with its text and tags, whatever the backup's text, tags,
// order, grouping or cue count. Only the request's own count can refuse.
func TestPreviewAutoAcceptStaleBackup(t *testing.T) {
	const ok, cues = "[1200,5400,9100]", "[00:01.00]one\n[00:05.00]two\n[00:09.00]three\n"
	const out = "[timing:canticle-aligner]\n[00:01.20]one\n[00:05.40]two\n[00:09.10]three\n"
	const grouped = "[ti:x]\n[00:01.00]one\n[00:01.00]two\n[00:09.00]three\n"
	for _, tc := range []struct {
		name, cur, orig, lines, words string
		want                          string // the file after a 200; "" means 400 lines, nothing touched
	}{
		// The word names a token only the current text has (the .orig line is empty).
		{name: "stale text", cur: editLRC, orig: "[ti:x]\n[00:01.00]OLD one\n[00:05.00]\n[00:09.00]OLD three\n",
			lines: ok, words: "[[],[[0,5400]],[]]", want: "[ti:x]\n" + out},
		// [re:canticle] injected after the .orig was made, and a tag only the .orig has.
		{name: "stale tags", cur: "[ti:x]\n[re:canticle]\n" + cues, orig: "[ti:x]\n[source:lane-old]\n" + cues, lines: ok,
			want: "[ti:x]\n[re:canticle]\n" + out},
		{name: "other order", cur: editLRC, orig: "[ti:x]\n[00:01.00]three\n[00:05.00]one\n[00:09.00]two\n", lines: ok, want: "[ti:x]\n" + out},
		// Equal counts, but the .orig groups cues 2+3 where the file groups 1+2.
		{name: "other grouping", cur: grouped, orig: "[ti:x]\n[00:01.00]one\n[00:05.00]two\n[00:05.00]three\n", lines: "[1200,1200,9100]",
			want: "[ti:x]\n[timing:canticle-aligner]\n[00:01.20]one\n[00:01.20]two\n[00:09.10]three\n"},
		{name: "splits the file's group", cur: grouped, orig: editLRC, lines: ok},
		{name: "cue count differs", cur: editLRC, orig: editLRC + "[00:09.50]four\n", lines: ok, want: "[ti:x]\n" + out},
		{name: "sized for the backup", cur: editLRC, orig: editLRC + "[00:09.50]four\n", lines: "[1200,5400,9100,9600]"},
	} {
		e := newEditEnv(t)
		e.put(t, "song.lrc", tc.cur)
		e.put(t, "song.lrc.orig", tc.orig)
		mt := e.mtime(t)
		rec := e.post("/preview/"+e.id+"/auto/accept", url.Values{"lines": {tc.lines}, "words": {tc.words}, "mtime": {mt}})
		if b, _ := os.ReadFile(e.lrcP + ".orig"); string(b) != tc.orig {
			t.Errorf("%s: .orig rewritten: %q", tc.name, b)
		}
		if tc.want != "" {
			if got := e.lrc(t); rec.Code != http.StatusOK || got != tc.want {
				t.Errorf("%s: %d %s wrote:\n%q\nwant the current file retimed:\n%q", tc.name, rec.Code, rec.Body, got, tc.want)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"lines"`) || e.lrc(t) != tc.cur || e.mtime(t) != mt || e.mark(t) != "null/0" {
			t.Errorf("%s: %d %s, want 400 lines with the file and mark untouched (%s): %q", tc.name, rec.Code, rec.Body, e.mark(t), e.lrc(t))
		}
	}

	e := newEditEnv(t)
	if rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}}); rec.Code != http.StatusOK {
		t.Fatalf("offset = %d %s", rec.Code, rec.Body)
	}
	if rec := e.accept(ok, e.mtime(t)); rec.Code != http.StatusOK {
		t.Fatalf("accept after an offset edit = %d %s", rec.Code, rec.Body)
	}
	if got, want := e.lrc(t), "[ti:x]\n[timing:canticle-aligner]\n[00:01.20]one\n[00:05.40]two\n[00:09.10]three\n"; got != want {
		t.Errorf("accept after an offset edit wrote:\n%q\nwant:\n%q", got, want)
	}
}

// TestPreviewAutoAcceptLargeBody pins the accept's own body cap: one start per
// line of a long file is several times the 4 KiB the offset form allows.
func TestPreviewAutoAcceptLargeBody(t *testing.T) {
	e := newEditEnv(t)
	var file strings.Builder
	starts := make([]string, 1000)
	for i := range starts {
		fmt.Fprintf(&file, "[00:%02d.%02d]w\n", i/50, i%50*2)
		starts[i] = strconv.Itoa(i*20 + 10)
	}
	e.put(t, "song.lrc", file.String())
	lines := "[" + strings.Join(starts, ",") + "]"
	if len(lines) <= editMaxBody {
		t.Fatalf("body is %d bytes, not over the %d byte offset cap", len(lines), editMaxBody)
	}
	if rec := e.accept(lines, e.mtime(t)); rec.Code != http.StatusOK {
		t.Fatalf("large accept = %d %s", rec.Code, rec.Body)
	}
	if got := e.lrc(t); !strings.Contains(got, "[00:19.99]w\n") {
		t.Error("the last line's stamp was not written")
	}
}

// A stale .orig of equal line count must refuse the save with 409 and leave
// the current file byte for byte alone (#1313).
func TestPreviewEditRefusesStaleOrig(t *testing.T) {
	e := newEditEnv(t)
	stale := "[ti:y]\n[00:01.00]uno\n[00:05.00]dos\n[00:09.00]tres\n"
	if err := os.WriteFile(e.lrcP+".orig", []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stale_orig") {
		t.Fatalf("save = %d %s, want 409 stale_orig", rec.Code, rec.Body)
	}
	if e.lrc(t) != editLRC {
		t.Errorf("a refused save rewrote the file:\n%s", e.lrc(t))
	}
	if _, edited, _ := e.q.LyricEdit(context.Background(), e.rowID); edited {
		t.Error("a refused save left the edit mark")
	}
}

// The handler must write the [offset:] header (#1385): the lyrics tests call
// WithOffsetTag themselves, so only this one covers the wiring.
func TestPreviewEditWritesAndReplacesOffsetTag(t *testing.T) {
	e := newEditEnv(t)
	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if got := strings.Count(e.lrc(t), "[offset:"); got != 1 || !strings.Contains(e.lrc(t), "[offset:600]") {
		t.Fatalf("first save: want exactly one [offset:600], got %d:\n%s", got, e.lrc(t))
	}
	rec = e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"-500"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("second save = %d %s", rec.Code, rec.Body)
	}
	if got := strings.Count(e.lrc(t), "[offset:"); got != 1 || !strings.Contains(e.lrc(t), "[offset:-500]") || strings.Contains(e.lrc(t), "[offset:600]") {
		t.Fatalf("second save: want exactly one [offset:-500] replacing 600, got %d:\n%s", got, e.lrc(t))
	}
}
