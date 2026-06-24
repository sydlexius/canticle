package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
	"github.com/sydlexius/mxlrcgo-svc/internal/pathutil"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
	"github.com/sydlexius/mxlrcgo-svc/internal/web"
)

const maxWebhookBody = 1 << 20 // 1 MiB

// Authenticator validates API keys for HTTP endpoints.
type Authenticator interface {
	ValidateKey(ctx context.Context, raw string, required auth.Scope) (auth.Key, error)
}

// WorkQueue enqueues lyrics work from webhooks.
type WorkQueue interface {
	Enqueue(ctx context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error)
	Cleanup(ctx context.Context, inputs models.Inputs) (int64, error)
}

// Readiness reports whether backing dependencies (e.g. the database) are
// reachable. *sql.DB satisfies this interface via PingContext.
type Readiness interface {
	PingContext(ctx context.Context) error
}

// StatusReporter returns queue depth grouped by status for the status endpoint.
type StatusReporter interface {
	CountByStatus(ctx context.Context) (map[string]int64, error)
}

// Inventory resolves webhook track metadata against persisted scan results so
// webhooks can reuse the container-visible file paths discovered by scans.
type Inventory interface {
	FindByTrack(ctx context.Context, artist, title string) ([]models.ScanResult, error)
}

// defaultPathChecker reports whether path is usable inside the running
// container. A nil error means the path exists as a regular file and can be
// targeted directly. Directories are rejected because a .lrc target is derived
// from a file path; a directory would produce an invalid synthetic destination.
// Callers confine path to a configured library root before calling this (see
// Handler.confinedPayloadPath), so it only ever stats operator-trusted roots.
func defaultPathChecker(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("path %q is a directory, not a file", path)
	}
	return nil
}

// Handler serves the HTTP API.
type Handler struct {
	auth               Authenticator
	queue              WorkQueue
	outdir             string
	priority           int
	ready              Readiness
	stats              StatusReporter
	metrics            MetricsReporter
	inventory          Inventory
	allowedRoots       []string
	pathChecker        func(string) error
	webui              *web.UI
	onboarding         *web.Onboarding
	reportsDB          *sql.DB
	settingsConfigPath string
	settingsStore      secrets.Store
	keyManager         web.KeyManager
	musixmatchInactive bool
	trusted            *trustnet.Policy
	mux                *http.ServeMux
}

// Option configures optional Handler dependencies.
type Option func(*Handler)

// WithReadiness wires a readiness checker used by GET /readyz.
func WithReadiness(r Readiness) Option {
	return func(h *Handler) { h.ready = r }
}

// WithStatusReporter wires a queue summary source used by GET /api/v1/status.
func WithStatusReporter(s StatusReporter) Option {
	return func(h *Handler) { h.stats = s }
}

// WithMetricsReporter wires a metrics source used by GET /metrics.
func WithMetricsReporter(m MetricsReporter) Option {
	return func(h *Handler) { h.metrics = m }
}

// WithInventory wires the scan-result inventory used to resolve webhook events
// to container-visible file paths.
func WithInventory(inv Inventory) Option {
	return func(h *Handler) { h.inventory = inv }
}

// WithAllowedRoots confines raw webhook-provided payload paths to the configured
// library roots. A trackFiles path is only used directly when it resolves
// (lexically and through symlinks) to a location inside one of these roots;
// anything else falls back to metadata. The roots are the operator-declared
// source of truth, so this prevents an authenticated webhook from directing a
// lyric write to an arbitrary location (path injection). With no roots
// configured, raw payload paths are never trusted and resolution always falls
// back to inventory or metadata. Roots are snapshotted at handler construction.
func WithAllowedRoots(roots []string) Option {
	return func(h *Handler) {
		cleaned := make([]string, 0, len(roots))
		for _, r := range roots {
			if r = strings.TrimSpace(r); r != "" {
				cleaned = append(cleaned, filepath.Clean(r))
			}
		}
		h.allowedRoots = cleaned
	}
}

