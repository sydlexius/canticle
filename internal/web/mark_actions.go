package web

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/sydlexius/canticle/internal/instrumentalmark"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/web/templates"
)

// InstrumentalMarker is instrumentalmark.Marker's surface (#1249).
type InstrumentalMarker interface {
	Mark(ctx context.Context, id int64, opts instrumentalmark.Options) (instrumentalmark.Result, error)
	Unmark(ctx context.Context, id int64, opts instrumentalmark.Options) (instrumentalmark.Result, error)
}

// LyricBlocker is lyricblock.Service's surface (#1249).
type LyricBlocker interface {
	Mark(ctx context.Context, req lyricblock.MarkRequest) (lyricblock.MarkResult, error)
	Unblock(ctx context.Context, req lyricblock.UnblockRequest) (lyricblock.UnblockResult, error)
}

// MarkDeps wires the mark actions (#1249): the two services, the database they
// and the track lookup read, and the folder the JSONL backups go to (beside the
// database, as the CLI does). An empty DBPath leaves the routes answering 404.
type MarkDeps struct {
	DB           *sql.DB
	Instrumental InstrumentalMarker
	Blocks       LyricBlocker
	// DBPath is the database file; backups are written to its folder.
	DBPath string
}

// AttachMarkActions enables the /queue/{id}/instrumental, .../instrumental/undo,
// .../wrong and .../unblock confirm and action routes.
func (u *UI) AttachMarkActions(d MarkDeps) { u.mark = &d }

func (u *UI) registerMarkRoutes(reg routeReg) {
	for _, k := range markKinds {
		reg("GET /queue/{id}/"+k.path, u.markConfirm(k))
		reg("POST /queue/{id}/"+k.path, u.markPost(k))
	}
}

// markKind is one of the four actions.
type markKind struct {
	path, stem, title, confirm string
	// what is the plain-words consequence, shown on the confirm page.
	what []string
}

var markKinds = []markKind{
	{path: "instrumental", stem: "instrumental-mark", title: "Mark instrumental", confirm: "Mark instrumental", what: []string{
		"Any lyric files for this track are removed and saved to a backup file in the database folder, and the track is marked instrumental. It will not be looked up again until you undo this.",
	}},
	{path: "instrumental/undo", stem: "instrumental-unmark", title: "Undo instrumental mark", confirm: "Undo", what: []string{
		"The instrumental mark is removed, its marker files are removed and saved to a backup file in the database folder, and the track is looked up again.",
	}},
	{path: "wrong", stem: "mark-wrong", title: "Lyrics are wrong", confirm: "Remove and block", what: []string{
		"These exact words are never written for this track again. The lyric files are removed and saved to a backup file in the database folder, and the track is looked up again.",
	}},
	{path: "unblock", title: "Unblock lyrics", confirm: "Unblock", what: []string{
		"The words blocked for this track can be written again. No lyric file is restored; the track is looked up again.",
	}},
}

// safeReturn accepts only a local path (no scheme, host, or protocol-relative
// form) so the post-action redirect can never leave the site; anything else is
// the queue landing page.
func safeReturn(raw string) string {
	return safeLocalPath(raw, "/queue")
}

// markTrack reads the track's display name by work item id, never from the request.
func (u *UI) markTrack(ctx context.Context, id int64) (string, error) {
	var artist, title string
	err := u.mark.DB.QueryRowContext(ctx, `SELECT artist, title FROM work_queue WHERE id = ?`, id).Scan(&artist, &title)
	return artist + " - " + title, err
}

// markRun runs one action (dry for the confirm page's preview) and returns the
// number of lyric files affected and a status code. Errors carry ids only.
func (u *UI) markRun(ctx context.Context, k markKind, id int64, dry bool) (files int, code string) {
	var bk *lyrics.LazyBackupFile
	if !dry && k.stem != "" {
		bk = &lyrics.LazyBackupFile{Path: lyrics.DefaultBackupPath(u.mark.DBPath, k.stem, time.Now()), What: k.stem + " backup"}
		defer bk.Close()
	}
	switch k.path {
	case "instrumental", "instrumental/undo":
		opts := instrumentalmark.Options{DryRun: dry}
		if bk != nil {
			opts.Report = func(rec instrumentalmark.Record) error {
				f, err := bk.File()
				if err != nil {
					return err
				}
				return instrumentalmark.AppendRecord(f, rec)
			}
		}
		run := u.mark.Instrumental.Mark
		okCode := templates.MarkInstrumentalDone
		if k.path != "instrumental" {
			run, okCode = u.mark.Instrumental.Unmark, templates.MarkInstrumentalUndone
		}
		res, err := run(ctx, id, opts)
		switch {
		case err == nil && (res.Outcome == instrumentalmark.OutcomeAlreadyMarked || res.Outcome == instrumentalmark.OutcomeNotMarked):
			return res.FilesBackedUp, templates.MarkNothingToDo
		case err == nil:
			return res.FilesBackedUp, okCode
		case errors.Is(err, queue.ErrManualInstrumentalNotFound):
			return 0, templates.MarkNotFound
		case errors.Is(err, queue.ErrManualInstrumentalInFlight):
			return 0, templates.MarkInFlight
		}
		slog.Error("web: instrumental mark failed", "action", k.path, "work_item", id, "error_class", markErrClass(err))
		return 0, templates.MarkFailed
	case "wrong":
		libs, err := library.New(u.mark.DB).List(ctx)
		if err != nil {
			slog.Error("web: mark wrong: list libraries failed", "work_item", id, "error_class", markErrClass(err))
			return 0, templates.MarkFailed
		}
		roots := make([]string, 0, len(libs))
		for _, l := range libs {
			roots = append(roots, l.Path)
		}
		req := lyricblock.MarkRequest{WorkItemID: id, Roots: roots, DryRun: dry}
		if bk != nil {
			req.Report = func(b lyricblock.Backup) error {
				f, err := bk.File()
				if err != nil {
					return err
				}
				return lyricblock.AppendBackup(f, b)
			}
		}
		res, err := u.mark.Blocks.Mark(ctx, req)
		if err == nil {
			return res.Files, templates.MarkWrongDone
		}
		if c := markRefusalCode(lyricblock.Refusal(err, res)); c != "" {
			return 0, c
		}
		slog.Error("web: mark wrong failed", "work_item", id, "error_class", markErrClass(err))
		return res.Files, templates.MarkFailed
	}
	res, err := u.mark.Blocks.Unblock(ctx, lyricblock.UnblockRequest{WorkItemID: id, DryRun: dry})
	switch {
	case err == nil && res.Removed == 0:
		slog.Info("web: unblock found no block", "work_item", id, "removed", res.Removed, "reopened", res.Reopened, "dry_run", dry)
		return 0, templates.MarkNothingToDo
	case err == nil:
		return res.Removed, templates.MarkUnblocked
	case errors.Is(err, lyricblock.ErrNotFound):
		return 0, templates.MarkNotFound
	}
	slog.Error("web: unblock failed", "work_item", id, "error_class", markErrClass(err))
	return 0, templates.MarkFailed
}

