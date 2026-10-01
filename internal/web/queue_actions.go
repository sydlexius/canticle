package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/web/templates"
)

// reviveScopeAll is the library form value meaning every library.
const reviveScopeAll = "all"

// reviveMaxAttempts bounds the SQLITE_BUSY retries around the revive write.
// It mirrors the queue package's own attempt budget for serve-side writes.
const reviveMaxAttempts = 5

const (
	reviveStaleNotice   = "The counts changed since you loaded this page. Review the numbers below and confirm again."
	reviveNoLibraryText = "That library has no retired tracks left; the list has been refreshed."
)

// handleReviveRetiredPreview renders the revive blast radius (#598): per-library
// counts, the shared-row note, a library picker, and the confirm form for the
// previewed scope. It is a GET and never mutates.
func (u *UI) handleReviveRetiredPreview(w http.ResponseWriter, r *http.Request) {
	// Library names are operational detail; keep the page out of caches (set
	// first so an early error response is uncached too).
	w.Header().Set("Cache-Control", "no-store")
	if u.queueActions == nil {
		http.Error(w, "queue actions unavailable", http.StatusServiceUnavailable)
		return
	}
	// Default to "all" only when the key is absent: an explicitly empty value is
	// malformed and must reach parseReviveScope, not widen to every library.
	scope := reviveScopeAll
	if q := r.URL.Query(); q.Has("library") {
		scope = q.Get("library")
	}
	view, ok := u.buildReviveView(w, r, scope)
	if !ok {
		return
	}
	u.renderRevive(w, r, view)
}

// handleReviveRetiredConfirm revives the retired rows of the chosen scope.
// Order mirrors the keys page: same-origin, then CSRF, then the backend work,
// then an audit line (counts and ids only, never row content).
func (u *UI) handleReviveRetiredConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !enforceSameOrigin(w, r) {
		return
	}
	if !enforceCSRFToken(w, r) {
		return
	}
	if u.queueActions == nil {
		http.Error(w, "queue actions unavailable", http.StatusServiceUnavailable)
		return
	}
	// A missing field defaults to "all"; an explicitly empty one is malformed.
	_ = r.ParseForm()
	scope := reviveScopeAll
	if r.PostForm.Has("library") {
		scope = r.PostForm.Get("library")
	}
	libraryID, ok := parseReviveScope(scope)
	if !ok {
		http.Error(w, "invalid library", http.StatusBadRequest)
		return
	}

	// Read the live scope fresh. A library the preview no longer lists (for
	// example a refresh after a successful revive) re-renders the page with a
	// notice rather than failing.
	before, ok := u.buildReviveView(w, r, scope)
	if !ok {
		return
	}
	if before.Error != "" {
		u.renderRevive(w, r, before)
		return
	}
	// The confirm is bound to the count the operator saw. If the population
	// moved since the page loaded, show the new numbers instead of reviving.
	expected, err := strconv.ParseInt(r.PostFormValue("expected"), 10, 64)
	if err != nil || expected < 0 {
		http.Error(w, "invalid expected count", http.StatusBadRequest)
		return
	}
	if expected != before.ScopeCount {
		before.Error = reviveStaleNotice
		u.renderRevive(w, r, before)
		return
	}
	// Name the scope from the pre-revive preview: afterwards the library has no
	// retired rows left and would drop out of the listing.
	label := before.ScopeLabel

	// RecheckRetired runs a deferred transaction on serve's read-write handle; a
	// concurrent commit can fail the lock upgrade with SQLITE_BUSY at once
	// (#978), which busy_timeout cannot cure. The transaction rolls back whole on
	// error, so a retry re-runs it from a clean slate.
	var revived int64
	err = db.RetryOnBusy(r.Context(), reviveMaxAttempts, func() error {
		var rerr error
		revived, rerr = u.queueActions.RecheckRetiredExpect(r.Context(), libraryID, expected)
		return rerr
	})
	if errors.Is(err, queue.ErrRecheckRetiredCountChanged) {
		// The count moved between the pre-check and the write (or across a busy
		// retry): re-read the live numbers and ask again.
		fresh, ok := u.buildReviveView(w, r, scope)
		if !ok {
			return
		}
		fresh.Error = reviveStaleNotice
		u.renderRevive(w, r, fresh)
		return
	}
	if err != nil {
		slog.Error("queue: revive retired failed", "scope", scope, "error", err)
		http.Error(w, "revive failed", http.StatusInternalServerError)
		return
	}
	// Audit trail: who/what rows are deliberately absent; ids and counts only.
	slog.Info("queue: retired rows revived", "event", "revive_retired", "scope", scope, "revived", revived)

	view, ok := u.buildReviveView(w, r, reviveScopeAll)
	if !ok {
		return
	}
	view.Result = &templates.ReviveResult{Revived: revived, Scope: label}
	u.renderRevive(w, r, view)
}

