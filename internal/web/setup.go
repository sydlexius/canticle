package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/sydlexius/canticle/internal/secrets"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/webauth"
	"github.com/sydlexius/canticle/web/templates"
)

// passwordTooShortMsg is the single user-facing copy for a too-short password,
// derived from webauth.MinPasswordLength so the UI copy and the enforced policy
// can never drift apart.
var passwordTooShortMsg = fmt.Sprintf("Password must be at least %d characters.", webauth.MinPasswordLength)

// OnboardingService is the subset of *webauth.Service the first-run flow needs:
// first-run detection, admin creation, and login (to issue the new admin a
// session). Keeping it an interface lets tests substitute the real service over
// in-memory SQLite while the web package stays decoupled from the stores.
type OnboardingService interface {
	HasUsers(ctx context.Context) (bool, error)
	Setup(ctx context.Context, username, password string) (webauth.User, error)
	Login(ctx context.Context, username, password string) (string, error)
}

// SecretSetter is the subset of secrets.Store onboarding uses to persist the
// optional runtime secrets entered on the setup form. A nil setter disables the
// secret fields (admin creation still works).
type SecretSetter interface {
	Set(ctx context.Context, name, plaintext string) error
}

// Onboarding implements the first-run setup flow (issue #204, lane 4): the
// /setup GET/POST endpoints, the access gate that limits setup to loopback or a
// configured trusted network, the first-run redirect of the UI routes to
// /setup, and the env-independent admin-credential + runtime-secret writes. It
// is safe for concurrent use.
type Onboarding struct {
	service OnboardingService
	secrets SecretSetter // may be nil (no secret fields)
	auth    *Auth        // issues the post-setup session cookie
	policy  *trustnet.Policy
	version string
	// adminExists latches true once an admin is known to exist. In v1 there is no
	// admin-deletion path, so the first-run state is monotonic: once HasUsers
	// reports true it can never revert. Caching it lets the per-request gates skip
	// the DB query for the entire post-onboarding lifetime of the process.
	adminExists atomic.Bool
}

// NewOnboarding builds the onboarding flow. service performs first-run detection
// and admin creation; secretStore (optional, may be nil) persists the runtime
// secrets; auth issues the session cookie after a successful setup; policy gates
// who may reach /setup (a nil policy defaults to loopback-only, the safe
// default); version labels the page.
func NewOnboarding(service OnboardingService, secretStore SecretSetter, auth *Auth, policy *trustnet.Policy, version string) *Onboarding {
	if service == nil {
		panic("web: NewOnboarding: service must not be nil")
	}
	if auth == nil {
		panic("web: NewOnboarding: auth must not be nil")
	}
	if policy == nil {
		policy = trustnet.LoopbackOnly()
	}
	return &Onboarding{
		service: service,
		secrets: secretStore,
		auth:    auth,
		policy:  policy,
		version: version,
	}
}

// hasAdmin reports whether an admin account exists, caching a true result for
// the life of the process. The first-run state is monotonic (no admin-deletion
// in v1), so once the latch is set the DB query is skipped; the pre-admin path
// still queries every time so the first admin is detected promptly.
func (o *Onboarding) hasAdmin(ctx context.Context) (bool, error) {
	if o.adminExists.Load() {
		return true, nil
	}
	has, err := o.service.HasUsers(ctx)
	if err != nil {
		return false, err
	}
	if has {
		o.adminExists.Store(true)
	}
	return has, nil
}