// WithPathChecker overrides how the handler tests whether a webhook-provided
// path is usable inside the container. Used in tests; production uses os.Stat.
func WithPathChecker(check func(string) error) Option {
	return func(h *Handler) { h.pathChecker = check }
}

// WithWebUI mounts the serve-mode web UI (sidebar shell, Reports placeholder,
// and the read-only Config view) onto the handler. cfg is rendered (redacted)
// by the Config view and version labels the sidebar. Omitting this option
// leaves the handler serving only the JSON API.
func WithWebUI(cfg config.Config, version string) Option {
	return func(h *Handler) { h.webui = web.NewUI(cfg, version) }
}

// WithTrustedNetworks wires the trusted-network policy that gates GET /metrics
// (issue #204, S3): only a request whose resolved client IP is loopback or
// within a configured CIDR may scrape. A nil policy is ignored, leaving the
// default-closed (loopback-only) policy installed by NewHandler.
func WithTrustedNetworks(p *trustnet.Policy) Option {
	return func(h *Handler) {
		if p != nil {
			h.trusted = p
		}
	}
}

// WithWebUIAuth mounts the serve-mode web UI with session authentication: the
// page routes are gated by RequireSession and the /login + /logout endpoints are
// registered (issue #204, lane 3). auth supplies credential/session validation,
// the trusted-network bypass, and cookie management. A nil auth degrades to an
// unauthenticated UI (equivalent to WithWebUI), so callers fail safe to the
// existing behavior rather than panicking.
func WithWebUIAuth(cfg config.Config, version string, auth *web.Auth) Option {
	return func(h *Handler) {
		if auth == nil {
			h.webui = web.NewUI(cfg, version)
			return
		}
		h.webui = web.NewUI(cfg, version, web.WithAuth(auth))
	}
}

// WithOnboarding attaches the first-run onboarding flow (issue #204, lane 4) to
// the authenticated web UI: the /setup endpoints and the redirect of the UI
// routes to /setup until an admin exists. It is meaningful only alongside
// WithWebUIAuth (the UI must be mounted to attach onboarding); a nil onboarding,
// or the absence of a mounted UI (web UI disabled), is a no-op. This is correct,
// not a silent failure: with no web UI there is nowhere to onboard.
func WithOnboarding(o *web.Onboarding) Option {
	return func(h *Handler) { h.onboarding = o }
}

// WithReportsDB wires the database that backs the serve-mode Reports workspace.
// The handler builds a read-only reports.Repo from db and attaches it to the
// mounted web UI (see NewHandler). It is meaningful only alongside a mounted web
// UI; with no UI, or a nil db, it is a no-op (the reports routes never mount).
func WithReportsDB(db *sql.DB) Option {
	return func(h *Handler) { h.reportsDB = db }
}

// WithSettingsWriter enables the settings page write path (#288 Phase 2): the
// resolved config file path the save handlers write through config.ApplyChanges,
// and the encrypted secret store that absorbs secret-field saves (the Musixmatch
// token) off the TOML. Meaningful only alongside a mounted web UI; with no UI it
// is a no-op. An empty path leaves the page read-only.
func WithSettingsWriter(configPath string, store secrets.Store) Option {
	return func(h *Handler) {
		h.settingsConfigPath = configPath
		h.settingsStore = store
	}
}

// WithKeyManagerUI wires the managed webhook API key store that backs the key
// management page (#300). The handler attaches it to the mounted web UI (see
// NewHandler). Meaningful only alongside a mounted web UI; with no UI, or a nil
// manager, it is a no-op (the page renders an unavailable notice).
func WithKeyManagerUI(km web.KeyManager) Option {
	return func(h *Handler) { h.keyManager = km }
}

// WithMusixmatchInactive marks the Musixmatch provider as token-less (#385) so
// the mounted web UI renders the lyrics-disabled notice banner on every shell
// page. Threaded from runServe. Meaningful only alongside a mounted web UI; with
// no UI it is a no-op.
func WithMusixmatchInactive(inactive bool) Option {
	return func(h *Handler) { h.musixmatchInactive = inactive }
}

