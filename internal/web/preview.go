package web

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/timing"
	"github.com/sydlexius/canticle/web/templates"
)

// registerPreviewRoutes registers the read-only preview player routes (#481)
// through reg, so they are guarded exactly like every other page route.
func (u *UI) registerPreviewRoutes(reg routeReg) {
	reg("GET /preview/{id}", u.handlePreviewPage)
	reg("GET /preview/{id}/audio", u.handlePreviewAudio)
}

// previewSidecarMax bounds how much of a lyric sidecar the page reads; a real
// .lrc or .elrc is a few KiB, so anything larger is cut, not buffered.
const previewSidecarMax = 2 << 20

// readPreviewSidecar reads a sidecar through the same os.Root confinement as
// the audio route (the path is DB-derived), bounded to previewSidecarMax. It
// reads one byte past the bound so an oversized file is told apart from one
// that exactly fits: on overflow the body is cut back to its last complete
// line (never mid-cue) and truncated is true.
func readPreviewSidecar(roots []string, p string) (body string, truncated, ok bool) {
	if p == "" {
		return "", false, false
	}
	f, _, ok := openPreviewAudio(roots, p)
	if !ok {
		return "", false, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, previewSidecarMax+1))
	if err != nil {
		return "", false, false
	}
	if len(b) > previewSidecarMax {
		b = b[:previewSidecarMax]
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			b = b[:i+1]
		} else {
			b = nil
		}
		return string(b), true, true
	}
	return string(b), false, true
}

// previewWords splits a line's words out of its own text, recording the exact
// text before each word so the page renders the line as
// written. ParseTimedLRC trims every word, so the separator is not in the word:
// a space for Latin text, nothing for CJK, any leading unmarked text before
// the first marker. Words are found in source order; when one cannot be found
// the line cannot be reconstructed losslessly and ok is false, so the caller
// renders the line's full text without spans.
func previewWords(text string, ws []lyrics.TimedWord) (words []templates.PreviewWord, ok bool) {
	pos := 0
	for _, w := range ws {
		i := strings.Index(text[pos:], w.Text)
		if i < 0 {
			return nil, false
		}
		words = append(words, templates.PreviewWord{
			StartMS: strconv.Itoa(w.StartMS),
			Before:  text[pos : pos+i],
			Text:    w.Text,
		})
		pos += i + len(w.Text)
	}
	return words, true
}

// previewLines parses the line-synced body and, when an owned word-synced
// companion body is given, attaches its A2 words to the lines by start time
// and, among lines sharing a start, by occurrence order. Inline words in the
// .lrc itself are kept. A companion that is not canticle's own is ignored, by
// the lyrics package's own header-only [by:canticle] rule.
func previewLines(lrc, elrc string) ([]templates.PreviewLine, bool) {
	parsed := lyrics.ParseTimedLRC(lrc)
	words := map[int][][]lyrics.TimedWord{}
	if elrc != "" && lyrics.IsOwnedCompanionBody(elrc) {
		for _, l := range lyrics.ParseTimedLRC(elrc).Lines {
			words[l.StartMS] = append(words[l.StartMS], l.Words)
		}
	}
	seen := map[int]int{}
	out := make([]templates.PreviewLine, 0, len(parsed.Lines))
	hasWords := false
	for _, l := range parsed.Lines {
		n := seen[l.StartMS]
		seen[l.StartMS]++
		ws := l.Words
		if len(ws) == 0 && !l.Decorative && n < len(words[l.StartMS]) {
			ws = words[l.StartMS][n]
		}
		pl := templates.PreviewLine{StartMS: strconv.Itoa(l.StartMS), Text: l.Text, Decorative: l.Decorative}
		if len(ws) > 0 {
			if pw, ok := previewWords(l.Text, ws); ok {
				pl.Words = pw
				hasWords = true
			}
		}
		out = append(out, pl)
	}
	return out, hasWords
}

