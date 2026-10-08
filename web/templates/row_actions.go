package templates

import (
	"net/url"
	"strconv"
	"strings"
)

// Mark status codes (#1249). A mark action redirects back to the page it came
// from with ?mark=<code>; the code, never free text, picks the sentence, so a
// crafted link cannot put words on the page.
const (
	MarkStatusParam = "mark"
	MarkFilesParam  = "files"

	MarkInstrumentalDone   = "instrumental_marked"
	MarkInstrumentalUndone = "instrumental_undone"
	MarkWrongDone          = "wrong_marked"
	MarkUnblocked          = "unblocked"
	MarkNothingToDo        = "nothing_to_do"
	MarkNotFound           = "not_found"
	MarkInFlight           = "in_flight"
	MarkNoLyricFile        = "no_lyric_file"
	MarkIsInstrumental     = "is_instrumental"
	MarkNotMarkable        = "not_markable"
	MarkFailed             = "failed"
)

// MarkIsSuccess reports whether code is a completed action.
func MarkIsSuccess(code string) bool {
	switch code {
	case MarkInstrumentalDone, MarkInstrumentalUndone, MarkWrongDone, MarkUnblocked:
		return true
	}
	return false
}

// MarkStatusText is the one-line status for a code ("" for an unknown code).
// files is the lyric-file count the action reported. No sentence names a path.
// A count outside 0..maxMarkFiles is treated as no count, as is zero: with no
// file the sentence says only what happened, never that files were removed.
func MarkStatusText(code string, files int) string {
	removed := func() string {
		switch {
		case files < 1 || files > maxMarkFiles:
			return ""
		case files == 1:
			return " 1 lyric file removed and saved to a backup file."
		}
		return " " + strconv.Itoa(files) + " lyric files removed and saved to a backup file."
	}
	switch code {
	case MarkInstrumentalDone:
		return "Marked instrumental." + removed()
	case MarkInstrumentalUndone:
		return "Instrumental mark removed. The track will be looked up again."
	case MarkWrongDone:
		if r := removed(); r != "" {
			return "Lyrics marked wrong." + strings.TrimSuffix(r, ".") + "; the track will be looked up again."
		}
		return "Lyrics marked wrong. The track will be looked up again."
	case MarkUnblocked:
		return "Unblocked. The track will be looked up again."
	case MarkNothingToDo:
		return "Nothing changed: the track is already in that state."
	case MarkNotFound:
		return "Not done: that track no longer exists."
	case MarkInFlight:
		return "Not done: the track is being processed right now. Try again in a moment."
	case MarkNoLyricFile:
		return "Not done: there is no lyric file on disk for this track."
	case MarkIsInstrumental:
		return "Not done: the track is marked instrumental. Undo that first."
	case MarkNotMarkable:
		return "Not done: the track failed or was given up on. Revive it first."
	case MarkFailed:
		return "Failed: see the server log for details. Nothing further was changed."
	}
	return ""
}

// MarkStatusFromQuery builds the status line for a page's query, or "" when the
// page was not reached by a mark redirect (or carried an unknown code).
func MarkStatusFromQuery(q url.Values) string {
	code := q.Get(MarkStatusParam)
	files := 0
	// Only the two codes that report a file count read it; MarkStatusText
	// enforces the bound itself, so this parse is not the only guard.
	if code == MarkInstrumentalDone || code == MarkWrongDone {
		if n, err := strconv.Atoi(q.Get(MarkFilesParam)); err == nil {
			files = n
		}
	}
	return MarkStatusText(code, files)
}

// maxMarkFiles bounds the file count a status line may display; a count outside
// 0..maxMarkFiles is treated as no count.
const maxMarkFiles = 1000

// localReturn is the template layer's own copy of the handler's return-path
// check (internal/web safeReturn, which templates must not import): only a
// local absolute path survives, anything else is the fallback, so a template
// never renders an off-site link even if a caller passes one.
func localReturn(s, fallback string) string {
	if s == "" || s[0] != '/' || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\") {
		return fallback
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fallback
		}
	}
	if p, err := url.Parse(s); err != nil || p.Scheme != "" || p.Host != "" {
		return fallback
	}
	return s
}

// markReturnFallback is the landing page used when a return value is not local.
const markReturnFallback = "/queue"

// safeMarkReturn is localReturn with the queue page as the fallback.
func safeMarkReturn(s string) string { return localReturn(s, markReturnFallback) }

// RowActions is the per-row action model: which pills and links a table row
// shows. ID zero (marking unavailable) renders nothing.
type RowActions struct {
	ID int64
	// Return is the local path the action returns to.
	Return string
	// HasLyric: a lyric file is written. Manual: marked instrumental by hand.
	// Blocked: the track has a blocked result. InFlight: a worker holds the row.
	HasLyric, Manual, Blocked, InFlight bool
}

// Href is the confirm-page URL for one action path.
func (a RowActions) Href(path string) string {
	h := "/queue/" + strconv.FormatInt(a.ID, 10) + "/" + path
	if a.Return != "" {
		h += "?return=" + url.QueryEscape(a.Return)
	}
	return h
}

// ShowInstrumental reports whether the instrumental icon shows: any row not in flight (an Undo link when already marked).
func (a RowActions) ShowInstrumental() bool { return !a.InFlight }

// ShowWrong reports whether "Lyrics are wrong" shows: only a row with a lyric written, not blocked, not hand-marked.
func (a RowActions) ShowWrong() bool { return a.HasLyric && !a.Blocked && !a.Manual && !a.InFlight }

// ShowUnblock reports whether Unblock shows: a blocked row offers Unblock in place of "Lyrics are wrong".
func (a RowActions) ShowUnblock() bool { return a.Blocked && !a.InFlight }

// HasLinks reports whether any link is offered (so the phone menu is not empty).
func (a RowActions) HasLinks() bool { return a.ShowInstrumental() || a.ShowWrong() || a.ShowUnblock() }

// MarkConfirmView is the model of a confirm page. Alert set means the action is
// refused and the page has no form.
type MarkConfirmView struct {
	Title, Track, Confirm, Return, Action, CSRFToken, Alert string
	Notes                                                   []string
	// Files, when non-nil, is the lyric-file count the dry run found.
	Files *int
}