// WithWebUIIf conditionally mounts the web UI. When enabled is false it
// returns a no-op option so callers do not need an inline if-branch.
func WithWebUIIf(enabled bool, cfg config.Config, version string) Option {
	if !enabled {
		return func(*Handler) {}
	}
	return WithWebUI(cfg, version)
}

// NewHandler creates an HTTP API handler.
func NewHandler(a Authenticator, q WorkQueue, outdir string, opts ...Option) *Handler {
	h := &Handler{
		auth:        a,
		queue:       q,
		outdir:      outdir,
		priority:    queue.PriorityWebhook,
		pathChecker: defaultPathChecker,
		trusted:     trustnet.LoopbackOnly(),
		mux:         http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(h)
	}
	h.mux.HandleFunc("POST /api/v1/webhooks/lidarr", h.handleLidarr)
	h.mux.HandleFunc("GET /healthz", h.handleHealthz)
	h.mux.HandleFunc("GET /readyz", h.handleReadyz)
	h.mux.HandleFunc("GET /api/v1/status", h.handleStatus)
	h.mux.HandleFunc("GET /metrics", h.handleMetrics)
	if h.webui != nil {
		if h.onboarding != nil {
			h.webui.AttachOnboarding(h.onboarding)
		}
		if h.reportsDB != nil {
			h.webui.AttachReports(reports.New(h.reportsDB))
		}
		if h.settingsConfigPath != "" {
			h.webui.AttachSettingsWriter(h.settingsConfigPath, h.settingsStore)
		}
		if h.keyManager != nil {
			h.webui.AttachKeyManager(h.keyManager)
		}
		h.webui.AttachMusixmatchInactive(h.musixmatchInactive)
		h.webui.Register(h.mux)
	}
	return h
}

// writeJSON serializes v as JSON with the given status code. Encoding failures
// are logged rather than surfaced because the status line is already written.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to encode JSON response", "error", err)
	}
}

// handleHealthz reports process liveness. It performs no dependency checks so a
// 200 means only that the HTTP server is accepting requests.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports readiness by checking database reachability. When no
// readiness checker is configured the endpoint still reports ready, but omits
// the database check from the response rather than claiming a check that never
// ran. Error detail is intentionally omitted to avoid leaking filesystem paths
// or connection strings.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if h.ready == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		return
	}
	if err := h.ready.PingContext(r.Context()); err != nil {
		slog.Warn("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"checks": map[string]string{"database": "error"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"checks": map[string]string{"database": "ok"},
	})
}

// handleStatus returns a queue summary. It requires an admin-scoped API key so
// operational detail is not exposed to unauthenticated callers, and never
// includes tokens, webhook keys, or filesystem paths.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, "auth unavailable", http.StatusInternalServerError)
		return
	}
	if _, err := h.auth.ValidateKey(r.Context(), apiKey(r), auth.ScopeAdmin); err != nil {
		switch {
		case errors.Is(err, auth.ErrForbiddenScope):
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevokedKey):
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		default:
			slog.Error("status authentication failed", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	resp := map[string]any{"status": "ok"}
	if h.stats != nil {
		counts, err := h.stats.CountByStatus(r.Context())
		if err != nil {
			slog.Error("status queue summary failed", "error", err)
			http.Error(w, "status unavailable", http.StatusInternalServerError)
			return
		}
		resp["queue"] = counts
	}
	writeJSON(w, http.StatusOK, resp)
}

// contentSecurityPolicy is the single authoritative CSP applied to every
// serve-mode response. 'unsafe-inline' in script-src is required by the
// dashboard's inline timezone-rewrite <script> IIFE (web/templates/dashboard.templ),
// which runs before paint to localize timestamps; a stricter script-src would
// break that page. Future hardening: replace 'unsafe-inline' with a per-request
// nonce or a precomputed hash for that one inline block. There are no inline
// styles, so style-src stays at 'self'.
const contentSecurityPolicy = "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'"

// setSecurityHeaders applies a conservative baseline of security headers to
// every serve-mode response (static assets and pages alike). It must be called
// before any handler writes a status line or body, otherwise the headers are
// dropped.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

// ServeHTTP logs requests and dispatches them to API routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set security headers before dispatch so they are present on every
	// response, including ones whose handlers write status/body immediately.
	// statusRecorder embeds w and does not override Header(), so setting on w
	// is equivalent to setting on rec.
	setSecurityHeaders(w)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.mux.ServeHTTP(rec, r)
	slog.Debug("http request", "method", r.Method, "uri", redactURI(r.URL), "status", rec.status) //nolint:gosec // G706: request URI is logged as a structured slog field after apikey redaction; slog escapes values
}

