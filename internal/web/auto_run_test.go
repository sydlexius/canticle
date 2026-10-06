package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/aligner"
	"github.com/sydlexius/canticle/internal/lyrics"
)

func (f *fakeAuto) AlignFile(context.Context, io.Reader, []string) (aligner.Result, error) {
	return aligner.Result{}, errors.New("not scripted")
}

// runFake is an AutoAligner whose AlignFile blocks on hold (when set) until it
// is closed or the run's context ends (deaf: only until it is closed), then
// answers err or two words.
type runFake struct {
	fakeAuto
	mu     sync.Mutex
	aligns atomic.Int32
	audio  string
	lines  []string
	err    error
	hold   chan struct{}
	deaf   bool
	pre    func() // runs first
}

func (f *runFake) AlignFile(ctx context.Context, audio io.Reader, lines []string) (aligner.Result, error) {
	if f.pre != nil {
		f.pre()
	}
	b, _ := io.ReadAll(audio)
	f.mu.Lock()
	f.aligns.Add(1)
	f.audio, f.lines = string(b), lines
	err, hold := f.err, f.hold
	f.mu.Unlock()
	if hold != nil && f.deaf {
		<-hold
	} else if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return aligner.Result{}, fmt.Errorf("aligner: %w", ctx.Err())
		}
	}
	return aligner.Result{Words: make([]aligner.Word, 2)}, err
}

func (f *runFake) script(err error, hold chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.hold = err, hold
}

type autoEnv struct {
	*editEnv
	url string
}

// newAutoEnv attaches fake to an edit fixture. Its cleanup cancels and waits
// for every run, so no test leaves a goroutine behind.
func newAutoEnv(t *testing.T, fake *runFake) *autoEnv {
	t.Helper()
	e := &autoEnv{editEnv: newEditEnv(t)}
	e.url = "/preview/" + e.id + "/auto"
	e.ui.AttachAutoAligner(fake, 1)
	waitFor(t, "the priming probe", func() bool { return e.ui.auto.checkedAt.Load() != 0 && !e.ui.auto.refreshing.Load() })
	t.Cleanup(func() {
		e.ui.auto.runs.mu.Lock()
		all := slices.Collect(maps.Values(e.ui.auto.runs.m))
		e.ui.auto.runs.mu.Unlock()
		for _, r := range all {
			r.cancel()
			<-r.done
		}
	})
	return e
}

// touch moves the .lrc mtime hours on, as a rewrite would.
func (e *autoEnv) touch(hours time.Duration) {
	later := time.Now().Add(hours * time.Hour)
	_ = os.Chtimes(e.lrcP, later, later)
}

// run is the fixture row's registered run, or nil.
func (e *autoEnv) run() *autoRun {
	e.ui.auto.runs.mu.Lock()
	defer e.ui.auto.runs.mu.Unlock()
	return e.ui.auto.runs.m[e.rowID]
}

func (e *autoEnv) start(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return e.post(e.url, url.Values{"mtime": {e.mtime(t)}})
}

// poll GETs the run and decodes the JSON body (nil for a non-JSON answer).
func (e *autoEnv) poll() (int, map[string]any) {
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, e.url, nil))
	var body map[string]any
	dec := json.NewDecoder(rec.Body)
	dec.UseNumber() // the mtime is unix nanoseconds, beyond a float64
	_ = dec.Decode(&body)
	return rec.Code, body
}

func (e *autoEnv) waitState(t *testing.T, want string) map[string]any {
	t.Helper()
	var body map[string]any
	waitFor(t, "run state "+want, func() bool {
		_, body = e.poll()
		return body["state"] == want
	})
	if want != autoRunning {
		<-e.run().done // the goroutine has ended: its last log line is written
	}
	return body
}

