package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/web/templates"
)

// keyIDDisplayLen is how many leading characters of the public key ID are shown
// in the masked list (the rest is elided). The full ID is a non-secret public
// identifier, but truncating keeps the table compact while staying unambiguous.
const keyIDDisplayLen = 8

// maxKeyNameLen bounds the operator-supplied key name server-side. It matches the
// create form's client-side maxlength so a normal submission is never rejected,
// while a forged over-long name cannot be persisted.
const maxKeyNameLen = 120

// handleWebhookKeys renders the webhook API key management page (#300): the
// masked list of managed keys plus the create form. It never renders raw key
// material or a full hash. The response is no-store (it exposes key metadata).
func (u *UI) handleWebhookKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	view := u.buildWebhookKeysView(r.Context())
	u.attachKeysCSRF(w, r, &view)
	render(w, r, templates.WebhookKeysPage(u.version, view, u.buildRail("")))
}

// handleCreateWebhookKey creates a new managed key and returns the panel fragment
// with the raw key revealed exactly once (#300). Order mirrors the settings save
// path: same-origin, then CSRF, then the backend check, then the work.
func (u *UI) handleCreateWebhookKey(w http.ResponseWriter, r *http.Request) {
	// The success response carries the one-time RAW key, so it must never be
	// cached - same threat model as the list page's no-store (which only carries
	// metadata). Set it first so even an early error response stays uncached.
	w.Header().Set("Cache-Control", "no-store")
	if !enforceSameOrigin(w, r) {
		return
	}
	if !enforceCSRFToken(w, r) {
		return
	}
	if u.keys == nil {
		http.Error(w, "key management unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	scopes, err := parseScopeForm(r.PostForm["scope"])
	if err != nil {
		// The form only offers valid scopes, so this is a forged/garbled request.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	// The form caps the name client-side (maxlength), but a forged POST could send
	// an arbitrarily large value and have it persisted. Bound it server-side and
	// refuse with a friendly re-render rather than storing it.
	if utf8.RuneCountInString(name) > maxKeyNameLen {
		view := u.buildWebhookKeysView(r.Context())
		u.attachKeysCSRF(w, r, &view)
		view.Error = fmt.Sprintf("Key name is too long (maximum %d characters).", maxKeyNameLen)
		render(w, r, templates.WebhookKeysPanel(view))
		return
	}

	created, err := u.keys.CreateKey(r.Context(), name, scopes)
	if err != nil {
		slog.Error("settings: create webhook key failed", "error", err)
		http.Error(w, "failed to create key", http.StatusInternalServerError)
		return
	}

	view := u.buildWebhookKeysView(r.Context())
	u.attachKeysCSRF(w, r, &view)
	view.NewKey = &templates.NewWebhookKey{
		Raw:  created.Raw,
		Name: created.Key.Name,
		ID:   created.Key.ID,
	}
	render(w, r, templates.WebhookKeysPanel(view))
}

// handleRevokeWebhookKey revokes a managed key by its public ID and returns the
// re-rendered panel fragment (#300). The raw key is unrecoverable after creation,
// so revocation is by ID. A revoke re-renders the panel WITHOUT NewKey, so a
// previously revealed raw key is never shown again.
func (u *UI) handleRevokeWebhookKey(w http.ResponseWriter, r *http.Request) {
	// The re-rendered panel carries key metadata; keep it out of caches like the
	// list page (set first so an early error response is uncached too).
	w.Header().Set("Cache-Control", "no-store")
	if !enforceSameOrigin(w, r) {
		return
	}
	if !enforceCSRFToken(w, r) {
		return
	}
	if u.keys == nil {
		http.Error(w, "key management unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		http.Error(w, "missing key id", http.StatusBadRequest)
		return
	}

	_, err := u.keys.RevokeKeyByID(r.Context(), id)
	if err != nil && !errors.Is(err, auth.ErrInvalidKey) {
		slog.Error("settings: revoke webhook key failed", "error", err)
		http.Error(w, "failed to revoke key", http.StatusInternalServerError)
		return
	}
	// Build the result view exactly once, after the revoke, so the rows reflect
	// the new state. ErrInvalidKey means the key was already gone (concurrent
	// revoke, stale page): render the current list with a converge notice rather
	// than erroring.
	view := u.buildWebhookKeysView(r.Context())
	u.attachKeysCSRF(w, r, &view)
	if errors.Is(err, auth.ErrInvalidKey) {
		view.Error = "That key no longer exists; the list has been refreshed."
	}
	render(w, r, templates.WebhookKeysPanel(view))
}

// attachKeysCSRF embeds a double-submit CSRF token in the view when the page is
// manageable (a key backend is wired). On token-generation failure the page
// stays read-only: Manageable is forced false so no create/revoke controls
// render, matching the settings page's fail-safe behavior.
func (u *UI) attachKeysCSRF(w http.ResponseWriter, r *http.Request, view *templates.WebhookKeysView) {
	if !view.Manageable {
		return
	}
	token, err := ensureCSRFToken(w, r, u.secureRequest(r))
	if err != nil {
		slog.Error("settings: CSRF token generation failed; key management disabled", "error", err)
		view.Manageable = false
		return
	}
	view.CSRFToken = token
}

// buildWebhookKeysView assembles the page view model from the managed key store.
// It carries only metadata (never raw key material or the full hash). When no key
// backend is wired the view is not Manageable and the page renders an unavailable
// notice.
func (u *UI) buildWebhookKeysView(ctx context.Context) templates.WebhookKeysView {
	view := templates.WebhookKeysView{ScopeOptions: defaultScopeOptions()}
	if u.keys == nil {
		return view
	}
	view.Manageable = true

	keys, err := u.keys.ListKeys(ctx)
	if err != nil {
		slog.Error("settings: list webhook keys failed", "error", err)
		view.Error = "The existing keys could not be loaded."
		return view
	}
	loc := serverDisplayLocation()
	view.Keys = make([]templates.WebhookKeyRow, 0, len(keys))
	for _, k := range keys {
		view.Keys = append(view.Keys, keyRow(k, loc))
	}
	return view
}

// keyRow maps one stored key onto its masked display row. The full hash and any
// raw material are intentionally never read here. Timestamps use the dashboard
// pattern (formatDashboardTime): the server renders a labeled value and emits the
// RFC3339 UTC for keys.js to reformat into the viewer's local zone, so the cell
// never shows bare UTC when the viewer is elsewhere.
func keyRow(k auth.Key, loc *time.Location) templates.WebhookKeyRow {
	created, createdISO, createdTZ := formatDashboardTime(k.CreatedAt, loc)
	row := templates.WebhookKeyRow{
		ID:               truncateKeyID(k.ID),
		FullID:           k.ID,
		Name:             keyDisplayName(k.Name),
		Scopes:           scopesDisplay(k.Scopes),
		Created:          created,
		CreatedISO:       createdISO,
		CreatedTZApplied: createdTZ,
	}
	if k.RevokedAt != nil {
		row.Revoked = true
		row.RevokedAt, row.RevokedAtISO, row.RevokedAtTZApplied = formatDashboardTime(*k.RevokedAt, loc)
	}
	return row
}

// truncateKeyID shortens the public key ID for display, eliding the tail. The ID
// is a public identifier (hash prefix), so this is presentation, not redaction.
func truncateKeyID(id string) string {
	if len(id) <= keyIDDisplayLen {
		return id
	}
	return id[:keyIDDisplayLen] + "…"
}

// keyDisplayName returns a placeholder for an unnamed key so the column never
// renders blank.
func keyDisplayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

// scopesDisplay renders a key's scopes as a comma-joined string, or a placeholder
// when somehow empty.
func scopesDisplay(scopes []auth.Scope) string {
	if len(scopes) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ", ")
}

// defaultScopeOptions are the scope checkboxes offered on the create form, with
// webhook pre-checked (the common case for a Lidarr key).
func defaultScopeOptions() []templates.WebhookScopeOption {
	return []templates.WebhookScopeOption{
		{Value: string(auth.ScopeWebhook), Label: "Webhook (accept Lidarr webhook calls)", Checked: true},
		{Value: string(auth.ScopeAdmin), Label: "Admin (full access, including status and metrics)", Checked: false},
	}
}

// parseScopeForm validates and normalizes the scope checkbox values from the
// create form. With nothing selected it defaults to the webhook scope so the
// common case needs no choice; an unknown value is rejected (forged request).
func parseScopeForm(values []string) ([]auth.Scope, error) {
	cleaned := make([]auth.Scope, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			cleaned = append(cleaned, auth.Scope(v))
		}
	}
	if len(cleaned) == 0 {
		cleaned = []auth.Scope{auth.ScopeWebhook}
	}
	return auth.NormalizeScopes(cleaned)
}

// serverDisplayLocation resolves the timezone for created/revoked timestamps,
// mirroring buildReportView: the TZ env var when set and valid, otherwise nil
// (formatReportTime then normalizes to UTC).
func serverDisplayLocation() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return nil
}
