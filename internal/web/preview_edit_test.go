package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
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

// failingEditor records nothing: the file write succeeded, the DB did not.
type failingEditor struct{ *queue.DBQueue }

func (failingEditor) SetLyricEdit(context.Context, int64, int) error {
	return &fs.PathError{Op: "set", Path: "/music/Artist/Title.lrc", Err: errors.New("disk I/O error")}
}

// TestPreviewEditRecordFailure pins the post-write DB failure: a 500 with
// {"error":"record"}, the file already written, and a log line carrying the
// row id but never a path.
func TestPreviewEditRecordFailure(t *testing.T) {
	e := newEditEnv(t)
	e.ui.editor.Queue = failingEditor{e.q}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := e.post("/preview/"+e.id+"/offset", url.Values{"offset_ms": {"600"}, "mtime": {e.mtime(t)}})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"record"`) {
		t.Fatalf("record failure = %d %s, want 500 record", rec.Code, rec.Body)
	}
	if !strings.Contains(e.lrc(t), "[00:01.60]one") {
		t.Errorf("file not written before the record step:\n%s", e.lrc(t))
	}
	got := logs.String()
	if !strings.Contains(got, "id="+e.id) || strings.Contains(got, "/music") || strings.Contains(got, e.root) {
		t.Errorf("log must carry the id and no path: %q", got)
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
