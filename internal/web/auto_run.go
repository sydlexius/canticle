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
	// autoOrphanAfter: a running run not polled for this long is canceled (the
	// page polls every 2 s, so its viewer has gone).
	autoOrphanAfter = 20 * time.Second
	// autoResultTTL is how long a finished run's result is kept.
	autoResultTTL = 2 * time.Minute
	autoReapEvery = 5 * time.Second
	// autoCloseWait bounds shutdown's wait for run goroutines to end.
	autoCloseWait = 5 * time.Second
	autoRunning   = "running"
	autoDone      = "done"
	autoFailed    = "failed"
	// autoNoSuggestion: the aligner answered, but no word it returned could
	// be used (aligner.Suggest returned false). Not a failure: nothing to offer.
	autoNoSuggestion = "no_suggestion"
)

var (
	// errAutoTooLarge: a file grew past its size cap after its stat.
	errAutoTooLarge = errors.New("auto alignment: file grew past the size cap")
	// errAutoPanic: the aligner call or the suggestion mapping panicked.
	errAutoPanic = errors.New("auto alignment: the run panicked")
)

// autoRun is one alignment of one row. state, code, retryAfter, result and
// suggestion are guarded by autoRuns.mu and written once, by finish.
type autoRun struct {
	state      string
	code       string // machine code of a failed run
	retryAfter int    // seconds the sidecar advertised on "busy"; 0 unknown
	mtime      int64  // the .lrc mtime the run started against (unix ns)
	// seen is the start or last poll of a running run, then its finish time.
	seen   time.Time
	result aligner.Result
	// suggestion is aligner.Suggest's answer for a done run, nil otherwise.
	suggestion *aligner.Suggestion
	cancel     context.CancelFunc
	done       chan struct{} // closed when the run's goroutine has returned
}

// autoRuns is the in-memory registry of runs, keyed by work_queue id: at most
// one entry, so at most one running alignment, per row. Nothing is persisted.
// A run leaves by the cancel route, a changed .lrc, a replacing start, reap
// or close; its goroutine, once canceled, ends on its own and its late result
// is recorded on an entry nothing reads.
type autoRuns struct {
	mu sync.Mutex
	m  map[int64]*autoRun
	// active counts run goroutines not yet finished. A replaced or dropped
	// run leaves m at once but holds its audio and sidecar call until then.
	active int
	// Set at attach, m included; fields so a test can shrink or fail them.
	timeout   time.Duration
	maxAudio  int64
	reapEvery time.Duration
	closeWait time.Duration
	// closed: close ran, no run starts again. wg counts the run goroutines and
	// the reaper, which the first admitted run starts (stop is nil until then).
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
}

// get returns a copy of the row's run and counts as a poll of a running one.
func (s *autoRuns) get(id int64, now time.Time) (autoRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[id]
	if r != nil && r.state == autoRunning {
		r.seen = now // before the reap: a late poll must not reap the run it asks for
	}
	if s.reap(now); s.m[id] == nil {
		return autoRun{}, false
	}
	return *r, true
}

// reap (s.mu held) cancels and forgets every running run not polled for
// autoOrphanAfter, and forgets every result autoResultTTL old, whatever
// became of its row.
func (s *autoRuns) reap(now time.Time) {
	for id, r := range s.m {
		limit := autoResultTTL
		if r.state == autoRunning {
			limit = autoOrphanAfter
		}
		if now.Sub(r.seen) < limit {
			continue
		}
		delete(s.m, id)
		if r.state == autoRunning {
			r.cancel()
			slog.Info("auto alignment: run abandoned; canceled", "id", id)
		}
	}
}

// reapLoop reaps on a ticker until close, so an abandoned run is canceled even
// when no other request arrives.
func (s *autoRuns) reapLoop(now func() time.Time) {
	defer s.wg.Done()
	t := time.NewTicker(s.reapEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.reap(now())
			s.mu.Unlock()
		}
	}
}

// cancel forgets the row's run, running or finished, and cancels it. Its
// goroutine counts under the cap until it has ended.
func (s *autoRuns) cancel(id int64) (found, wasRunning bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[id]
	if r == nil {
		return false, false
	}
	delete(s.m, id)
	r.cancel()
	return true, r.state == autoRunning
}

