package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/repair"
	"github.com/sydlexius/canticle/web/templates"
)

// RepairRunner is the surface of *repair.Runner the repair routes use (#1544).
type RepairRunner interface {
	Start(key repair.Key, o repair.Options) (int64, error)
	Snapshot() repair.Snapshot
	CancelKey(key repair.Key) (int64, bool)
}

// RepairDeps wires the operator repair routes (#1544, epic #1537): the registry
// the keys resolve against, the runner that runs them one at a time, and the
// database file whose folder an applying run's backup goes to (as the mark
// actions' backups do). A nil Registry or Runner leaves the routes answering 404.
type RepairDeps struct {
	Registry *repair.Registry
	Runner   RepairRunner
	DBPath   string
}

// AttachRepairs enables the /repairs/{key} confirm, start, status and cancel
// routes. serve builds the registry and hosts the runner (#1545).
func (u *UI) AttachRepairs(d RepairDeps) { u.repairs = &d }

func (u *UI) registerRepairRoutes(reg routeReg) {
	reg("GET /repairs/{key}", u.handleRepairPage)
	reg("POST /repairs/{key}", u.handleRepairStart)
	reg("GET /repairs/{key}/status", u.handleRepairStatus)
	reg("POST /repairs/{key}/cancel", u.handleRepairCancel)
}

// repairEntry is the shared front of every repair handler: the 404 gates and
// the registered repair for the path's key.
func (u *UI) repairEntry(w http.ResponseWriter, r *http.Request) (repair.Repair, repair.Info, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if u.repairs == nil || u.repairs.Registry == nil || u.repairs.Runner == nil {
		http.NotFound(w, r)
		return nil, repair.Info{}, false
	}
	// Register admits only a valid Key, so a malformed one is unknown too.
	rep, ok := u.repairs.Registry.Get(repair.Key(r.PathValue("key")))
	if !ok {
		http.NotFound(w, r)
		return nil, repair.Info{}, false
	}
	return rep, rep.Info(), true
}

// repairView is the run page's view for info in state s, with its form and
// poll targets and a CSRF token when the state carries a form. A token that
// cannot be minted is an error: the caller answers 500 (see repairViewOr500),
// never a form that would post an empty token and fail with a bare 403.
func (u *UI) repairView(w http.ResponseWriter, r *http.Request, info repair.Info, s templates.RepairState) (templates.RepairRunView, error) {
	base := "/repairs/" + string(info.Key)
	v := templates.RepairRunView{
		State: s, Title: info.Title, Description: info.Description, CostNote: info.CostNote,
		ReadsFiles: info.ReadsFiles, NeedsBackup: info.NeedsBackup,
		StartAction: base, CancelAction: base + "/cancel", PollHref: base + "/status", BackHref: dashboardPath,
	}
	if s != templates.RepairFinished {
		token, err := ensureCSRFToken(w, r, u.secureRequest(r))
		if err != nil {
			return v, fmt.Errorf("repair view: %w", err)
		}
		v.CSRFToken = token
	}
	return v, nil
}

// repairViewOr500 is repairView that answers a plain fixed 500 itself when the
// CSRF token cannot be minted; ok is false then. The error's text is never
// shown or logged, only its class.
func (u *UI) repairViewOr500(w http.ResponseWriter, r *http.Request, info repair.Info, s templates.RepairState) (templates.RepairRunView, bool) {
	v, err := u.repairView(w, r, info, s)
	if err != nil {
		slog.Error("web: repair: CSRF token generation failed", "repair", string(info.Key), "error_class", markErrClass(err))
		http.Error(w, "The page could not be built. Details are in the server log.", http.StatusInternalServerError)
		return v, false
	}
	return v, true
}

// liveFor reports whether snapshot s is a live run of key.
func liveFor(s repair.Snapshot, key repair.Key) bool {
	return s.Key == key && s.State == repair.StateRunning
}

// fillRun copies a run snapshot's progress or outcome into v. A snapshot of
// another repair (or none) marks the view NoRun, with no counts: it holds no
// outcome of this repair, so it must not read as "The run finished."
func fillRun(v *templates.RepairRunView, s repair.Snapshot, key repair.Key) {
	if s.Key != key {
		v.NoRun = true
		return
	}
	v.Done, v.Total = s.Done, s.Total
	v.Failed, v.Canceled = s.State == repair.StateFailed, s.State == repair.StateCanceled
	for _, c := range s.Counts {
		v.Counts = append(v.Counts, templates.RepairCountLine{Key: string(c.Key), N: c.N})
	}
}

// handleRepairPage renders the run page: the live run's progress, the
// finished run the ?run= id names, or else the confirm state with the pending
// count. The count comes from Pending, which reads the database only; nothing
// here runs the repair, not even as a dry run (a dry run can read the library).
func (u *UI) handleRepairPage(w http.ResponseWriter, r *http.Request) {
	rep, info, ok := u.repairEntry(w, r)
	if !ok {
		return
	}
	snap := u.repairs.Runner.Snapshot()
	switch {
	case liveFor(snap, info.Key):
		v, ok := u.repairViewOr500(w, r, info, templates.RepairRunning)
		if !ok {
			return
		}
		fillRun(&v, snap, info.Key)
		u.renderRepairPage(w, r, v)
		return
	case snap.Key == info.Key && snap.ID > 0 && r.URL.Query().Get("run") == strconv.FormatInt(snap.ID, 10):
		v, ok := u.repairViewOr500(w, r, info, templates.RepairFinished)
		if !ok {
			return
		}
		fillRun(&v, snap, info.Key)
		u.renderRepairPage(w, r, v)
		return
	}
	n, err := rep.Pending(r.Context(), repair.Options{})
	if err != nil {
		slog.Error("web: repair pending count failed", "repair", string(info.Key), "error_class", markErrClass(err))
		http.Error(w, "The pending count could not be read. Details are in the server log.", http.StatusInternalServerError)
		return
	}
	v, ok := u.repairViewOr500(w, r, info, templates.RepairConfirm)
	if !ok {
		return
	}
	v.Pending = n
	u.renderRepairPage(w, r, v)
}

