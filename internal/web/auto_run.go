package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/aligner"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/reports"
)

const (
	// autoMaxAudioBytes mirrors the sidecar's ALIGNER_MAX_AUDIO_BYTES default.
	// The client buffers the whole file: this bounds ONE run's memory, and
	// begin bounds the runs going at once (autoState.maxConcurrent).
	autoMaxAudioBytes = 100 << 20
	// autoRunTimeout bounds one run (the sidecar's decode cap alone is 300 s).
	autoRunTimeout = 10 * time.Minute
	autoRunning    = "running"
	autoDone       = "done"
	autoFailed     = "failed"
)

// errAutoTooLarge: a file grew past its size cap after its stat.
var errAutoTooLarge = errors.New("auto alignment: file grew past the size cap")

// autoRun is one alignment of one row. state, code, retryAfter and result are
// guarded by autoRuns.mu and written once, by finish.
type autoRun struct {
	state      string
	code       string // machine code of a failed run
	retryAfter int    // seconds the sidecar advertised on "busy"; 0 unknown
	mtime      int64  // the .lrc mtime the run started against (unix ns)
	result     aligner.Result
	cancel     context.CancelFunc
	done       chan struct{} // closed when the run's goroutine has returned
}

// autoRuns is the in-memory registry of runs, keyed by work_queue id: at most
// one entry, so at most one running alignment, per row. Nothing is persisted.
// A finished run stays until the row's next start; expiry lands with S5.
type autoRuns struct {
	mu sync.Mutex
	m  map[int64]*autoRun
	// Set at attach, m included; fields so a test can shrink or fail them.
	timeout  time.Duration
	maxAudio int64
}

// get returns a copy of the row's run.
func (s *autoRuns) get(id int64) (autoRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.m[id]; r != nil {
		return *r, true
	}
	return autoRun{}, false
}

// begin registers r as the row's run (started). A run already going for the
// row against the same .lrc mtime is attached to instead (one run per track);
// one against another mtime aligned a file that has since changed, so it is
// canceled and replaced. busy: limit runs are going for other rows already.
func (s *autoRuns) begin(id int64, r *autoRun, limit int) (started, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.m[id]
	if cur != nil && cur.state == autoRunning && cur.mtime == r.mtime {
		return false, false
	}
	others := 0
	for other, o := range s.m {
		if other != id && o.state == autoRunning {
			others++
		}
	}
	if others >= limit {
		return false, true
	}
	if cur != nil && cur.state == autoRunning {
		cur.cancel()
	}
	s.m[id] = r
	return true, false
}

// drop cancels the run get returned and forgets it, unless a newer run (told
// apart by its done channel) has taken the row since.
func (s *autoRuns) drop(id int64, r autoRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.m[id]; cur != nil && cur.done == r.done {
		delete(s.m, id)
	}
	r.cancel()
}

// finish records the run's outcome and returns its log class.
func (s *autoRuns) finish(r *autoRun, res aligner.Result, err error) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.state, r.result = autoDone, res
	if err != nil {
		r.state = autoFailed
		r.code, r.retryAfter = autoRunCode(err)
		return r.code
	}
	return autoDone
}

// autoRunCode is a failed run's machine code. The error's text is never used
// or logged: it can quote the sidecar's answer.
func autoRunCode(err error) (code string, retryAfter int) {
	var busy *aligner.BusyError
	switch {
	case errors.As(err, &busy):
		return "busy", int(busy.RetryAfter / time.Second)
	case errors.Is(err, aligner.ErrBreakerOpen):
		return "unavailable", 0
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", 0
	case errors.Is(err, aligner.ErrRejected), errors.Is(err, aligner.ErrInvalidUTF8):
		return "rejected", 0
	case errors.Is(err, errAutoTooLarge):
		return "too_large", 0
	}
	return "aligner", 0
}

// autoLines is the text sent per cue of the CURRENT .lrc, index for index, as
// lyrics.CurrentLines returns them. A decorative line and the
// later lines of a same-stamp group (a bilingual pair) go blank; any reports
// a line left with text to align.
func autoLines(cues []lyrics.TimedLine) (lines []string, any bool) {
	lines = make([]string, len(cues))
	for i, l := range cues {
		if !l.Decorative && (i == 0 || l.StartMS != cues[i-1].StartMS) {
			lines[i] = l.Text
			any = any || l.Text != ""
		}
	}
	return lines, any
}

// autoTarget resolves the row both routes act on. Auto not attached, an
// unknown id and an ineligible row are the same bare 404 the player gives.
func (u *UI) autoTarget(w http.ResponseWriter, r *http.Request) (id int64, t reports.PreviewTarget, roots []string, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || u.auto == nil || u.reports == nil {
		http.NotFound(w, r)
		return 0, t, nil, false
	}
	t, err = u.reports.PreviewSource(r.Context(), id)
	if errors.Is(err, reports.ErrPreviewNotFound) || (err == nil && (!autoEligible(t) || t.LRCPath == "")) {
		http.NotFound(w, r)
		return 0, t, nil, false
	}
	if err == nil {
		roots, err = u.reports.LibraryRoots(r.Context())
	}
	if err != nil {
		slog.Error("auto alignment: lookup failed", "id", id, editErrAttr(err))
		writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup"})
		return 0, t, nil, false
	}
	return id, t, roots, true
}

