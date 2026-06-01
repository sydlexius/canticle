package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
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

// Handler serves the HTTP API.
type Handler struct {
	auth     Authenticator
	queue    WorkQueue
	outdir   string
	priority int
	ready    Readiness
	stats    StatusReporter
	mux      *http.ServeMux
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

// NewHandler creates an HTTP API handler.
func NewHandler(a Authenticator, q WorkQueue, outdir string, opts ...Option) *Handler {
	h := &Handler{
		auth:     a,
		queue:    q,
		outdir:   outdir,
		priority: queue.PriorityWebhook,
		mux:      http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(h)
	}
	h.mux.HandleFunc("POST /api/v1/webhooks/lidarr", h.handleLidarr)
	h.mux.HandleFunc("GET /healthz", h.handleHealthz)
	h.mux.HandleFunc("GET /readyz", h.handleReadyz)
	h.mux.HandleFunc("GET /api/v1/status", h.handleStatus)
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

// ServeHTTP logs requests and dispatches them to API routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.mux.ServeHTTP(rec, r)
	slog.Info("http request", "method", r.Method, "uri", redactURI(r.URL), "status", rec.status) //nolint:gosec // G706: request URI is logged as a structured slog field after apikey redaction; slog escapes values
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
		inputs, err := h.inputs(payload)
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
		inputs, err := h.inputs(payload)
		if err != nil {
			slog.Info("lidarr delete received without cleanup target", "artist", payload.Artist.ArtistName, "album", payload.Album.Title)
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

func (h *Handler) inputs(payload lidarrWebhook) ([]models.Inputs, error) {
	artist := strings.TrimSpace(payload.Artist.ArtistName)
	if artist == "" {
		return nil, fmt.Errorf("missing artist")
	}
	album := strings.TrimSpace(payload.Album.Title)
	tracks := payload.Tracks
	if len(tracks) == 0 && payload.Track.Title != "" {
		tracks = []lidarrTrack{payload.Track}
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("missing tracks")
	}
	inputs := make([]models.Inputs, 0, len(tracks))
	for _, track := range tracks {
		title := strings.TrimSpace(track.Title)
		if title == "" {
			return nil, fmt.Errorf("missing track title")
		}
		inputs = append(inputs, models.Inputs{
			Track: models.Track{
				ArtistName: artist,
				TrackName:  title,
				AlbumName:  album,
			},
			Outdir: h.outdir,
			OutputPaths: []models.OutputPath{{
				Outdir: h.outdir,
			}},
		})
	}
	return inputs, nil
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
	EventType string        `json:"eventType"`
	Artist    lidarrArtist  `json:"artist"`
	Album     lidarrAlbum   `json:"album"`
	Track     lidarrTrack   `json:"track"`
	Tracks    []lidarrTrack `json:"tracks"`
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