func TestAutoRunStartAttachPollDone(t *testing.T) {
	fake := &runFake{hold: make(chan struct{})}
	e := newAutoEnv(t, fake)
	// A same-stamp follower and a decorative line are sent blank.
	e.put(t, "song.lrc", "[00:01.00]one\n[00:01.00]uno\n[00:05.00]♪\n[00:09.00]three\n")
	logs := captureLogs(t)
	before, mtime := e.lrc(t), e.mtime(t)
	for i := range 3 {
		if rec := e.start(t); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"running"`) {
			t.Fatalf("start %d = %d %s, want 202 running", i, rec.Code, rec.Body)
		}
	}
	close(fake.hold)
	body := e.waitState(t, autoDone)
	if got := fake.aligns.Load(); got != 1 {
		t.Fatalf("sidecar calls after three starts of one track = %d, want 1", got)
	}
	if body["mtime"] != json.Number(mtime) || body["aligned_words"] != json.Number("2") {
		t.Errorf("done payload = %v, want mtime %s and aligned_words 2", body, mtime)
	}
	if want := []string{"one", "", "", "three"}; !slices.Equal(fake.lines, want) || fake.audio != string(previewBytes) {
		t.Errorf("sent lines %q audio %q, want %q and the row's audio bytes", fake.lines, fake.audio, want)
	}
	if e.lrc(t) != before || e.hasOrig() {
		t.Error("a run changed the .lrc or created a .orig; it must write nothing")
	}
	if l := logs.String(); !strings.Contains(l, "run finished") || strings.Contains(l, "song") || strings.Contains(l, "three") {
		t.Errorf("logs must name the finished run and carry no path or lyric text:\n%s", l)
	}
}

func TestAutoRunRefusals(t *testing.T) {
	fake := &runFake{}
	e := newAutoEnv(t, fake)
	e.put(t, "word.lrc", editLRC)
	wordID := itoa(e.seedTier(t, e.writeFile(t, e.root, "word.flac"), "word"))
	good := url.Values{"mtime": {e.mtime(t)}}
	check := func(name string, rec *httptest.ResponseRecorder, code int, body string) {
		t.Helper()
		if rec.Code != code || !strings.Contains(rec.Body.String(), body) {
			t.Errorf("%s = %d %s, want %d containing %q", name, rec.Code, rec.Body, code, body)
		}
	}
	check("no csrf token", e.postWith(e.url, good, false), http.StatusForbidden, "")
	check("cross-site", e.postWith(e.url, good, true, "Sec-Fetch-Site", "cross-site"), http.StatusForbidden, "")
	check("unknown row", e.post("/preview/999999/auto", good), http.StatusNotFound, "")
	check("word-tier row", e.post("/preview/"+wordID+"/auto", good), http.StatusNotFound, "")
	check("missing mtime", e.post(e.url, url.Values{}), http.StatusBadRequest, `"mtime"`)
	check("stale mtime", e.post(e.url, url.Values{"mtime": {"12345"}}), http.StatusConflict, `"changed"`)
	if code, _ := e.poll(); code != http.StatusNotFound {
		t.Errorf("poll with no run = %d, want 404", code)
	}

	// A .lrc that is a symlink inside the root is refused, as accept refuses it
	// (the row lookup's Lstat already drops it); so is one under no root, which
	// only the shared reader's confinement refuses.
	if os.Symlink(e.lrcP, filepath.Join(e.root, "link.lrc")) == nil {
		linkID := itoa(e.seedTier(t, e.writeFile(t, e.root, "link.flac"), "line"))
		check("symlinked lyrics", e.post("/preview/"+linkID+"/auto", good), http.StatusNotFound, "")
	}
	if err := os.WriteFile(filepath.Join(e.outside, "out.lrc"), []byte(editLRC), 0o600); err != nil {
		t.Fatal(err)
	}
	outID := itoa(e.seedTier(t, e.writeFile(t, e.outside, "out.flac"), "line"))
	check("lyrics under no root", e.post("/preview/"+outID+"/auto", good), http.StatusNotFound, "page not found")
	// An unreadable .lrc is a read failure (500), not a refusal (404).
	if os.Geteuid() > 0 && os.Chmod(e.lrcP, 0) == nil {
		check("lyric read failure", e.start(t), http.StatusInternalServerError, `"read"`)
		_ = os.Chmod(e.lrcP, 0o600)
	}
	e.ui.auto.runs.maxAudio = int64(len(previewBytes)) - 1
	check("audio over the cap", e.start(t), http.StatusRequestEntityTooLarge, `"too_large"`)

	// The row's audio becomes a symlink leaving the library root.
	audio := filepath.Join(e.root, "song.flac")
	_ = os.Remove(audio)
	if os.Symlink(e.writeFile(t, e.outside, "secret.flac"), audio) == nil {
		check("audio outside the root", e.start(t), http.StatusNotFound, "")
	}
	e.put(t, "song.lrc", "[00:01.00]\n[00:03.00]\u3000\n[00:05.00]♪\n")
	check("no line to align", e.start(t), http.StatusUnprocessableEntity, `"no_lines"`)
	if lines, any := autoLines([]lyrics.TimedLine{{Text: " \t\u3000"}, {StartMS: 9, Text: "\u00a0"}}); any || lines[0] != "" || lines[1] != "" {
		t.Errorf("whitespace-only cues = %q any %v, want blanks and no line to align", lines, any)
	}
	if got := fake.aligns.Load(); got != 0 {
		t.Fatalf("sidecar calls after refusals only = %d, want 0", got)
	}
}

// A .lrc that changed after the start: 409, the run is canceled and forgotten.
// A start naming a newer mtime replaces a run going against the old one.
func TestAutoRunChangedFile(t *testing.T) {
	e := newAutoEnv(t, &runFake{hold: make(chan struct{})})
	ended := func(what string, r *autoRun) {
		t.Helper()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("the run was not canceled when %s", what)
		}
	}
	e.start(t)
	first := e.run()
	e.touch(1)
	if code, body := e.poll(); code != http.StatusConflict || body["error"] != "changed" {
		t.Fatalf("poll after the .lrc changed = %d %v, want 409 changed", code, body)
	}
	ended("its .lrc changed", first)
	if code, _ := e.poll(); code != http.StatusNotFound || e.run() != nil {
		t.Fatalf("poll after the 409 = %d (run %v), want 404 and no run", code, e.run())
	}

	e.start(t)
	old := e.run()
	e.touch(2)
	// Busy until the canceled goroutine ends (the cap is 1), then admitted.
	waitFor(t, "the replacing start", func() bool { return e.start(t).Code == http.StatusAccepted })
	ended("a start named a newer .lrc", old)
	if cur := e.run(); cur == nil || cur == old || cur.state != autoRunning {
		t.Fatalf("run after a start with a newer mtime = %+v, want a new running run", cur)
	}
	e.ui.auto.runs.drop(e.rowID, *old)
	if e.run() == nil {
		t.Fatal("dropping the replaced run removed the run that replaced it")
	}
}

// word_sync_generate.concurrency (1 here) bounds runs across rows.
func TestAutoRunGlobalCap(t *testing.T) {
	fake := &runFake{hold: make(chan struct{})}
	e := newAutoEnv(t, fake)
	e.put(t, "two.lrc", editLRC)
	two := "/preview/" + itoa(e.seedTier(t, e.writeFile(t, e.root, "two.flac"), "line")) + "/auto"
	vals := url.Values{"mtime": {itoa(previewSidecarMTime([]string{e.root}, filepath.Join(e.root, "two.lrc")))}}
	e.start(t)
	if rec := e.post(two, vals); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("second row at the cap = %d %s, want 429 busy", rec.Code, rec.Body)
	}
	if rec := e.start(t); rec.Code != http.StatusAccepted {
		t.Fatalf("attach to the running row at the cap = %d %s, want 202", rec.Code, rec.Body)
	}
	close(fake.hold)
	e.waitState(t, autoDone)
	if rec := e.post(two, vals); rec.Code != http.StatusAccepted {
		t.Fatalf("second row once the first finished = %d %s, want 202", rec.Code, rec.Body)
	}
}

// The cap counts live goroutines: a replaced or dropped run that has not
// noticed its cancel still holds its audio and its sidecar call.
func TestAutoRunCapCountsLiveGoroutines(t *testing.T) {
	fake := &runFake{hold: make(chan struct{}), deaf: true}
	e := newAutoEnv(t, fake)
	release := sync.OnceFunc(func() { close(fake.hold) })
	t.Cleanup(release) // before the env's own cleanup, which waits for every run
	busy := func(what string) {
		t.Helper()
		if rec := e.start(t); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"busy"`) {
			t.Fatalf("start %s = %d %s, want 429 busy", what, rec.Code, rec.Body)
		}
	}
	e.start(t)
	first := e.run()
	waitFor(t, "the first sidecar call", func() bool { return fake.aligns.Load() == 1 })
	e.touch(1)
	busy("replacing a run whose goroutine is alive")
	if code, _ := e.poll(); code != http.StatusConflict || e.run() != nil {
		t.Fatalf("poll after the .lrc changed = %d (run %v), want 409 and the run dropped", code, e.run())
	}
	busy("after the run was dropped but before its goroutine ended")
	release()
	<-first.done
	if got := fake.aligns.Load(); got != 1 {
		t.Fatalf("sidecar calls while the first goroutine lived = %d, want 1", got)
	}
	if rec := e.start(t); rec.Code != http.StatusAccepted {
		t.Fatalf("start once the goroutine ended = %d %s, want 202", rec.Code, rec.Body)
	}
	e.waitState(t, autoDone)
}

