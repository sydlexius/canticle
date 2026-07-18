package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/secrets"
	"github.com/sydlexius/canticle/web/templates"
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

	// --- settings write path (#288 Phase 2) ---
	// configPath is the RESOLVED config file path the save handlers write through
	// config.ApplyChanges. Empty disables the editable write path (the settings
	// page stays read-only). Threaded from runServe via WithConfigPath.
	configPath string
	// secretStore routes secret-field saves (api.token) into the encrypted store
	// instead of the TOML, so a secret never lands in config.toml or its .bak
	// (#290). Nil disables secret saves (they are rejected, not written to TOML).
	secretStore secrets.Store
	// saveMu serializes the read-modify-write config saves so concurrent POSTs
	// cannot interleave ApplyChanges' load/modify/atomic-rename cycle (#290
	// single-writer guard).
	saveMu sync.Mutex

	// keys is the managed (DB-backed) webhook API key store the key-management
	// page (#300) lists, creates, and revokes against. Nil when no key seam is
	// wired (e.g. some tests, or the web UI built without serve wiring), in which
	// case the page renders an "unavailable" notice and the create/revoke POSTs
	// return 503 rather than panicking.
	keys KeyManager

	// musixmatchInactive is set when serve started without a usable Musixmatch
	// token (#385): the shell renders a notice banner explaining lyrics fetching
	// is disabled and linking to Settings to add a token. False (the default)
	// renders nothing.
	musixmatchInactive bool
}

// KeyManager is the subset of *auth.Service the webhook key management page
// (#300) needs: list existing keys (metadata only), create a key (returning the
// one-time raw material), and revoke a key by its public ID (the raw key is not
// recoverable after creation, so revocation is by ID). *auth.Service satisfies
// it.
type KeyManager interface {
	ListKeys(ctx context.Context) ([]auth.Key, error)
	CreateKey(ctx context.Context, name string, scopes []auth.Scope) (auth.CreatedKey, error)
	RevokeKeyByID(ctx context.Context, id string) (auth.Key, error)
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

// WithKeyManager wires the managed webhook API key store the key-management page
// (#300) operates on. Omitting it leaves the page reachable but renders an
// "unavailable" notice (no key backend wired); the create/revoke POSTs return
// 503.
func WithKeyManager(km KeyManager) UIOption {
	return func(u *UI) { u.keys = km }
}

// AttachKeyManager wires the key store onto an already-constructed UI, the
// post-construction equivalent of WithKeyManager used by the server layer where
// the UI is built first (WithWebUIAuth) and the seam attached after.
func (u *UI) AttachKeyManager(km KeyManager) { u.keys = km }

// WithMusixmatchInactive marks the Musixmatch provider as token-less so the
// shell renders the lyrics-disabled notice banner (#385). Threaded from runServe
// via server.WithMusixmatchInactive. Default (false) renders no banner.
func WithMusixmatchInactive(inactive bool) UIOption {
	return func(u *UI) { u.musixmatchInactive = inactive }
}

// AttachMusixmatchInactive sets the banner flag on an already-constructed UI,
// the post-construction equivalent of WithMusixmatchInactive used by the server
// layer (WithWebUIAuth builds the UI first).
func (u *UI) AttachMusixmatchInactive(inactive bool) { u.musixmatchInactive = inactive }

// WithConfigPath sets the resolved config file path the settings save handlers
// write through. Without it the settings page stays read-only (no write path).
func WithConfigPath(path string) UIOption {
	return func(u *UI) { u.configPath = path }
}

// WithSecretStore wires the encrypted secret store used to persist secret-field
// saves (the Musixmatch token) off the TOML. Without it secret saves are
// rejected rather than written to the config file.
func WithSecretStore(s secrets.Store) UIOption {
	return func(u *UI) { u.secretStore = s }
}

// AttachSettingsWriter wires the settings write path (config file + secret store)
// onto an already-constructed UI, the post-construction equivalent of
// WithConfigPath + WithSecretStore used by the server layer (WithWebUIAuth builds
// the UI first). A nil store leaves secret saves rejected.
func (u *UI) AttachSettingsWriter(configPath string, store secrets.Store) {
	u.configPath = configPath
	u.secretStore = store
}

// secureRequest reports whether the effective connection is TLS, for the CSRF
// cookie's Secure attribute. It defers to the auth subsystem's proxy-aware check
// when present; otherwise it reads the direct TLS state.
func (u *UI) secureRequest(r *http.Request) bool {
	if u.auth != nil {
		return u.auth.secureRequest(r)
	}
	return r.TLS != nil
}

// secretPresentSentinel is a non-empty placeholder set on a re-loaded config's
// secret fields when the secret exists only in the encrypted store (not the
// file or env), which a file reload cannot see. It marks the field "set" for the
// display without exposing a value: effectiveValue never echoes a secret and
// FormatConfigText redacts it, so the sentinel is never rendered.
const secretPresentSentinel = "\x00stored\x00"

// currentConfig returns the config to render the settings view from: the CURRENT
// on-disk file, re-loaded and env-resolved the same way startup does, so a value
// just saved through the write path is reflected on reload (#288 Phase 2). It
// falls back to the frozen startup snapshot when the write path is not wired or
// a reload fails (logged). Secret presence from the store is folded in so a
// store-only secret still reads "(set)".
func (u *UI) currentConfig(ctx context.Context) config.Config {
	if u.configPath == "" {
		return u.cfg
	}
	cfg, _, err := config.LoadWithSources(u.configPath)
	if err != nil {
		slog.Error("settings: reload config for display failed; showing startup snapshot", "error", err)
		return u.cfg
	}
	if u.secretStore != nil {
		if cfg.API.Token == "" {
			if _, ok, err := u.secretStore.Get(ctx, secrets.NameMusixmatchToken); err != nil {
				slog.Warn("settings: secret-store read failed; secret presence unknown for display", "key", secrets.NameMusixmatchToken, "error", err)
			} else if ok {
				cfg.API.Token = secretPresentSentinel
			}
		}
		if len(cfg.Server.WebhookAPIKeys) == 0 {
			if v, ok, err := u.secretStore.Get(ctx, secrets.NameWebhookAPIKey); err != nil {
				slog.Warn("settings: secret-store read failed; secret presence unknown for display", "key", secrets.NameWebhookAPIKey, "error", err)
			} else if ok && v != "" {
				cfg.Server.WebhookAPIKeys = []string{secretPresentSentinel}
			}
		}
	}
	return cfg
}

// NewUI builds the web UI renderer from the effective config and build version.
func NewUI(cfg config.Config, version string, opts ...UIOption) *UI {
	u := &UI{cfg: cfg, version: version}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// Register wires the web UI routes onto mux: the static asset handler, a root
// redirect to /settings, the Reports pages, and the Settings page (with /config
// kept as a redirect to /settings for old links). Routes are GET-only;
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
		mux.Handle("GET /dashboard", guard(http.HandlerFunc(u.handleDashboard)))
		mux.Handle("GET /reports", guard(http.HandlerFunc(u.handleReports)))
		mux.Handle("GET /reports/{key}", guard(http.HandlerFunc(u.handleReportFragment)))
		mux.Handle("GET /config", guard(http.HandlerFunc(u.handleConfig)))
		mux.Handle("GET /settings", guard(http.HandlerFunc(u.handleSettings)))
		mux.Handle("POST /settings/field", guard(http.HandlerFunc(u.handleSaveField)))
		mux.Handle("POST /settings/section", guard(http.HandlerFunc(u.handleSaveSection)))
		mux.Handle("GET /settings/keys", guard(http.HandlerFunc(u.handleWebhookKeys)))
		mux.Handle("POST /settings/keys", guard(http.HandlerFunc(u.handleCreateWebhookKey)))
		mux.Handle("POST /settings/keys/revoke", guard(http.HandlerFunc(u.handleRevokeWebhookKey)))
		return
	}
	mux.HandleFunc("GET /{$}", u.handleRoot)
	mux.HandleFunc("GET /dashboard", u.handleDashboard)
	mux.HandleFunc("GET /reports", u.handleReports)
	mux.HandleFunc("GET /reports/{key}", u.handleReportFragment)
	mux.HandleFunc("GET /config", u.handleConfig)
	mux.HandleFunc("GET /settings", u.handleSettings)
	mux.HandleFunc("POST /settings/field", u.handleSaveField)
	mux.HandleFunc("POST /settings/section", u.handleSaveSection)
	mux.HandleFunc("GET /settings/keys", u.handleWebhookKeys)
	mux.HandleFunc("POST /settings/keys", u.handleCreateWebhookKey)
	mux.HandleFunc("POST /settings/keys/revoke", u.handleRevokeWebhookKey)
}