// parseReviveScope maps the library form value to RecheckRetiredExpect's argument:
// nil for "all", else a positive library id. ok is false for anything else.
func parseReviveScope(scope string) (libraryID *int64, ok bool) {
	if scope == reviveScopeAll {
		return nil, true
	}
	id, err := strconv.ParseInt(scope, 10, 64)
	if err != nil || id <= 0 {
		return nil, false
	}
	return &id, true
}

// buildReviveView assembles the page model for a scope, reading the live
// preview. It writes the error response and returns ok=false on a malformed
// scope or a failed read. A well-formed library id the preview does not list
// yields the all-libraries view with Error set (and ok=true).
func (u *UI) buildReviveView(w http.ResponseWriter, r *http.Request, scope string) (templates.ReviveView, bool) {
	var view templates.ReviveView
	if _, ok := parseReviveScope(scope); !ok {
		http.Error(w, "invalid library", http.StatusBadRequest)
		return view, false
	}
	p, err := u.queueActions.RecheckRetiredPreview(r.Context())
	if err != nil {
		slog.Error("queue: revive preview failed", "error", err)
		http.Error(w, "preview failed", http.StatusInternalServerError)
		return view, false
	}
	view = reviveViewFromPreview(p, scope)
	if view.Selected != scope {
		view.Error = reviveNoLibraryText
	}
	return view, true
}

// reviveViewFromPreview maps the queue preview onto the view for a scope.
// Selected is left as "all" when the scope names a library the preview does not
// list, so the caller can tell.
func reviveViewFromPreview(p queue.RecheckRetiredPreview, scope string) templates.ReviveView {
	view := templates.ReviveView{
		Total: p.Total, Shared: p.Shared, Unlinked: p.Unlinked,
		Selected: reviveScopeAll, ScopeLabel: "all libraries",
		ScopeCount: p.Total, ScopeShared: p.Shared,
	}
	for _, l := range p.Libraries {
		id := strconv.FormatInt(l.LibraryID, 10)
		view.Libraries = append(view.Libraries, templates.ReviveLibrary{ID: id, Name: l.Name, Count: l.Count, Shared: l.Shared})
		if id == scope {
			view.Selected = id
			view.ScopeLabel = l.Name
			view.ScopeCount = l.Count
			view.ScopeShared = l.Shared
		}
	}
	return view
}

// renderRevive attaches a CSRF token to the view and renders the page. When no
// token can be issued the page stays read-only (no confirm form).
func (u *UI) renderRevive(w http.ResponseWriter, r *http.Request, view templates.ReviveView) {
	token, err := ensureCSRFToken(w, r, u.secureRequest(r))
	if err != nil {
		slog.Error("queue: CSRF token generation failed; revive form disabled", "error", err)
	} else {
		view.Manageable = true
		view.CSRFToken = token
	}
	render(w, r, templates.ReviveRetiredPage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}
