package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
	"github.com/sydlexius/mxlrcgo-svc/web/templates"
)

// recentOutcomesLimit caps the Recent outcomes report. It is a display bound,
// not a query budget: the report shows the most recent N completed tracks.
const recentOutcomesLimit = 50

// reportTimeFormat renders run and completion timestamps in the server's local
// timezone with a zone abbreviation, so an operator reads the value in whatever
// zone the daemon runs in (typically UTC in a container) rather than a silently
// converted one.
const reportTimeFormat = "2006-01-02 15:04:05 MST"

// reportDef is the static metadata for one canned report. The slice order is the
// rail order from the #186 design of record.
type reportDef struct {
	key      string
	title    string
	subtitle string
}

var reportDefs = []reportDef{
	{"queue-summary", "Queue summary", "Work-queue rows grouped by status."},
	{"recent-outcomes", "Recent outcomes", "The most recently completed tracks and their derived result."},
	{"provider-effectiveness", "Provider effectiveness", "Per-lane hits, misses, and true per-track hit-rate."},
	{"instrumental-inventory", "Instrumental inventory", "Tracks the audio detector confirmed instrumental."},
	{"failure-analysis", "Failure analysis", "Failed and deferred tracks grouped by reason."},
}

func lookupReportDef(key string) (reportDef, bool) {
	for _, d := range reportDefs {
		if d.key == key {
			return d, true
		}
	}
	return reportDef{}, false
}

// UI renders the serve-mode web pages. It holds the effective configuration (for
// the Config view) and the build version (for the sidebar). The config is read
// only for rendering and is never mutated. When an Auth is attached (issue #204,
// lane 3) the UI routes are gated by RequireSession and the /login + /logout
// endpoints are registered; without it the routes are public (the #210 default).
type UI struct {
	cfg        config.Config
	version    string
	auth       *Auth
	onboarding *Onboarding
	// reports is the read-only report query repo backing the Reports workspace.
	// It is nil when the UI is built without a database seam (e.g. some tests);
	// the report-fragment handler degrades to a 503 rather than panicking.
	reports *reports.Repo
}

// UIOption customizes a UI.
type UIOption func(*UI)

// WithAuth attaches the session-auth subsystem: it gates the UI routes behind
// RequireSession and registers the login/logout endpoints. Omitting it leaves
// the routes public (the #210 behavior).
func WithAuth(a *Auth) UIOption {
	return func(u *UI) { u.auth = a }
}

// WithOnboarding attaches the first-run onboarding flow (issue #204, lane 4): it
// registers the /setup endpoints and redirects the UI routes to /setup until an
// admin exists. It is meaningful only alongside WithAuth (onboarding feeds the
// login session); without auth it is ignored.
func WithOnboarding(o *Onboarding) UIOption {
	return func(u *UI) { u.onboarding = o }
}

// AttachOnboarding wires the onboarding flow onto an already-constructed UI. It
// is the post-construction equivalent of WithOnboarding, used by the server
// layer where the UI is built first (WithWebUIAuth) and onboarding attached
// after.
func (u *UI) AttachOnboarding(o *Onboarding) { u.onboarding = o }

// WithReports attaches the read-only reports repo that backs the Reports
// workspace. Omitting it leaves the workspace shell reachable but the
// report-fragment routes degrade to a 503 (no data source wired).
func WithReports(repo *reports.Repo) UIOption {
	return func(u *UI) { u.reports = repo }
}

// AttachReports wires the reports repo onto an already-constructed UI. It is the
// post-construction equivalent of WithReports, used by the server layer where
// the UI is built first (WithWebUIAuth) and the reports repo attached after.
func (u *UI) AttachReports(repo *reports.Repo) { u.reports = repo }