// settingsPath is the single config destination. Settings replaced the old
// read-only Config page (#288); /config is kept only as a redirect so old links
// and bookmarks still resolve.
const settingsPath = "/settings"

// dashboardPath is the default landing page after authentication.
const dashboardPath = "/dashboard"

// handleRoot redirects the bare root to the Dashboard, the default landing page.
func (u *UI) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, dashboardPath, http.StatusFound)
}

// handleReports renders the Reports workspace shell with no report selected. No
// query runs on this path: the default pane is a placeholder prompting the
// operator to pick a report, keeping execution strictly user-initiated.
func (u *UI) handleReports(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render(w, r, templates.ReportsPage(u.version, u.buildRail(""), nil, u.musixmatchInactive))
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
	render(w, r, templates.ReportsPage(u.version, rail, &view, u.musixmatchInactive))
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

	// Resolve the server display timezone for completed-at timestamps,
	// mirroring buildDashboardView. If TZ env is set and valid, format
	// server-side in that zone; otherwise leave timestamps in the zone they
	// carry (UTC, as stored).
	var serverLoc *time.Location
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			serverLoc = loc
		}
	}

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
				CompletedAt: formatReportTime(o.CompletedAt, serverLoc),
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

// formatReportTime renders a timestamp, or a hyphen for the zero value (a NULL
// completed_at), so an empty cell reads as "no timestamp" rather than a bogus
// epoch. With a non-nil loc the timestamp is shown in that zone; with a nil loc
// it is normalized to UTC (symmetric with formatDashboardTime), so a timestamp
// that carries a non-UTC zone still renders with a UTC label.
func formatReportTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "-"
	}
	if loc != nil {
		return t.In(loc).Format(reportTimeFormat)
	}
	return t.UTC().Format(reportTimeFormat)
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

// handleConfig redirects the retired Config page to Settings, which absorbed the
// read-only config view as its Raw config tab (#288). The route is kept so old
// links and bookmarks still resolve.
func (u *UI) handleConfig(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, settingsPath, http.StatusMovedPermanently)
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
