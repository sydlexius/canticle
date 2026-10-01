package web

import (
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/reports"
)

// registerPreviewRoutes registers the read-only preview player routes (#481)
// through reg, so they are guarded exactly like every other page route.
func (u *UI) registerPreviewRoutes(reg routeReg) {
	reg("GET /preview/{id}/audio", u.handlePreviewAudio)
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
