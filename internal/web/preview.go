package web

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// handlePreviewAudio streams the audio file of one work_queue row with Range
// support. The path comes from a database column, so it is confined to the LIVE
// library roots (symlink-resolved on both sides) and opened without following a
// symlink. Every refusal is the same bare 404: it never says which check failed
// and never carries a path. The path is never logged, only the row id.
func (u *UI) handlePreviewAudio(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	target, err := u.reports.PreviewSource(r.Context(), id)
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
	resolved := ""
	for _, root := range roots {
		if p, ok := pathutil.ResolveWithinRoot(root, target.AudioPath); ok {
			resolved = p
			break
		}
	}
	if resolved == "" {
		slog.Warn("preview audio refused: not under a library root", "id", id)
		http.NotFound(w, r)
		return
	}
	f, err := openPreviewAudio(resolved)
	if err != nil {
		slog.Warn("preview audio open failed", "id", id)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// The handle must be the file the confined path names right now, not
	// something swapped in between the resolve and the open.
	if li, err := os.Lstat(resolved); err != nil || !os.SameFile(fi, li) {
		slog.Warn("preview audio refused: path changed during open", "id", id)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", previewContentType(resolved))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
