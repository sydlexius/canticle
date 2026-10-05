package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

// LyricEditor is the seam the edit routes record a hand edit through (#481
// Stage 2); nil means the editor is not offered and every edit route 404s.
type LyricEditor interface {
	SetLyricEdit(ctx context.Context, id int64, offsetMS int) error
	ClearLyricEdit(ctx context.Context, id int64) error
	LyricEdit(ctx context.Context, id int64) (offsetMS int, edited bool, err error)
	// SetLyricRetime marks an accepted generated retiming (#1008): edited, no
	// offset. LyricRetimed reports that shape so a failed save can restore it.
	SetLyricRetime(ctx context.Context, id int64) error
	LyricRetimed(ctx context.Context, id int64) (bool, error)
}

// EditDeps wires the lyric offset editor.
type EditDeps struct {
	Queue      LyricEditor
	Durations  *audiodur.Store     // nil: duration unknown, the timing guard fails open
	SelfWrites *selfwrite.Registry // nil-safe
}

// AttachLyricEditor enables POST /preview/{id}/offset, /revert and /auto/accept.
func (u *UI) AttachLyricEditor(d EditDeps) { u.editor = &d }

// rowLocks serializes edits per work_queue row: lyrics.ApplyEdit's mtime check
// and .orig publish assume one writer per file, so two concurrent saves of one
// row must not both pass the check. Entries are refcounted and dropped on the
// last release, so the map never grows with the number of rows ever edited.
type rowLocks struct {
	mu sync.Mutex
	m  map[int64]*rowLock
}

type rowLock struct {
	sync.Mutex
	refs int
}

func (l *rowLocks) lock(id int64) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[int64]*rowLock{}
	}
	e := l.m[id]
	if e == nil {
		e = &rowLock{}
		l.m[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.Lock()
	return func() {
		e.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}

// editMaxBody bounds the offset and revert forms (three short fields);
// acceptMaxBody bounds an accept, which carries one number per line and word.
const (
	editMaxBody   = 4 << 10
	acceptMaxBody = 1 << 20
)

func writeEditJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// editErrAttr describes err without any path it may carry (a library path
// names the artist and title); logs hold the row id only. The wrapping layers
// of a chain are where callers put paths ("audiodur: lookup %q: %w",
// *fs.PathError, *os.LinkError), so only the innermost error's text is
// emitted, plus the operation of a filesystem error. Never err.Error().
func editErrAttr(err error) slog.Attr {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return slog.String("error", pe.Op+": "+innermostErr(pe.Err).Error())
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return slog.String("error", le.Op+": "+innermostErr(le.Err).Error())
	}
	return slog.String("error", innermostErr(err).Error())
}

// innermostErr follows the Unwrap chain (the first branch of a joined error)
// to its leaf.
func innermostErr(err error) error {
	for {
		var next error
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			next = x.Unwrap()
		case interface{ Unwrap() []error }:
			if errs := x.Unwrap(); len(errs) > 0 {
				next = errs[0]
			}
		}
		if next == nil {
			return err
		}
		err = next
	}
}

func (u *UI) handlePreviewOffset(w http.ResponseWriter, r *http.Request) {
	u.handlePreviewEdit(w, r, false, false)
}
func (u *UI) handlePreviewRevert(w http.ResponseWriter, r *http.Request) {
	u.handlePreviewEdit(w, r, true, false)
}

// handlePreviewAutoAccept accepts a generated (aligner-suggested) retiming
// (#1008). Its caller is the Auto accept UI of the next #1008 slice; until
// then nothing in the page posts here. It is gated by the session, CSRF and
// the word-timing predicate only, deliberately NOT by aligner availability:
// it writes the timings it is handed and never contacts the sidecar.
func (u *UI) handlePreviewAutoAccept(w http.ResponseWriter, r *http.Request) {
	u.handlePreviewEdit(w, r, false, true)
}

// parseAccept reads an accept's timings: "lines" is a JSON array with one
// start (ms) per line, "words" an optional JSON array holding, per line, its
// [tokenIndex, ms] pairs. It returns the form field that is malformed, or "".
// No lyric text is read from the request; text only ever comes from the file.
// A JSON null anywhere is malformed: it would decode to 0 and be written (a
// client serializes NaN as null), and no valid value contains the word.
func parseAccept(r *http.Request) (starts []int, words [][][]int, bad string) {
	ls := r.PostFormValue("lines")
	if err := json.Unmarshal([]byte(ls), &starts); err != nil || len(starts) == 0 || strings.Contains(ls, "null") {
		return nil, nil, "lines"
	}
	if ws := strings.TrimSpace(r.PostFormValue("words")); ws != "" {
		if err := json.Unmarshal([]byte(ws), &words); err != nil || strings.Contains(ws, "null") {
			return nil, nil, "words"
		}
	}
	return starts, words, ""
}