// markErrClass names the kind of an error for the log without printing it. The
// services' errors can carry a library path (a sidecar path holds the library's
// private artist and title) or free text, so a log line gets only a stable class:
// a known sentinel, a context or SQL condition, or the bare errno of a path
// error. Anything else is "unclassified"; err.Error() is never logged.
func markErrClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, instrumentalmark.ErrNoLibrary):
		return "instrumentalmark: no library"
	case errors.Is(err, instrumentalmark.ErrSymlinkedSidecar), errors.Is(err, lyricblock.ErrSymlinkedSidecar):
		return "symlinked sidecar"
	case errors.Is(err, instrumentalmark.ErrNoBackupSink), errors.Is(err, lyricblock.ErrNoBackupSink):
		return "no backup sink"
	case errors.Is(err, instrumentalmark.ErrMarkWithdrawn):
		return "mark withdrawn"
	case errors.Is(err, lyricblock.ErrOutsideRoots):
		return "outside library roots"
	case errors.Is(err, lyricblock.ErrNoFingerprint):
		return "no fingerprint"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, sql.ErrNoRows):
		return "no rows"
	case errors.Is(err, fs.ErrNotExist):
		return "file does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrExist):
		return "file exists"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "errno: " + errno.Error()
	}
	return "unclassified"
}

// markRefusalCode maps lyricblock.Refusal's phrase to a status code.
func markRefusalCode(phrase string) string {
	switch phrase {
	case "not found":
		return templates.MarkNotFound
	case "in flight":
		return templates.MarkInFlight
	case "no lyric file on disk":
		return templates.MarkNoLyricFile
	case "marked instrumental by hand":
		return templates.MarkIsInstrumental
	case "failed or unavailable":
		return templates.MarkNotMarkable
	}
	return ""
}

// markEntry is the shared front of both handlers: the 404 gates and the id.
func (u *UI) markEntry(w http.ResponseWriter, r *http.Request) (int64, bool) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if u.mark == nil || u.mark.DBPath == "" || err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// markConfirm renders the confirm page: the track (read by id), what will
// happen in plain words, and the real file count from the service's dry run.
func (u *UI) markConfirm(k markKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := u.markEntry(w, r)
		if !ok {
			return
		}
		track, err := u.markTrack(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("web: mark confirm: track lookup failed", "work_item", id, "error_class", markErrClass(err))
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		view := templates.MarkConfirmView{
			Title: k.title, Track: track, Confirm: k.confirm, Return: safeReturn(r.URL.Query().Get("return")),
			Action: "/queue/" + strconv.FormatInt(id, 10) + "/" + k.path,
		}
		files, code := u.markRun(r.Context(), k, id, true)
		if code != "" && !templates.MarkIsSuccess(code) {
			view.Alert = templates.MarkStatusText(code, 0)
		} else {
			view.Notes = append(view.Notes, k.what...)
			if k.path == "wrong" || k.path == "instrumental" {
				view.Files = &files
			}
			token, terr := ensureCSRFToken(w, r, u.secureRequest(r))
			if terr != nil {
				slog.Error("web: mark confirm: CSRF token generation failed", "error", terr)
				view.Alert = templates.MarkStatusText(templates.MarkFailed, 0)
			}
			view.CSRFToken = token
		}
		render(w, r, templates.MarkConfirmPage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
	}
}

// markPost applies the action: same-origin, then CSRF, then the service, then a
// redirect back to the page the operator came from carrying a status code.
func (u *UI) markPost(k markKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
			return
		}
		id, ok := u.markEntry(w, r)
		if !ok {
			return
		}
		files, code := u.markRun(r.Context(), k, id, false)
		slog.Info("web: mark action", "event", "mark_action", "action", k.path, "work_item", id, "result", code)
		dest, _ := url.Parse(safeReturn(r.PostFormValue("return")))
		q := dest.Query()
		q.Set(templates.MarkStatusParam, code)
		if files > 0 {
			q.Set(templates.MarkFilesParam, strconv.Itoa(files))
		}
		dest.RawQuery = q.Encode()
		dest.Fragment = ""
		http.Redirect(w, r, dest.String(), http.StatusSeeOther) //nolint:gosec // reason: G710 -- dest is built from safeReturn, which admits only a local path (no scheme, host or //)
	}
}