// close cancels and forgets every run, stops the reaper and waits, at most
// closeWait, for their goroutines to end. Idempotent; begin refuses afterwards.
func (s *autoRuns) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for id, r := range s.m {
		delete(s.m, id)
		r.cancel()
	}
	if s.stop != nil {
		close(s.stop)
	}
	s.mu.Unlock()
	ended := make(chan struct{})
	go func() { s.wg.Wait(); close(ended) }()
	t := time.NewTimer(s.closeWait)
	defer t.Stop()
	select {
	case <-ended:
	case <-t.C:
		s.mu.Lock()
		defer s.mu.Unlock()
		slog.Warn("auto alignment: shutdown stopped waiting for runs that ignored their cancel", "running", s.active)
	}
}

// begin registers r as the row's run (started). A run already going for the
// row against the same .lrc mtime is attached to instead (one run per track);
// one against another mtime aligned a file that has since changed, so it is
// canceled and replaced. It answers "" (started), autoRunning (attached),
// "busy" (limit run goroutines are still live, the canceled one included until
// it ends; the client retries the start) or "unavailable" (closed).
func (s *autoRuns) begin(id int64, r *autoRun, limit int, now func() time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "unavailable"
	}
	cur := s.m[id]
	if cur != nil && cur.state == autoRunning && cur.mtime == r.mtime {
		cur.seen = r.seen
		return autoRunning
	}
	if cur != nil && cur.state == autoRunning {
		cur.cancel()
	}
	if s.active >= limit {
		return "busy"
	}
	s.m[id] = r
	s.active++
	s.wg.Add(1)
	if s.stop == nil {
		s.stop = make(chan struct{})
		s.wg.Add(1)
		go s.reapLoop(now)
	}
	return ""
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

// finish records the run's outcome, frees its place under the cap (its audio
// is closed by then) and returns its log class. A run that did not fail is
// done with sug, or no_suggestion when sug is nil.
func (s *autoRuns) finish(r *autoRun, res aligner.Result, sug *aligner.Suggestion, err error, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	r.state, r.result, r.seen = autoDone, res, now
	switch {
	case err != nil:
		r.state = autoFailed
		r.code, r.retryAfter = autoRunCode(err)
		return r.code
	case sug == nil:
		r.state = autoNoSuggestion
	}
	r.suggestion = sug
	return r.state
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
	case errors.Is(err, context.Canceled):
		return "canceled", 0
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
	var prev lyrics.TimedLine
	for i, l := range cues {
		follower := i > 0 && l.StartMS == prev.StartMS
		// Blank as the client filters it: whitespace alone is no line.
		if prev = l; !l.Decorative && !follower && !aligner.IsBlank(l.Text) {
			lines[i], any = l.Text, true
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
	if cur, ok := runs.get(id, u.auto.now()); ok && cur.state == autoRunning && cur.mtime == mtime {
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
	starts := make([]int, len(cues))
	for i, c := range cues {
		starts[i] = c.StartMS
	}
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
	started := u.auto.now()
	run := &autoRun{state: autoRunning, mtime: mtime, seen: started, cancel: cancel, done: make(chan struct{})}
	if refused := runs.begin(id, run, u.auto.maxConcurrent, u.auto.now); refused != "" {
		cancel()
		_ = audio.Close()
		switch refused {
		case autoRunning: // attached to the run already going; nothing started
			running()
		case "busy":
			writeEditJSON(w, http.StatusTooManyRequests, map[string]string{"error": refused})
		default: // CloseAuto ran: the server is shutting down
			writeEditJSON(w, http.StatusServiceUnavailable, map[string]string{"error": refused})
		}
		return
	}
	// Before the answer: only this goroutine frees the slot begin just counted.
	go func() {
		defer runs.wg.Done() // after done: close waits for the whole goroutine
		// done closes last: by then the run holds nothing and has logged.
		defer close(run.done)
		// A panic must still finish the run; its value (a path?) is not logged.
		res, err := aligner.Result{}, errAutoPanic
		var sug *aligner.Suggestion
		defer func() {
			if recover() != nil {
				slog.Error("auto alignment: the run panicked", "id", id)
				res, sug, err = aligner.Result{}, nil, errAutoPanic
			}
			_ = audio.Close()
			cancel()
			slog.Info("auto alignment: run finished", "id", id, "result", runs.finish(run, res, sug, err, u.auto.now()),
				"duration_ms", u.auto.now().Sub(started).Milliseconds())
		}()
		// One byte past the cap is readable: a file grown since the stat fails,
		// whatever the aligner answered to the oversized upload (a 413).
		bounded := &io.LimitedReader{R: autoAudio{audio}, N: limit + 1}
		res, err = u.auto.aligner.AlignFile(ctx, bounded, lines)
		if bounded.N <= 0 {
			res, err = aligner.Result{}, errAutoTooLarge
		}
		if err == nil {
			// Mapped against the cues the run started on: the poll drops the
			// run once the .lrc mtime moves, so they are the current ones.
			if s, ok := aligner.Suggest(lines, starts, res); ok {
				sug = &s
			}
		}
	}()
	slog.Info("auto alignment: run started", "id", id)
	running()
}

// autoAudio closes the audio file at its first read error, EOF included: the
// aligner client buffers the whole upload before it sends, so the handle is
// not held while the sidecar works.
type autoAudio struct{ f io.ReadCloser }

func (a autoAudio) Read(p []byte) (int, error) {
	n, err := a.f.Read(p)
	if err != nil {
		_ = a.f.Close()
	}
	return n, err
}

// handleAutoCancel cancels the row's run and forgets it, result included, so a
// later poll answers 404. Idempotent: with no run it answers the same 200
// {"state":"canceled"}. The cancel is canticle-side only: it ends this
// process's request; the sidecar may still finish the alignment it was sent.
// A registered run is canceled before any row lookup, so one whose row has
// since left the eligible set (or vanished) can still be stopped.
func (u *UI) handleAutoCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, editMaxBody)
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	found := false
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil && u.auto != nil {
		var running bool
		if found, running = u.auto.runs.cancel(id); running {
			slog.Info("auto alignment: run canceled", "id", id)
		}
	}
	if !found {
		if _, _, _, ok := u.autoTarget(w, r); !ok {
			return
		}
	}
	writeEditJSON(w, http.StatusOK, map[string]string{"state": "canceled"})
}

// handleAutoPoll reports the row's run: running, failed with a machine code,
// no_suggestion (the aligner answered, but no word was usable), or done with
// the suggestion (autoSuggestionJSON) and the .lrc mtime it was started
// against, which the accept route takes as its ExpectMTime. A .lrc whose mtime moved since the start makes the
// run worthless: it is canceled and dropped, and the poll answers 409.
func (u *UI) handleAutoPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, t, roots, ok := u.autoTarget(w, r)
	if !ok {
		return
	}
	run, ok := u.auto.runs.get(id, u.auto.now())
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
		autoSuggestionJSON(body, run.suggestion)
	case autoNoSuggestion:
		body["aligned_words"] = len(run.result.Words)
	case autoFailed:
		body["error"] = run.code
		if run.retryAfter > 0 {
			body["retry_after"] = run.retryAfter
		}
	}
	writeEditJSON(w, http.StatusOK, body)
}