// The words aligned are the current .lrc's, never a .orig backup's. A panic in
// the aligner call, or audio grown past the cap since its stat, fails the run.
func TestAutoRunCurrentFilePanicGrownAudio(t *testing.T) {
	fake := &runFake{}
	e := newAutoEnv(t, fake)
	e.ui.auto.runs.maxAudio = int64(len(previewBytes)) // exactly at the cap: accepted
	e.put(t, "song.lrc.orig", "[00:01.00]OLD a\n[00:05.00]OLD b\n[00:09.00]OLD c\n")
	e.put(t, "song.lrc", "[00:01.00]new a\n[00:05.00]new b\n[00:09.00]new c\n")
	e.start(t)
	e.waitState(t, autoDone)
	if want := []string{"new a", "new b", "new c"}; !slices.Equal(fake.lines, want) {
		t.Errorf("lines sent = %q, want the current file's %q", fake.lines, want)
	}

	logs := captureLogs(t)
	fake.pre = func() { panic("invented panic naming song.flac") }
	e.start(t)
	if got := e.waitState(t, autoFailed); got["error"] != "aligner" {
		t.Errorf("run whose aligner call panicked = %v, want failed aligner", got)
	}
	if l := logs.String(); !strings.Contains(l, "panicked") || strings.Contains(l, "song") {
		t.Errorf("logs must name the panic and carry none of its text:\n%s", l)
	}

	fake.pre = func() { _ = os.WriteFile(filepath.Join(e.root, "song.flac"), append(previewBytes[:20:20], 'x'), 0o600) }
	e.start(t)
	if got := e.waitState(t, autoFailed); got["error"] != "too_large" {
		t.Errorf("audio that grew past the cap mid-run = %v, want failed too_large", got)
	}
	// A real sidecar answers the oversized upload 413: the byte cap still wins.
	fake.script(fmt.Errorf("%w, status 413", aligner.ErrRejected), nil)
	e.writeFile(t, e.root, "song.flac") // back under the cap; pre grows it again
	if rec := e.start(t); rec.Code != http.StatusAccepted {
		t.Fatalf("start with the audio back under the cap = %d %s, want 202", rec.Code, rec.Body)
	}
	if got := e.waitState(t, autoFailed); got["error"] != "too_large" {
		t.Errorf("grown audio the sidecar rejected = %v, want failed too_large", got)
	}
}