// acceptWords resolves posted [tokenIndex, ms] pairs against the current
// file's line texts (whitespace-separated tokens), refusing a wrong line count,
// a malformed pair, and an index that repeats, goes backwards or names no token.
func acceptWords(cur []lyrics.TimedLine, words [][][]int) ([]models.WordTiming, bool) {
	if len(words) != 0 && len(words) != len(cur) {
		return nil, false
	}
	var out []models.WordTiming
	for i, pairs := range words {
		toks, prev := strings.Fields(cur[i].Text), -1
		for _, p := range pairs {
			if len(p) != 2 || p[0] <= prev || p[0] >= len(toks) {
				return nil, false
			}
			prev = p[0]
			out = append(out, models.WordTiming{Line: i, Text: toks[p[0]], StartMS: p[1]})
		}
	}
	return out, true
}

// handlePreviewEdit saves (offset_ms applied to the original) or reverts (the
// original, offset 0) the .lrc of one line-editable row. The path is never
// taken from the request: it is the row's own sidecar, from PreviewSource.
// A row that is unknown or not line-editable is the same bare 404 the player
// gives, so the route reveals nothing the player does not.
// An accept (#1008) posts one start per original line instead of an offset.
func (u *UI) handlePreviewEdit(w http.ResponseWriter, r *http.Request, revert, accept bool) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, map[bool]int64{false: editMaxBody, true: acceptMaxBody}[accept])
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || u.editor == nil || u.reports == nil {
		http.NotFound(w, r)
		return
	}
	offset := 0
	var starts []int
	var words [][][]int
	if accept {
		var bad string
		if starts, words, bad = parseAccept(r); bad != "" {
			writeEditJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
			return
		}
	} else if !revert {
		offset, err = strconv.Atoi(strings.TrimSpace(r.PostFormValue("offset_ms")))
		if err != nil || offset > lyrics.MaxEditOffsetMS || offset < -lyrics.MaxEditOffsetMS {
			writeEditJSON(w, http.StatusBadRequest, map[string]string{"error": "offset"})
			return
		}
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("mtime")), 10, 64)
	if err != nil || mtime <= 0 {
		writeEditJSON(w, http.StatusBadRequest, map[string]string{"error": "mtime"})
		return
	}

	unlock := u.editLocks.lock(id)
	defer unlock() // released after the path lock below (defers run LIFO)
	t, err := u.reports.PreviewSource(r.Context(), id)
	if errors.Is(err, reports.ErrPreviewNotFound) || (err == nil && (!t.LineEditable || t.LRCPath == "")) {
		http.NotFound(w, r)
		return
	}
	roots, rerr := u.reports.LibraryRoots(r.Context())
	if err == nil {
		err = rerr
	}
	var priorOff int
	var priorEdited, priorRetimed bool
	if err == nil {
		priorOff, priorEdited, err = u.editor.Queue.LyricEdit(r.Context(), id)
	}
	if err == nil {
		priorRetimed, err = u.editor.Queue.LyricRetimed(r.Context(), id)
	}
	if err != nil {
		slog.Error("lyric edit: lookup failed", "id", id, editErrAttr(err))
		writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup"})
		return
	}
	// Revert undoes a recorded edit. On an unedited row it would only rewrite
	// the file (and create the .orig no save asked for).
	if revert && !priorEdited {
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "not_edited"})
		return
	}

	// Ordering. The sweeps that replace a line-tier .lrc (upgrade, word
	// recheck) skip a row with lyric_edited_at set, but only when they ADMIT
	// it. So a save MARKS the row before writing, then re-checks that it is
	// still a settled line-editable row: once marked no sweep can admit it,
	// and a row a sweep admitted before the mark is no longer 'done' (or is
	// word-recheck 'queued'), so the re-check refuses it and the worker's
	// later write cannot overwrite the hand edit. A failure after the mark
	// restores the prior mark. A revert keeps write-then-clear: the row is
	// marked throughout, and a failed clear leaves it protected (the safe
	// direction).
	//
	// The per-sidecar edit lock (lyrics.LockEditPath, #1226) is held from
	// before the mark changes until the file write (and a revert's clear) is
	// done, so the serve-mode timing sweep, which re-reads the mark under the
	// same lock before each remediation move, never moves a file mid-edit.
	unlockPath := lyrics.LockEditPath(t.LRCPath)
	defer unlockPath()
	restore := func() {}
	if !revert {
		mark := u.editor.Queue.SetLyricRetime
		if !accept {
			mark = func(ctx context.Context, id int64) error { return u.editor.Queue.SetLyricEdit(ctx, id, offset) }
		}
		if err := mark(r.Context(), id); err != nil {
			slog.Error("lyric edit: recording the edit failed", "id", id, editErrAttr(err))
			writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "record"})
			return
		}
		restore = func() { u.restoreLyricEdit(r.Context(), id, priorOff, priorEdited, priorRetimed) }
	}
	t2, err := u.reports.PreviewSource(r.Context(), id)
	if err == nil && (!t2.LineEditable || t2.LRCPath != t.LRCPath || t2.AudioPath != t.AudioPath) {
		restore()
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "busy"})
		return
	}
	if err != nil {
		restore()
		if errors.Is(err, reports.ErrPreviewNotFound) {
			writeEditJSON(w, http.StatusConflict, map[string]string{"error": "busy"})
			return
		}
		slog.Error("lyric edit: lookup failed", "id", id, editErrAttr(err))
		writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup"})
		return
	}

	var res lyrics.EditResult
	var gen *lyrics.GeneratedEdit
	badField := "words" // which accept field an ErrEditInvalid is about
	orig, tags, err := lyrics.OriginalLines(t.LRCPath, roots)
	lines := orig
	switch {
	case err != nil:
		// Nothing to build; the error is mapped below.
	case !accept:
		lines = lyrics.ShiftLines(orig, offset)
	default:
		// orig supplies the same-stamp group rule only; text, tags and word
		// tokens are the current file's, read by ApplyEdit.
		if lines, err = lyrics.RetimeLines(orig, starts); err != nil {
			badField = "lines"
		}
		gen = &lyrics.GeneratedEdit{WordsFor: func(cur []lyrics.TimedLine) ([]models.WordTiming, bool) {
			return acceptWords(cur, words)
		}}
	}
	if err == nil {
		res, err = lyrics.ApplyEdit(t.LRCPath, lines, tags, lyrics.EditOptions{
			Roots:           roots,
			ExpectMTime:     time.Unix(0, mtime),
			DurationSeconds: u.editDuration(r, id, roots, t.AudioPath),
			SelfWrites:      u.editor.SelfWrites,
			Generated:       gen,
		})
	}
	if err != nil {
		restore()
	}
	switch {
	case errors.Is(err, lyrics.ErrEditRefused):
		slog.Warn("lyric edit refused: not a regular .lrc under a library root", "id", id)
		http.NotFound(w, r)
		return
	case errors.Is(err, lyrics.ErrEditChanged):
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "changed"})
		return
	case errors.Is(err, lyrics.ErrEditHasWords):
		writeEditJSON(w, http.StatusConflict, map[string]string{"error": "has_words"})
		return
	case errors.Is(err, lyrics.ErrEditInvalid):
		writeEditJSON(w, http.StatusBadRequest, map[string]string{"error": badField})
		return
	case errors.Is(err, lyrics.ErrEditTiming):
		detail := strings.TrimPrefix(err.Error(), lyrics.ErrEditTiming.Error()+": ")
		writeEditJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "timing", "detail": detail})
		return
	case err != nil:
		slog.Error("lyric edit: write failed", "id", id, editErrAttr(err))
		writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "write"})
		return
	}
	if revert {
		if err := u.editor.Queue.ClearLyricEdit(r.Context(), id); err != nil {
			// The original is back on disk but the row stays marked, so it
			// is only over-protected; the next revert clears it.
			slog.Error("lyric edit: clearing the edit failed", "id", id, editErrAttr(err))
			writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "record"})
			return
		}
	}
	slog.Info("lyric edit saved", "id", id, "offset_ms", offset, "revert", revert, "generated", accept, "created_orig", res.CreatedOrig)
	writeEditJSON(w, http.StatusOK, map[string]any{
		"offset_ms": offset, "mtime": res.NewMTime.UnixNano(), "created_orig": res.CreatedOrig,
	})
}