func (u *UI) renderRepairPage(w http.ResponseWriter, r *http.Request, v templates.RepairRunView) {
	render(w, r, templates.RepairRunPage(u.version, v, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// handleRepairStart starts a run: same-origin, then CSRF, then the pending
// count (refused when zero), then the runner (which refuses while any run is
// live), then a redirect to the run page.
// Refusals are plain text; an error's own text is never shown or logged.
func (u *UI) handleRepairStart(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	rep, info, ok := u.repairEntry(w, r)
	if !ok {
		return
	}
	key := string(info.Key)
	n, err := rep.Pending(r.Context(), repair.Options{})
	if err != nil {
		slog.Error("web: repair pending count failed", "repair", key, "error_class", markErrClass(err))
		http.Error(w, "The pending count could not be read. Details are in the server log.", http.StatusInternalServerError)
		return
	}
	if n <= 0 {
		slog.Info("web: repair start refused", "repair", key, "reason", "nothing_pending", "pending", n)
		http.Error(w, "Nothing is pending for this repair.", http.StatusConflict)
		return
	}
	o := repair.Options{Apply: true}
	if info.NeedsBackup && u.repairs.DBPath != "" {
		o.Backup = lyrics.DefaultBackupPath(u.repairs.DBPath, key, time.Now())
	}
	id, err := u.repairs.Runner.Start(info.Key, o)
	if errors.Is(err, repair.ErrBusy) {
		// A double-submitted Start finds its own run live: show that run
		// rather than a refusal of it.
		if snap := u.repairs.Runner.Snapshot(); liveFor(snap, info.Key) {
			slog.Info("web: repair start joined the live run", "repair", key, "run", snap.ID)
			http.Redirect(w, r, "/repairs/"+key+"?run="+strconv.FormatInt(snap.ID, 10), http.StatusSeeOther)
			return
		}
	}
	if err != nil {
		u.repairStartRefused(w, r, key, err)
		return
	}
	slog.Info("web: repair started", "event", "repair_start", "repair", key, "pending", n, "run", id)
	http.Redirect(w, r, "/repairs/"+key+"?run="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// repairStartRefused answers a runner refusal with a plain message.
func (u *UI) repairStartRefused(w http.ResponseWriter, r *http.Request, key string, err error) {
	code, msg, reason := http.StatusInternalServerError, "The repair could not be started. Details are in the server log.", "error"
	var class string
	switch {
	case errors.Is(err, repair.ErrBusy):
		code, msg, reason, class = http.StatusConflict, "Another repair is already running. Wait for it to finish, then start this one.", "busy", "repair: busy"
	case errors.Is(err, repair.ErrUnknownRepair):
		http.NotFound(w, r)
		return
	case errors.Is(err, repair.ErrClosed):
		code, msg, reason, class = http.StatusServiceUnavailable, "The server is shutting down.", "closed", "repair: runner closed"
	case errors.Is(err, repair.ErrBackupRequired):
		code, msg, reason, class = http.StatusServiceUnavailable, "This repair needs a backup location, and none is set.", "no_backup", "repair: backup required"
	default:
		class = markErrClass(err)
	}
	slog.Warn("web: repair start refused", "repair", key, "reason", reason, "error_class", class)
	http.Error(w, msg, code)
}

// handleRepairStatus is the htmx poll. It answers 200 with the
// RepairRunBody fragment: htmx swaps no error response and would keep polling,
// so when this repair has no live run the finished state (which carries no
// hx-trigger) ends the polling.
func (u *UI) handleRepairStatus(w http.ResponseWriter, r *http.Request) {
	_, info, ok := u.repairEntry(w, r)
	if !ok {
		return
	}
	snap := u.repairs.Runner.Snapshot()
	state := templates.RepairFinished
	if liveFor(snap, info.Key) {
		state = templates.RepairRunning
	}
	v, ok := u.repairViewOr500(w, r, info, state)
	if !ok {
		return
	}
	fillRun(&v, snap, info.Key)
	render(w, r, templates.RepairRunBody(v))
}

// handleRepairCancel cancels this repair's live run (never another repair's:
// CancelKey checks the key under the runner's lock) and returns to that run's
// page, or to the confirm page when this repair had no live run.
func (u *UI) handleRepairCancel(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) || !enforceCSRFToken(w, r) {
		return
	}
	_, info, ok := u.repairEntry(w, r)
	if !ok {
		return
	}
	page := "/repairs/" + string(info.Key)
	id, canceled := u.repairs.Runner.CancelKey(info.Key)
	if !canceled {
		slog.Info("web: repair cancel", "event", "repair_cancel", "repair", string(info.Key), "canceled", false)
		http.Redirect(w, r, page, http.StatusSeeOther)
		return
	}
	slog.Info("web: repair cancel", "event", "repair_cancel", "repair", string(info.Key), "run", id, "canceled", true)
	http.Redirect(w, r, page+"?run="+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