func (h *Handler) handleLidarr(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, "auth unavailable", http.StatusInternalServerError)
		return
	}
	if h.queue == nil {
		http.Error(w, "queue unavailable", http.StatusInternalServerError)
		return
	}
	if _, err := h.auth.ValidateKey(r.Context(), apiKey(r), auth.ScopeWebhook); err != nil {
		switch {
		case errors.Is(err, auth.ErrForbiddenScope):
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevokedKey):
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		default:
			slog.Error("lidarr webhook authentication failed", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	var payload lidarrWebhook
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err := dec.Decode(&payload); err != nil {
		http.Error(w, "invalid lidarr webhook payload", http.StatusBadRequest)
		return
	}

	event := strings.TrimSpace(payload.EventType)
	switch event {
	case "Download", "TrackRetag":
		inputs, err := h.resolveInputs(r.Context(), payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, v := range inputs {
			if _, err := h.queue.Enqueue(r.Context(), v, h.priority); err != nil {
				slog.Error("failed to enqueue work from lidarr webhook", "event", event, "error", err, "input", v)
				http.Error(w, "enqueue failed", http.StatusInternalServerError)
				return
			}
		}
		slog.Info("lidarr webhook enqueued", "event", event, "count", len(inputs))
	case "Grab":
		slog.Info("lidarr grab received", "artist", payload.Artist.ArtistName, "album", payload.Album.Title)
	case "Rename":
		slog.Info("lidarr rename received", "artist", payload.Artist.ArtistName, "album", payload.Album.Title)
	case "Delete":
		inputs, err := h.metadataInputs(payload)
		if err != nil {
			slog.Warn("lidarr delete received without cleanup target", "artist", payload.Artist.ArtistName, "album", payload.Album.Title)
			break
		}
		var removed int64
		for _, v := range inputs {
			n, err := h.queue.Cleanup(r.Context(), v)
			if err != nil {
				slog.Error("failed to clean queued work from lidarr webhook", "event", event, "error", err, "input", v)
				http.Error(w, "cleanup failed", http.StatusInternalServerError)
				return
			}
			removed += n
		}
		slog.Info("lidarr delete cleanup completed", "artist", payload.Artist.ArtistName, "album", payload.Album.Title, "removed", removed)
	default:
		http.Error(w, "unsupported lidarr event", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// webhookTracks validates the payload and returns the artist, album, and the
// list of track titles to process.
func (h *Handler) webhookTracks(payload lidarrWebhook) (artist, album string, titles []string, err error) {
	artist = strings.TrimSpace(payload.Artist.ArtistName)
	if artist == "" {
		return "", "", nil, fmt.Errorf("missing artist")
	}
	album = strings.TrimSpace(payload.Album.Title)
	tracks := payload.Tracks
	if len(tracks) == 0 && payload.Track.Title != "" {
		tracks = []lidarrTrack{payload.Track}
	}
	if len(tracks) == 0 {
		return "", "", nil, fmt.Errorf("missing tracks")
	}
	titles = make([]string, 0, len(tracks))
	for _, track := range tracks {
		title := strings.TrimSpace(track.Title)
		if title == "" {
			return "", "", nil, fmt.Errorf("missing track title")
		}
		titles = append(titles, title)
	}
	return artist, album, titles, nil
}

// metadataInputs builds queue inputs from webhook metadata only, writing to the
// configured output directory. This is the resolution fallback and the form
// used for cleanup, which matches queue rows by normalized artist/title.
func (h *Handler) metadataInputs(payload lidarrWebhook) ([]models.Inputs, error) {
	artist, album, titles, err := h.webhookTracks(payload)
	if err != nil {
		return nil, err
	}
	inputs := make([]models.Inputs, 0, len(titles))
	for _, title := range titles {
		inputs = append(inputs, h.metadataInput(artist, title, album))
	}
	return inputs, nil
}

func (h *Handler) metadataInput(artist, title, album string) models.Inputs {
	return models.Inputs{
		Track: models.Track{
			ArtistName: artist,
			TrackName:  title,
			AlbumName:  album,
		},
		Outdir: h.outdir,
		OutputPaths: []models.OutputPath{{
			Outdir: h.outdir,
		}},
	}
}

// resolveInputs builds queue inputs for ingestion events, resolving each track
// through the scanned library inventory first, then a directly usable payload
// path, then metadata. The configured library scans are the source of truth for
// container-visible filesystem paths, so inventory matches reuse the exact
// Outdir, Filename, and OutputPaths recorded by the scheduler, with SourcePath
// taken from the scan result's FilePath via scan.ResultInputs.
func (h *Handler) resolveInputs(ctx context.Context, payload lidarrWebhook) ([]models.Inputs, error) {
	artist, album, titles, err := h.webhookTracks(payload)
	if err != nil {
		return nil, err
	}
	paths := payload.payloadPaths()
	single := len(titles) == 1
	inputs := make([]models.Inputs, 0, len(titles))
	for _, title := range titles {
		inputs = append(inputs, h.resolveTrack(ctx, artist, title, album, paths, single))
	}
	return inputs, nil
}

// resolveTrack resolves a single track to queue inputs. Inventory matches win;
// then a payload path that is usable inside the container; then metadata.
func (h *Handler) resolveTrack(ctx context.Context, artist, title, album string, paths []string, single bool) models.Inputs {
	if h.inventory != nil {
		results, err := h.inventory.FindByTrack(ctx, artist, title)
		if err != nil {
			// Inventory lookup failure must not hard-fail the webhook; fall back.
			slog.Warn("inventory lookup failed; falling back", "artist", artist, "title", title, "error", err)
		} else if best, ok := pickByAlbum(results, album); ok {
			in, err := scan.ResultInputs(best)
			if err == nil {
				return in
			}
			// Mirror the inventory-lookup-failure branch above: log and fall
			// through rather than silently dropping the conversion error.
			slog.Warn("inventory match could not be converted to inputs; falling back", "artist", artist, "title", title, "error", err)
		}
	}
	if path := h.usablePath(paths, title, single); path != "" {
		return pathInput(artist, title, album, path)
	}
	return h.metadataInput(artist, title, album)
}

// pickByAlbum chooses the best scan result for a track. When the album hint is
// present it prefers a result whose file path matches the album; otherwise it
// returns the first result (FindByTrack orders non-terminal rows first).
func pickByAlbum(results []models.ScanResult, album string) (models.ScanResult, bool) {
	if len(results) == 0 {
		return models.ScanResult{}, false
	}
	if albumKey := normalize.NormalizeKey(album); albumKey != "" {
		for _, res := range results {
			if strings.Contains(normalize.NormalizeKey(res.FilePath), albumKey) {
				return res, true
			}
		}
	}
	return results[0], true
}

// usablePath returns a payload path that exists inside the container for the
// given track, or "" when none applies. With a single track and a single path
// they are paired directly; otherwise a path is matched by basename to the
// track title so multi-track payloads target the right file.
func (h *Handler) usablePath(paths []string, title string, single bool) string {
	if len(paths) == 0 {
		return ""
	}
	if single && len(paths) == 1 {
		if safe, ok := h.confinedPayloadPath(paths[0]); ok && h.pathExists(safe) {
			return safe
		}
		return ""
	}
	titleKey := normalize.NormalizeKey(title)
	for _, path := range paths {
		safe, ok := h.confinedPayloadPath(path)
		if !ok {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(safe), filepath.Ext(safe))
		if titleKey != "" && strings.Contains(normalize.NormalizeKey(base), titleKey) && h.pathExists(safe) {
			return safe
		}
	}
	return ""
}

// confinedPayloadPath returns a raw webhook payload path only when it resolves
// to a location inside a configured library root, and returns that fully
// resolved (symlink-free) path so the value validated here is the exact value
// later stat-ed and written to. Confinement provides lexical + symlink
// containment against the operator-declared roots, which blocks an authenticated
// webhook from steering a lyric write outside them (path-injection guard). It
// returns ok=false when no roots are configured, the path lies outside every
// root, a symlink escapes its root, or the path does not exist (fail closed).
//
// Containment is enforced lexically and via EvalSymlinks at request time. The
// residual write-time symlink-swap TOCTOU (a path component swapped for a
// symlink before the worker writes) is not closed here but in the writing
// layer: lyrics.LRCWriter re-resolves and re-confines the output dir
// immediately before the write (#102), so the same roots passed here also
// confine the worker's write.
func (h *Handler) confinedPayloadPath(path string) (string, bool) {
	for _, root := range h.allowedRoots {
		if resolved, ok := pathutil.ResolveWithinRoot(root, path); ok {
			return resolved, true
		}
	}
	if len(h.allowedRoots) > 0 {
		// Observability for a misconfiguration or an injection attempt: a payload
		// path was supplied but matched no configured root after resolution.
		slog.Warn("webhook payload path rejected by library-root confinement; falling back", "path", path)
	}
	return "", false
}

func (h *Handler) pathExists(path string) bool {
	check := h.pathChecker
	if check == nil {
		check = defaultPathChecker
	}
	if err := check(path); err != nil {
		slog.Warn("webhook payload path not usable inside container; falling back", "path", path, "error", err)
		return false
	}
	return true
}

// pathInput builds queue inputs that write the .lrc next to a directly usable
// audio file path, mirroring how scan-created work derives its destination.
func pathInput(artist, title, album, path string) models.Inputs {
	outdir := filepath.Dir(path)
	base := filepath.Base(path)
	filename := strings.TrimSuffix(base, filepath.Ext(base)) + ".lrc"
	return models.Inputs{
		Track: models.Track{
			ArtistName: artist,
			TrackName:  title,
			AlbumName:  album,
		},
		Outdir:     outdir,
		Filename:   filename,
		SourcePath: path,
		OutputPaths: []models.OutputPath{{
			Outdir:   outdir,
			Filename: filename,
		}},
	}
}

func apiKey(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("apikey")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		scheme, token, ok := strings.Cut(v, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

func redactURI(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	q := cp.Query()
	if _, ok := q["apikey"]; ok {
		q.Set("apikey", "REDACTED")
		cp.RawQuery = q.Encode()
	}
	return cp.RequestURI()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

type lidarrWebhook struct {
	EventType  string            `json:"eventType"`
	Artist     lidarrArtist      `json:"artist"`
	Album      lidarrAlbum       `json:"album"`
	Track      lidarrTrack       `json:"track"`
	Tracks     []lidarrTrack     `json:"tracks"`
	TrackFiles []lidarrTrackFile `json:"trackFiles"`
}

type lidarrTrackFile struct {
	Path string `json:"path"`
}

// payloadPaths returns the non-empty trackFile paths carried by the webhook.
func (p lidarrWebhook) payloadPaths() []string {
	paths := make([]string, 0, len(p.TrackFiles))
	for _, tf := range p.TrackFiles {
		if path := strings.TrimSpace(tf.Path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

type lidarrArtist struct {
	ArtistName string `json:"artistName"`
}

type lidarrAlbum struct {
	Title string `json:"title"`
}

type lidarrTrack struct {
	Title string `json:"title"`
}