// restoreLyricEdit puts back the SHAPE of the mark a refused or failed save
// replaced: the prior offset, the prior retime mark, or no mark at all. The
// mark's timestamp is rewritten, not restored. A failed restore leaves the row
// marked, which only stops automatic upgrades of it (the safe direction), so
// it is logged and not surfaced.
func (u *UI) restoreLyricEdit(ctx context.Context, id int64, priorOff int, priorEdited, priorRetimed bool) {
	var err error
	switch {
	case priorRetimed:
		err = u.editor.Queue.SetLyricRetime(ctx, id)
	case priorEdited:
		err = u.editor.Queue.SetLyricEdit(ctx, id, priorOff)
	default:
		err = u.editor.Queue.ClearLyricEdit(ctx, id)
	}
	if err != nil {
		slog.Error("lyric edit: restoring the prior edit mark failed; row stays marked", "id", id, editErrAttr(err))
	}
}

// editDuration is the exact audio duration the timing guard judges against,
// from the duration store keyed on the audio file as opened through the same
// root confinement as the player. Any miss is 0 (unknown), which fails open.
func (u *UI) editDuration(r *http.Request, id int64, roots []string, audioPath string) int {
	if u.editor.Durations == nil {
		return 0
	}
	f, fi, ok := openPreviewAudio(roots, audioPath)
	if !ok {
		return 0
	}
	_ = f.Close()
	secs, found, err := u.editor.Durations.Lookup(r.Context(), audioPath, fi.ModTime().UnixNano(), fi.Size())
	if err != nil {
		slog.Warn("lyric edit: duration lookup failed; timing guard fails open", "id", id, editErrAttr(err))
	}
	if err != nil || !found {
		return 0
	}
	return secs
}