// handleAutoStart starts an alignment of one row, or attaches to the one
// already running for it, and answers at once: 202 {"state":"running"}. It
// writes nothing to disk or the database; the result is held in memory.
func (u *UI) handleAutoStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, editMaxBody)
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	id, t, roots, ok := u.autoTarget(w, r)
	if !ok {
		return
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("mtime")), 10, 64)
	if err != nil || mtime <= 0 {
		writeEditJSON(w, http.StatusBadRequest, map[string]string{"error": "mtime"})
		return
	}
	runs := &u.auto.runs
	running := func() { writeEditJSON(w, http.StatusAccepted, map[string]string{"state": autoRunning}) }
	// A repeat start of a run already going opens no file.
	if cur, ok := runs.get(id); ok && cur.state == autoRunning && cur.mtime == mtime {
		running()
		return
	}
	// The accept route's own reader: the current .lrc, never the .orig, no
	// symlink followed, and only while its mtime is the request's. A file this
	// refuses could not be accepted either.
	cues, err := lyrics.CurrentLines(t.LRCPath, roots, time.Unix(0, mtime))
	switch {
	case errors.Is(err, lyrics.ErrEditChanged):
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "changed"})
		return
	case errors.Is(err, lyrics.ErrEditRefused):
		slog.Warn("auto alignment refused: lyrics are not a regular file under a library root", "id", id)
		http.NotFound(w, r)
		return
	case err != nil:
		slog.Error("auto alignment: lyric read failed", "id", id, editErrAttr(err))
		writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "read"})
		return
	}
	lines, any := autoLines(cues)
	if !any {
		writeEditJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no_lines"})
		return
	}
	audio, fi, ok := openPreviewAudio(roots, t.AudioPath)
	if !ok {
		slog.Warn("auto alignment refused: audio is not a regular file under a library root", "id", id)
		http.NotFound(w, r)
		return
	}
	limit := runs.maxAudio
	if fi.Size() > limit {
		_ = audio.Close()
		writeEditJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return
	}
	// The run outlives this request, so its context is its own.
	ctx, cancel := context.WithTimeout(context.Background(), runs.timeout)
	run, started := &autoRun{state: autoRunning, mtime: mtime, cancel: cancel, done: make(chan struct{})}, u.auto.now()
	if ok, busy := runs.begin(id, run, u.auto.maxConcurrent); !ok {
		cancel()
		_ = audio.Close()
		if busy {
			writeEditJSON(w, http.StatusTooManyRequests, map[string]string{"error": "busy"})
			return
		}
		running() // attached to the run already going; nothing started
		return
	}
	running()
	slog.Info("auto alignment: run started", "id", id)
	go func() {
		defer close(run.done)
		defer cancel()
		defer func() { _ = audio.Close() }()
		// A panic must still finish the run; its value (a path?) is not logged.
		res, err := aligner.Result{}, errors.New("auto alignment: the aligner call panicked")
		defer func() {
			if recover() != nil {
				slog.Error("auto alignment: the aligner call panicked", "id", id)
			}
			slog.Info("auto alignment: run finished", "id", id, "result", runs.finish(run, res, err),
				"duration_ms", u.auto.now().Sub(started).Milliseconds())
		}()
		// One byte past the cap is readable: a file grown since the stat fails.
		bounded := &io.LimitedReader{R: audio, N: limit + 1}
		res, err = u.auto.aligner.AlignFile(ctx, bounded, lines)
		if err == nil && bounded.N <= 0 {
			res, err = aligner.Result{}, errAutoTooLarge
		}
	}()
}

// handleAutoPoll reports the row's run: running, failed with a machine code,
// or done with the .lrc mtime it was started against, which the accept route
// takes as its ExpectMTime. A .lrc whose mtime moved since the start makes the
// run worthless: it is canceled and dropped, and the poll answers 409.
func (u *UI) handleAutoPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, t, roots, ok := u.autoTarget(w, r)
	if !ok {
		return
	}
	run, ok := u.auto.runs.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if previewSidecarMTime(roots, t.LRCPath) != run.mtime {
		u.auto.runs.drop(id, run)
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "changed"})
		return
	}
	body := map[string]any{"state": run.state}
	switch run.state {
	case autoDone:
		// Unix nanoseconds, a JSON number as the save route answers it.
		body["mtime"] = run.mtime
		body["aligned_words"] = len(run.result.Words)
	case autoFailed:
		body["error"] = run.code
		if run.retryAfter > 0 {
			body["retry_after"] = run.retryAfter
		}
	}
	writeEditJSON(w, http.StatusOK, body)
}