func TestAutoRunFailureCodes(t *testing.T) {
	fake := &runFake{}
	e := newAutoEnv(t, fake)
	for _, tc := range []struct {
		err  error
		want map[string]any
	}{
		{&aligner.BusyError{RetryAfter: 30 * time.Second}, map[string]any{"state": autoFailed, "error": "busy", "retry_after": json.Number("30")}},
		{aligner.ErrBreakerOpen, map[string]any{"state": autoFailed, "error": "unavailable"}},
		{fmt.Errorf("%w, status 422: invented detail", aligner.ErrRejected), map[string]any{"state": autoFailed, "error": "rejected"}},
		{fmt.Errorf("line 2: %w", aligner.ErrInvalidUTF8), map[string]any{"state": autoFailed, "error": "rejected"}},
		{errors.New("dial tcp 192.0.2.7:9000: refused"), map[string]any{"state": autoFailed, "error": "aligner"}},
	} {
		fake.script(tc.err, nil)
		e.start(t)
		if got := e.waitState(t, autoFailed); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("failed run for %q = %v, want %v", tc.err, got, tc.want)
		}
	}
	// A run that outlives its budget is ended by its own context.
	fake.script(nil, make(chan struct{}))
	e.ui.auto.runs.timeout = 20 * time.Millisecond
	e.start(t)
	if got := e.waitState(t, autoFailed); got["error"] != "timeout" {
		t.Errorf("run past its timeout = %v, want error timeout", got)
	}
}