// NewUI builds the web UI renderer from the effective config and build version.
func NewUI(cfg config.Config, version string, opts ...UIOption) *UI {
	u := &UI{cfg: cfg, version: version}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// Register wires the web UI routes onto mux: the static asset handler, a root
// redirect to /config, and the Reports and Config pages. Routes are GET-only;
// the JSON API and its method patterns are registered separately by the server.
// When an Auth is attached the page routes are wrapped in RequireSession and the
// /login (GET/POST) and /logout (POST) endpoints are registered; the static
// assets and the login endpoints themselves stay public.
func (u *UI) Register(mux *http.ServeMux) {
	mux.Handle("GET "+staticPrefix, StaticHandler())
	if u.auth != nil {
		mux.HandleFunc("GET /login", u.auth.handleLoginForm)
		mux.HandleFunc("POST /login", u.auth.handleLogin)
		mux.HandleFunc("POST /logout", u.auth.handleLogout)
		if u.onboarding != nil {
			mux.HandleFunc("GET /setup", u.onboarding.handleSetupForm)
			mux.HandleFunc("POST /setup", u.onboarding.handleSetup)
		}
		// The page guard is RequireSession, wrapped (when onboarding is present)
		// by FirstRunGate so an un-onboarded daemon redirects to /setup before the
		// session check runs.
		guard := func(h http.Handler) http.Handler {
			sess := u.auth.RequireSession(h)
			if u.onboarding != nil {
				return u.onboarding.FirstRunGate(sess)
			}
			return sess
		}
		mux.Handle("GET /{$}", guard(http.HandlerFunc(u.handleRoot)))
		mux.Handle("GET /reports", guard(http.HandlerFunc(u.handleReports)))
		mux.Handle("GET /reports/{key}", guard(http.HandlerFunc(u.handleReportFragment)))
		mux.Handle("GET /config", guard(http.HandlerFunc(u.handleConfig)))
		return
	}
	mux.HandleFunc("GET /{$}", u.handleRoot)
	mux.HandleFunc("GET /reports", u.handleReports)
	mux.HandleFunc("GET /reports/{key}", u.handleReportFragment)
	mux.HandleFunc("GET /config", u.handleConfig)
}

// handleRoot redirects the bare root to the Config view, the default landing
// page for v1 (Reports is still a placeholder).
func (u *UI) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/config", http.StatusFound)
}

// handleReports renders the Reports workspace shell with no report selected. No
// query runs on this path: the default pane is a placeholder prompting the
// operator to pick a report, keeping execution strictly user-initiated.
func (u *UI) handleReports(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render(w, r, templates.ReportsPage(u.version, u.buildRail(""), nil))
}

// handleReportFragment runs one canned report on demand and returns its results.
// For an htmx request it returns the report-view fragment plus an out-of-band
// rail re-render (so the selection highlight and last-run timestamps update in
// place); for a plain navigation (no JS) it returns the full workspace page with
// the report selected, so each rail URL is a real, bookmarkable destination.
func (u *UI) handleReportFragment(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	def, ok := lookupReportDef(key)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Reports expose operational detail; never let a browser or proxy cache a run.
	w.Header().Set("Cache-Control", "no-store")
	if u.reports == nil {
		// No data source wired: fail loudly with a 503 rather than rendering an
		// empty table that would read as "no data".
		slog.Error("reports repo not wired; cannot serve report", "report", key)
		http.Error(w, "reports data source unavailable", http.StatusServiceUnavailable)
		return
	}

	view, err := u.buildReportView(r.Context(), def)
	if err != nil {
		slog.Error("report query failed", "report", key, "error", err)
		http.Error(w, "report query failed", http.StatusInternalServerError)
		return
	}

	view.LastRun = time.Now().Format(reportTimeFormat)
	rail := u.buildRail(key)

	if r.Header.Get("HX-Request") == "true" {
		render(w, r, templates.ReportFragment(rail, view))
		return
	}
	render(w, r, templates.ReportsPage(u.version, rail, &view))
}

// reportPath builds the sidebar link target for a report key, encoding the key
// as a single path segment so a key containing reserved characters cannot break
// the URL. It is a no-op for the kebab-case canned-report keys.
func reportPath(key string) string {
	return "/reports/" + url.PathEscape(key)
}

// buildRail builds the sidebar report-nav view models in design-doc order,
// marking activeKey selected.
func (u *UI) buildRail(activeKey string) []templates.RailItem {
	rail := make([]templates.RailItem, 0, len(reportDefs))
	for _, d := range reportDefs {
		rail = append(rail, templates.RailItem{
			Key:    d.key,
			Path:   reportPath(d.key),
			Title:  d.title,
			Active: d.key == activeKey,
		})
	}
	return rail
}