// handlePreviewPage renders the player page for one work_queue row (#481). It
// is session-guarded and no-store (it shows library content). An unknown id, a
// row with no readable .lrc sidecar, or one outside every library root is the
// same bare 404 the audio route gives. Only the id is ever logged.
func (u *UI) handlePreviewPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve preview page", "id", id)
		http.Error(w, "preview data source unavailable", http.StatusServiceUnavailable)
		return
	}
	t, err := u.reports.PreviewSource(r.Context(), id)
	if errors.Is(err, reports.ErrPreviewNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("preview source lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	roots, err := u.reports.LibraryRoots(r.Context())
	if err != nil {
		slog.Error("preview library roots lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	// The mtime is taken BEFORE the body is read, so a file replaced in
	// between leaves the page with a stale mtime and the save refuses as
	// "changed" (the safe direction).
	lrcMTime := previewSidecarMTime(roots, t.LRCPath)
	lrc, lrcCut, ok := readPreviewSidecar(roots, t.LRCPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	elrc, elrcCut, _ := readPreviewSidecar(roots, t.ELRCCandidate)
	if elrcCut {
		// A cut companion would misalign its words with the lines; ignore it.
		elrc = ""
	}
	lines, hasWords := previewLines(lrc, elrc)
	view := templates.PreviewView{
		Artist:    t.Artist,
		Title:     t.Title,
		Album:     t.Album,
		AudioSrc:  "/preview/" + strconv.FormatInt(id, 10) + "/audio",
		Lines:     lines,
		HasWords:  hasWords,
		Truncated: lrcCut,
	}
	if u.editor != nil && !lrcCut {
		u.fillPreviewEditor(w, r, &view, t, id, roots, lrcMTime)
	}
	render(w, r, templates.PreviewPage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// previewSidecarMTime is the mtime (unix nanoseconds) of a sidecar opened
// through the same confinement as the reads; 0 when it cannot be opened.
func previewSidecarMTime(roots []string, p string) int64 {
	f, fi, ok := openPreviewAudio(roots, p)
	if !ok {
		return 0
	}
	_ = f.Close()
	return fi.ModTime().UnixNano()
}

// fillPreviewEditor sets the lyric offset editor fields (#1211 S5). A
// line-editable row gets the panel; a word-synced one (by DB tier OR by the
// parsed file) gets the read-only reason; anything else (not yet classified,
// unsynced) gets neither. A failed
// lookup or token degrades to no editor, logged, never a failed page.
func (u *UI) fillPreviewEditor(w http.ResponseWriter, r *http.Request, view *templates.PreviewView, t reports.PreviewTarget, id int64, roots []string, mtime int64) {
	// The DB tier can lag the file: a row tiered "line" whose .lrc or owned
	// .elrc now carries word timing would lose it to a line-only shift, so the
	// parsed file is consulted too and either signal makes the page read-only.
	if t.SyncTier == "word" || view.HasWords {
		view.ReadOnlyReason = "Offset editing works on line-synced files only. This file also has word timing, which a line-only shift would put out of step."
		return
	}
	if !t.LineEditable {
		return
	}
	// The editor's offset is relative to the ORIGINAL, and a save clamps
	// negative starts to 0, so the shown file cannot be un-shifted on the
	// client. The original starts come from the server, read exactly as the
	// save route reads them (.orig when present, else the .lrc).
	orig, _, err := lyrics.OriginalLines(t.LRCPath, roots)
	if err != nil {
		slog.Error("preview editor: original lines unreadable; editor off", "id", id, editErrAttr(err))
		return
	}
	if len(orig) != len(view.Lines) {
		// The shown file is not a line-for-line shift of the original, so no
		// original start can be paired with a shown line.
		slog.Error("preview editor: shown lines do not match the original; editor off", "id", id)
		return
	}
	origMS := make([]string, len(orig))
	for i, l := range orig {
		origMS[i] = strconv.Itoa(l.StartMS)
	}
	off, edited, err := u.editor.Queue.LyricEdit(r.Context(), id)
	if err != nil {
		slog.Error("preview editor: edit state lookup failed; editor off", "id", id, editErrAttr(err))
		return
	}
	if mtime <= 0 {
		slog.Error("preview editor: sidecar mtime unreadable; editor off", "id", id)
		return
	}
	token, err := ensureCSRFToken(w, r, u.secureRequest(r))
	if err != nil {
		slog.Error("preview editor: CSRF token generation failed; editor off", "id", id, "error", err)
		return
	}
	view.Editable = true
	view.EditURL = "/preview/" + strconv.FormatInt(id, 10)
	view.OffsetMS = off
	view.Edited = edited
	view.MTime = strconv.FormatInt(mtime, 10)
	view.DurationMS = u.editDuration(r, id, roots, t.AudioPath) * 1000
	view.CSRFToken = token
	view.OrigMS = strings.Join(origMS, ",")
	view.ToleranceMS = int(timing.Tolerance * 1000)
}

// previewAudioTypes maps a lowercase audio extension to its Content-Type. The
// stdlib mime table is platform-dependent and misses most of these.
var previewAudioTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".mp4":  "audio/mp4",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".wma":  "audio/x-ms-wma",
}

func previewContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := previewAudioTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); strings.HasPrefix(t, "audio/") {
		return t
	}
	return "application/octet-stream"
}

// previewWriteBound replaces the server-wide 15s WriteTimeout for one audio
// response. A browser reads a media stream with backpressure (it stops reading
// once it has buffered ahead, and while paused), so a whole-track stream
// legitimately outlives 15s by minutes. 30 minutes covers a long track played
// or paused end to end, while still bounding how long a stalled client can pin
// a goroutine and an open file; a stream cut at the bound is resumed by the
// browser with a fresh Range request from where it stopped.
const previewWriteBound = 30 * time.Minute

// openPreviewAudio opens p through an os.Root on the library root it lies
// under, so no component of the path, intermediate or final, can resolve
// outside that root at open time, whatever was swapped after the row was
// written. A root matches by its configured spelling or its symlink-resolved
// spelling, since a row may store either. The handle is fstat'ed regular.
func openPreviewAudio(roots []string, p string) (*os.File, fs.FileInfo, bool) {
	// A cheap pre-filter: filepath.Rel already refuses to relate a relative p
	// to an absolute root, so this only skips the per-root work.
	if !filepath.IsAbs(p) {
		return nil, nil, false
	}
	p = filepath.Clean(p)
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, canon := pathutil.CanonicalRoot(root)
		spellings := []string{abs, canon}
		if abs == canon {
			spellings = spellings[:1]
		}
		for _, spelling := range spellings {
			rel, ok := relUnder(spelling, p)
			if !ok {
				continue
			}
			if f, fi, ok := openInRoot(canon, rel); ok {
				return f, fi, true
			}
		}
	}
	return nil, nil, false
}

// relUnder returns p relative to dir when p lies at or beneath dir (lexically).
func relUnder(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return rel, true
}

// openInRoot opens rel read-only beneath dir via os.Root (which refuses any
// escaping component on every platform) and returns it only if it is a
// regular file. previewOpenFlags keeps a FIFO from blocking the open.
//
// os.Root also refuses every ABSOLUTE symlink, even one that stays inside dir
// (root/a.flac -> /root/B/a.flac, or a directory root/Link -> /root/B), which
// the scanner happily enqueues. On any failure other than not-exist, the path
// is therefore resolved with EvalSymlinks and, only when the result still lies
// under dir, that resolved relative path is opened through the SAME os.Root:
// a swap between the resolve and the open is still confined by the open.
func openInRoot(dir, rel string) (*os.File, fs.FileInfo, bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(rel, os.O_RDONLY|previewOpenFlags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, false
		}
		resolved, rerr := filepath.EvalSymlinks(filepath.Join(dir, rel))
		if rerr != nil {
			return nil, nil, false
		}
		// An early-out, not the authority: os.Root refuses a "../" name too.
		inner, ok := relUnder(dir, resolved)
		if !ok {
			return nil, nil, false
		}
		if f, err = root.OpenFile(inner, os.O_RDONLY|previewOpenFlags, 0); err != nil {
			return nil, nil, false
		}
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, false
	}
	return f, fi, true
}

// handlePreviewAudio streams the audio file of one work_queue row with Range
// support. The path comes from a database column, so it is opened only through
// an os.Root on one of the LIVE library roots (see openPreviewAudio). Every
// refusal is the same bare 404: it never says which check failed and never
// carries a path. The path is never logged, only the row id.
func (u *UI) handlePreviewAudio(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve preview audio", "id", id)
		http.Error(w, "preview data source unavailable", http.StatusServiceUnavailable)
		return
	}
	audioPath, err := u.reports.PreviewAudioPath(r.Context(), id)
	if errors.Is(err, reports.ErrPreviewNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("preview source lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	roots, err := u.reports.LibraryRoots(r.Context())
	if err != nil {
		slog.Error("preview library roots lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	f, fi, ok := openPreviewAudio(roots, audioPath)
	if !ok {
		slog.Warn("preview audio refused: not a regular file under a library root", "id", id)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(previewWriteBound)); err != nil {
		slog.Error("preview audio: cannot extend the write deadline; long streams will be cut", "id", id, "error", err)
	}
	w.Header().Set("Content-Type", previewContentType(audioPath))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
