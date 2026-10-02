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
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

// LyricEditor is the seam the edit routes record a hand edit through (#481
// Stage 2); nil means the editor is not offered and both routes 404.
type LyricEditor interface {
	SetLyricEdit(ctx context.Context, id int64, offsetMS int) error
	ClearLyricEdit(ctx context.Context, id int64) error
	LyricEdit(ctx context.Context, id int64) (offsetMS int, edited bool, err error)
}

// EditDeps wires the lyric offset editor.
type EditDeps struct {
	Queue      LyricEditor
	Durations  *audiodur.Store     // nil: duration unknown, the timing guard fails open
	SelfWrites *selfwrite.Registry // nil-safe
}

// AttachLyricEditor enables POST /preview/{id}/offset and /revert.
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

// editMaxBody bounds the form; it carries three short fields.
const editMaxBody = 4 << 10

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
	u.handlePreviewEdit(w, r, false)
}
func (u *UI) handlePreviewRevert(w http.ResponseWriter, r *http.Request) {
	u.handlePreviewEdit(w, r, true)
}

// handlePreviewEdit saves (offset_ms applied to the original) or reverts (the
// original, offset 0) the .lrc of one line-editable row. The path is never
// taken from the request: it is the row's own sidecar, from PreviewSource.
// A row that is unknown or not line-editable is the same bare 404 the player
// gives, so the route reveals nothing the player does not.
func (u *UI) handlePreviewEdit(w http.ResponseWriter, r *http.Request, revert bool) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, editMaxBody)
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || u.editor == nil || u.reports == nil {
		http.NotFound(w, r)
		return
	}
	offset := 0
	if !revert {
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
	var priorEdited bool
	if err == nil {
		priorOff, priorEdited, err = u.editor.Queue.LyricEdit(r.Context(), id)
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
		if err := u.editor.Queue.SetLyricEdit(r.Context(), id, offset); err != nil {
			slog.Error("lyric edit: recording the edit failed", "id", id, editErrAttr(err))
			writeEditJSON(w, http.StatusInternalServerError, map[string]string{"error": "record"})
			return
		}
		restore = func() { u.restoreLyricEdit(r.Context(), id, priorOff, priorEdited) }
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
	orig, tags, err := lyrics.OriginalLines(t.LRCPath, roots)
	if err == nil {
		res, err = lyrics.ApplyEdit(t.LRCPath, lyrics.ShiftLines(orig, offset), tags, lyrics.EditOptions{
			Roots:           roots,
			ExpectMTime:     time.Unix(0, mtime),
			DurationSeconds: u.editDuration(r, id, roots, t.AudioPath),
			SelfWrites:      u.editor.SelfWrites,
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
	slog.Info("lyric edit saved", "id", id, "offset_ms", offset, "revert", revert, "created_orig", res.CreatedOrig)
	writeEditJSON(w, http.StatusOK, map[string]any{
		"offset_ms": offset, "mtime": res.NewMTime.UnixNano(), "created_orig": res.CreatedOrig,
	})
}

// restoreLyricEdit puts back the mark a refused or failed save replaced: the
// prior offset, or no mark at all. A failed restore leaves the row marked,
// which only stops automatic upgrades of it (the safe direction), so it is
// logged and not surfaced.
func (u *UI) restoreLyricEdit(ctx context.Context, id int64, priorOff int, priorEdited bool) {
	var err error
	if priorEdited {
		err = u.editor.Queue.SetLyricEdit(ctx, id, priorOff)
	} else {
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