// buildReportView runs the report identified by def and maps its read-only
// results onto the presentation view. LastRun is stamped by the caller.
func (u *UI) buildReportView(ctx context.Context, def reportDef) (templates.ReportView, error) {
	v := templates.ReportView{Key: def.key, Title: def.title, Subtitle: def.subtitle}
	switch def.key {
	case "queue-summary":
		s, err := u.reports.QueueSummary(ctx)
		if err != nil {
			return templates.ReportView{}, err
		}
		v.QueueRows = []templates.QueueSummaryRow{
			{Status: "Pending", Count: strconv.FormatInt(s.Pending, 10)},
			{Status: "Processing", Count: strconv.FormatInt(s.Processing, 10)},
			{Status: "Done", Count: strconv.FormatInt(s.Done, 10)},
			{Status: "Failed", Count: strconv.FormatInt(s.Failed, 10)},
			{Status: "Deferred", Count: strconv.FormatInt(s.Deferred, 10)},
			{Status: "Total", Count: strconv.FormatInt(s.Total, 10), IsTotal: true},
		}
	case "recent-outcomes":
		rows, err := u.reports.RecentOutcomes(ctx, recentOutcomesLimit)
		if err != nil {
			return templates.ReportView{}, err
		}
		v.RecentRows = make([]templates.RecentOutcomeRow, 0, len(rows))
		for _, o := range rows {
			v.RecentRows = append(v.RecentRows, templates.RecentOutcomeRow{
				Artist:      o.Artist,
				Title:       o.Title,
				Album:       o.Album,
				Result:      string(o.Result),
				Lane:        o.ProviderLane,
				CompletedAt: formatReportTime(o.CompletedAt),
			})
		}
	case "provider-effectiveness":
		rows, err := u.reports.ProviderEffectiveness(ctx)
		if err != nil {
			return templates.ReportView{}, err
		}
		v.ProviderRows = make([]templates.ProviderRow, 0, len(rows))
		for _, p := range rows {
			v.ProviderRows = append(v.ProviderRows, templates.ProviderRow{
				Lane:    p.Lane,
				Hits:    strconv.FormatInt(p.Hits, 10),
				Misses:  strconv.FormatInt(p.Misses, 10),
				HitRate: fmt.Sprintf("%.1f%%", p.HitRate*100),
			})
		}
	case "instrumental-inventory":
		rows, err := u.reports.InstrumentalInventory(ctx)
		if err != nil {
			return templates.ReportView{}, err
		}
		v.InstrumentalRows = make([]templates.InstrumentalRow, 0, len(rows))
		for _, t := range rows {
			v.InstrumentalRows = append(v.InstrumentalRows, templates.InstrumentalRow{
				ID:              strconv.FormatInt(t.WorkQueueID, 10),
				Artist:          t.Artist,
				Title:           t.Title,
				File:            t.FilePath,
				DetectRequested: detectRequestedLabel(t.DetectRequested),
			})
		}
	case "failure-analysis":
		rows, err := u.reports.FailureAnalysis(ctx)
		if err != nil {
			return templates.ReportView{}, err
		}
		v.FailureRows = make([]templates.FailureRow, 0, len(rows))
		for _, g := range rows {
			v.FailureRows = append(v.FailureRows, templates.FailureRow{
				Status: g.Status,
				Reason: g.Reason,
				Count:  strconv.FormatInt(g.Count, 10),
			})
		}
	default:
		// Unreachable in practice: def.key is validated upstream in
		// handleReportFragment via lookupReportDef. Fail fast if a new
		// reportDef is ever added without a matching case here, rather
		// than silently rendering an empty report.
		return templates.ReportView{}, fmt.Errorf("unimplemented report: %s", def.key)
	}
	return v, nil
}

// formatReportTime renders a timestamp, or an em dash for the zero value (a NULL
// completed_at), so an empty cell reads as "no timestamp" rather than a bogus
// epoch.
func formatReportTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(reportTimeFormat)
}

// detectRequestedLabel maps the per-item detect_instrumental request flag to a
// human label. NULL (not Valid) means no per-item decision was stamped, so the
// worker used the global config default.
func detectRequestedLabel(n sql.NullInt64) string {
	if !n.Valid {
		return "config default"
	}
	if n.Int64 == 1 {
		return "requested"
	}
	return "not requested"
}

// handleConfig renders the effective configuration with secrets redacted.
// FormatConfigText is the single redaction source of truth (shared with the
// logging layer), so api.token and server.webhook_api_keys are masked before
// the text reaches the template. Source-hint maps are nil: the view shows the
// merged effective values, not per-field provenance.
func (u *UI) handleConfig(w http.ResponseWriter, r *http.Request) {
	toml := config.FormatConfigText(u.cfg, nil, nil)
	// Even with secrets redacted, the effective config exposes operational
	// detail (paths, intervals, provider lanes); keep it out of browser and
	// intermediary caches.
	w.Header().Set("Cache-Control", "no-store")
	render(w, r, templates.ConfigPage(u.version, toml, u.buildRail("")))
}

// render writes a templ component as the HTML response with an implicit 200.
func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	renderWithStatus(w, r, http.StatusOK, c)
}

// renderWithStatus writes a templ component as the HTML response with the given
// status code. It renders into a buffer first so that a render failure can
// return a clean 500 rather than a partial body under a committed 200; the
// status line is not written until the buffer is ready.
func renderWithStatus(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		slog.Error("web UI render failed", "path", r.URL.Path, "error", err) //nolint:gosec // G706 false positive: r.URL.Path logged as a structured slog key-value, not a format string / execution sink
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