// FirstRunGate wraps the authenticated UI routes so that, until an admin exists,
// every UI page redirects to /setup instead of serving (or redirecting to
// /login). Once an admin exists it is transparent and delegates straight to the
// session middleware it wraps.
func (o *Onboarding) FirstRunGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		has, err := o.hasAdmin(r.Context())
		if err != nil {
			slog.Error("onboarding: first-run check failed", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if !has {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSetupForm renders the onboarding form (GET /setup). It is reachable only
// from a trusted source; a non-trusted client gets 404 (the page's existence is
// not revealed off-network). Once an admin exists the page is closed and the
// client is sent to /login (one-shot: re-running setup is not how a password is
// changed).
func (o *Onboarding) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if !o.policy.Trusted(r) {
		http.NotFound(w, r)
		return
	}
	has, err := o.hasAdmin(r.Context())
	if err != nil {
		slog.Error("onboarding: first-run check failed", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if has {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	o.renderSetup(w, r, http.StatusOK, o.version, "", "")
}

// handleSetup processes the onboarding submission (POST /setup). It re-applies
// the trusted-source gate, validates the credentials, creates the admin (the
// race against a concurrent setup is closed atomically by webauth.Setup's
// conditional insert), optionally writes the runtime secrets, then logs the new
// admin in and redirects to the UI.
func (o *Onboarding) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) {
		return
	}
	if !o.policy.Trusted(r) {
		http.NotFound(w, r)
		return
	}
	// CSRF validation after the trust gate: untrusted peers are already refused
	// above; for trusted peers the double-submit token prevents login-CSRF.
	if !enforceCSRFToken(w, r) {
		return
	}
	has, err := o.hasAdmin(r.Context())
	if err != nil {
		slog.Error("onboarding: first-run check failed", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if has {
		// Setup already completed (possibly by a concurrent request); the page is
		// one-shot, so send the client to login rather than re-render the form.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		o.renderSetup(w, r, http.StatusBadRequest, o.version, "Invalid form submission.", "")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	if username == "" {
		o.renderSetup(w, r, http.StatusBadRequest, o.version, "Username is required.", username)
		return
	}
	if len(password) < webauth.MinPasswordLength {
		o.renderSetup(w, r, http.StatusBadRequest, o.version, passwordTooShortMsg, username)
		return
	}
	if password != confirm {
		o.renderSetup(w, r, http.StatusBadRequest, o.version, "Passwords do not match.", username)
		return
	}

	if _, err := o.service.Setup(r.Context(), username, password); err != nil {
		switch {
		case errors.Is(err, webauth.ErrUserExists):
			// Lost the race to a concurrent setup; the account now exists.
			o.adminExists.Store(true)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case errors.Is(err, webauth.ErrPasswordTooShort):
			o.renderSetup(w, r, http.StatusBadRequest, o.version, passwordTooShortMsg, username)
		default:
			slog.Error("onboarding: admin creation failed", "error", err)
			o.renderSetup(w, r, http.StatusInternalServerError, o.version,
				"Could not create the admin account. Please try again.", username)
		}
		return
	}

	// The admin now exists; latch it so the per-request gates skip the DB query
	// from here on (matches hasAdmin's monotonic-state assumption).
	o.adminExists.Store(true)

	o.writeSecrets(r.Context(),
		strings.TrimSpace(r.PostFormValue("musixmatch_token")),
		strings.TrimSpace(r.PostFormValue("webhook_api_key")))

	// Log the new admin in directly so onboarding flows into the UI without a
	// second credential prompt. A login failure here is unexpected (the account
	// was just created) but must not strand the operator; fall back to /login.
	token, err := o.service.Login(r.Context(), username, password)
	if err != nil {
		slog.Error("onboarding: auto-login after setup failed", "error", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	o.auth.setSessionCookie(w, r, token)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// writeSecrets persists any non-blank optional secret fields through the
// encrypted store. A blank field is skipped, leaving the existing TOML/env value
// in place (the DB is the lowest-precedence source). A write failure is logged
// but never fails onboarding: the admin account already exists, and the operator
// can set the secret later via the `secrets set` CLI.
func (o *Onboarding) writeSecrets(ctx context.Context, mxToken, webhookKey string) {
	if o.secrets == nil {
		if mxToken != "" || webhookKey != "" {
			slog.Warn("onboarding: secret fields submitted but no secret store is configured; ignoring")
		}
		return
	}
	if mxToken != "" {
		if err := o.secrets.Set(ctx, secrets.NameMusixmatchToken, mxToken); err != nil {
			slog.Error("onboarding: failed to store musixmatch token", "error", err)
		}
	}
	if webhookKey != "" {
		if err := o.secrets.Set(ctx, secrets.NameWebhookAPIKey, webhookKey); err != nil {
			slog.Error("onboarding: failed to store webhook API key", "error", err)
		}
	}
}

// renderSetup renders the setup page with the given status. It generates a
// fresh CSRF token, sets the CSRF cookie, then renders the page. Like the login
// page, the setup page is never cached (it reflects setup state and may set cookies).
func (o *Onboarding) renderSetup(w http.ResponseWriter, r *http.Request, status int, version, errMsg, username string) {
	csrfToken, err := ensureCSRFToken(w, r, o.auth.secureRequest(r))
	if err != nil {
		slog.Error("setup: failed to generate CSRF token", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	renderWithStatus(w, r, status, templates.SetupPage(version, errMsg, username, csrfToken))
}