// autoWord is one word unit of a suggested line: the strings.Fields token
// index it starts at, and its start in ms from the start of the audio.
type autoWord struct {
	Token   int `json:"token"`
	StartMS int `json:"start_ms"`
}

// autoQuality is aligner.Quality as the poll answers it; similarity is
// undefined (0) when has_transcript is false.
type autoQuality struct {
	Similarity     float64 `json:"similarity"`
	HasTranscript  bool    `json:"has_transcript"`
	MeanConfidence float64 `json:"mean_confidence"`
	Coverage       float64 `json:"coverage"`
	Tokens         int     `json:"tokens"`
	AlignedTokens  int     `json:"aligned_tokens"`
	Merged         int     `json:"merged"`
}

// autoSuggestionJSON adds a done run's suggestion to the poll body: lines, one
// start (ms) per cue of the .lrc in file order; words, per cue its word units
// or null; quality; and warnings, the codes below threshold in the fixed order
// similarity, confidence, coverage, merged (an array, never null).
func autoSuggestionJSON(body map[string]any, s *aligner.Suggestion) {
	words := make([][]autoWord, len(s.Words))
	for i, ws := range s.Words {
		for _, w := range ws {
			words[i] = append(words[i], autoWord{Token: w.Token, StartMS: w.StartMS})
		}
	}
	q := s.Quality
	body["lines"] = s.LineMS
	body["words"] = words
	body["quality"] = autoQuality{Similarity: q.Similarity, HasTranscript: q.HasTranscript,
		MeanConfidence: q.MeanConfidence, Coverage: q.Coverage, Tokens: q.Tokens,
		AlignedTokens: q.AlignedTokens, Merged: q.Merged}
	body["warnings"] = q.Warnings()
}
