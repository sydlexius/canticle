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
// /setup GET/POST endpoints, the admin-existence gate that opens setup until the
// first account exists and closes it permanently afterwards (#461), the
// first-run redirect of the UI routes to /setup, and the env-independent
// admin-credential + runtime-secret writes. It is safe for concurrent use.
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
	// hashSlots bounds how many setup submissions may be inside Argon2id at once.
	//
	// Password hashing is deliberately expensive in MEMORY (64 MiB, 4 threads --
	// see webauth/password.go), and since #461 an unauthenticated client can
	// reach it during the open window. Unbounded, a few dozen concurrent POSTs
	// allocate GiB and can OOM the daemon on a memory-capped container, which
	// is worse than it sounds: a crashed daemon restarts with the window STILL
	// OPEN, so it is a denial of onboarding rather than a transient outage.
	//
	// A per-IP rate limiter (the /login shape) is the wrong instrument here: the
	// quantity to bound is concurrent memory, and per-IP accounting does not
	// bound it when requests arrive from many addresses -- or, on Docker bridge
	// networking, when every LAN client presents as the same gateway address.
	// A global semaphore bounds the measured quantity directly. Excess requests
	// are refused with 503 + Retry-After rather than queued, so an attacker
	// cannot pin memory by parking goroutines in a wait.
	hashSlots chan struct{}
}

// NewOnboarding builds the onboarding flow. service performs first-run detection
// and admin creation; secretStore (optional, may be nil) persists the runtime
// secrets; auth issues the session cookie after a successful setup; version
// labels the page.
//
// policy no longer decides who may REACH /setup -- admin-existence does (#461).
// It now serves three narrower purposes: choosing between 404 and a /login
// redirect once setup has closed, deciding whether serving the open form is
// worth a WARN, and resolving the client IP for that log. A nil policy defaults
// to loopback-only, which is the safe default for all three.
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
		service:   service,
		secrets:   secretStore,
		auth:      auth,
		policy:    policy,
		version:   version,
		hashSlots: make(chan struct{}, maxConcurrentSetupHashes),
	}
}

// maxConcurrentSetupHashes bounds concurrent Argon2id hashing on /setup. At
// 64 MiB per hash this caps the route's transient footprint at ~256 MiB, which
// a container small enough to OOM on that is also small enough that refusing
// the surplus is the better failure. Onboarding is a once-per-deployment action
// by exactly one operator, so real traffic never approaches the limit.
const maxConcurrentSetupHashes = 4

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

// handleSetupForm renders the onboarding form (GET /setup).
//
// Reachability is decided by whether an admin EXISTS, not by the trusted-CIDR
// policy (#461). Before the first admin is created the page is served to any
// client that can reach the port; afterwards a non-trusted client gets 404 (the
// page's existence is not revealed off-network) and a trusted one is sent to
// /login, since re-running setup is not how a password is changed.
//
// The old order gated on trust FIRST, which made bootstrapping require handing
// out a permanent, blanket auth bypass: RequireSession short-circuits on
// policy.Trusted (auth.go), so any CIDR added to reach /setup also granted
// unauthenticated access to every UI route, forever. Deciding on admin-existence
// instead bounds the exposure to the pre-onboarding window, which closes
// permanently the moment setup succeeds and cannot reopen (see hasAdmin's
// monotonic latch, and webauth.Setup's atomic INSERT ... WHERE NOT EXISTS, which
// makes the claim first-writer-wins).
func (o *Onboarding) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	has, err := o.hasAdmin(r.Context())
	if err != nil {
		o.refuseOnAdminCheckError(w, r, err)
		return
	}
	if has {
		// Setup is closed. Preserve the pre-#461 disclosure posture exactly:
		// untrusted clients must not learn the endpoint exists.
		if !o.policy.Trusted(r) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	o.warnOpenSetup(r)
	o.renderSetup(w, r, http.StatusOK, o.version, "", "")
}

// refuseOnAdminCheckError answers a request whose admin-existence check failed.
//
// It fails CLOSED in both senses. An untrusted client gets the same 404 it would
// see if an admin existed: before #461 the trust gate ran first, so a DB fault
// could not reveal the endpoint off-network, and a 500 here would reintroduce
// that disclosure at exactly the moment an attacker probing for 500s learns the
// most. A trusted client gets the real 500, because an operator debugging their
// own daemon needs to see the fault rather than a fictional 404.
//
// Nothing is served either way: hasAdmin reports (false, err) on failure, so the
// error value is indistinguishable from "no admin exists". Treating an unknown
// answer as "setup is closed" is the safe direction -- the alternative would
// reopen the onboarding window on any transient DB error.
func (o *Onboarding) refuseOnAdminCheckError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("onboarding: first-run check failed", "error", err)
	if !o.policy.Trusted(r) {
		http.NotFound(w, r)
		return
	}
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// warnOpenSetup logs that the unauthenticated setup window was exercised by a
// client the trust policy would NOT have admitted. The window is bounded and
// self-closing, but it is still a period in which whoever reaches the port first
// claims the admin account, so an operator who left the port exposed should be
// able to see that in the log rather than infer it. A trusted client is not
// logged: that is the ordinary, expected path and would be pure noise.
func (o *Onboarding) warnOpenSetup(r *http.Request) {
	if o.policy.Trusted(r) {
		return
	}
	slog.Warn("serving first-run setup to an untrusted client because no admin account exists yet",
		"remote", o.policy.ClientIP(r),
		"hint", "create the admin account now; the setup page closes permanently once it exists")
}

// handleSetup processes the onboarding submission (POST /setup). It mirrors
// handleSetupForm's gate (admin-existence decides reachability, not the
// trusted-CIDR policy -- see that function and #461), validates the credentials,
// creates the admin (the race against a concurrent setup is closed atomically by
// webauth.Setup's conditional insert), optionally writes the runtime secrets,
// then logs the new admin in and redirects to the UI.
func (o *Onboarding) handleSetup(w http.ResponseWriter, r *http.Request) {
	// The closed-setup refusal runs FIRST, ahead of the same-origin and CSRF
	// checks, because both of those answer 403 and a 403 where the mux otherwise
	// 404s is an endpoint-existence oracle: an attacker probing /setup post-admin
	// would learn it is there without ever holding a valid token. GET already
	// 404s for this client, so refusing here reveals nothing new, and the check
	// is a latched in-memory read once an admin exists.
	has, err := o.hasAdmin(r.Context())
	if err != nil {
		o.refuseOnAdminCheckError(w, r, err)
		return
	}
	if has && !o.policy.Trusted(r) {
		http.NotFound(w, r)
		return
	}

	// Same-origin is the real defense against a hostile page in the operator's
	// browser driving this route during the open window: it rejects the request
	// on Sec-Fetch-Site/Origin/Referer, which a cross-site caller cannot forge.
	if !enforceSameOrigin(w, r) {
		return
	}
	// The double-submit token is defense in depth, NOT the anti-CSRF primitive.
	// It compares two client-supplied values with no server-side binding, so a
	// direct attacker simply mints both halves; what makes it useful against a
	// cross-site caller is SameSite=Lax withholding the cookie, which is a
	// property of the cookie rather than of the comparison. Keep it (it costs
	// nothing and closes same-site-but-untrusted script paths), but do not treat
	// it as the reason this route is safe -- enforceSameOrigin above is.
	if !enforceCSRFToken(w, r) {
		return
	}
	if has {
		// Setup already completed (possibly by a concurrent request); the page is
		// one-shot, so send the client to login rather than re-render the form.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	o.warnOpenSetup(r)

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

	// Acquire a hashing slot AFTER the cheap validation above, so a malformed or
	// too-short submission never occupies one. Refuse rather than queue: parking
	// goroutines here would let an attacker hold memory and defeat the cap.
	select {
	case o.hashSlots <- struct{}{}:
		defer func() { <-o.hashSlots }()
	default:
		slog.Warn("onboarding: refusing setup submission; hashing capacity is saturated",
			"remote", o.policy.ClientIP(r), "limit", maxConcurrentSetupHashes)
		w.Header().Set("Retry-After", "2")
		o.renderSetup(w, r, http.StatusServiceUnavailable, o.version,
			"The server is busy. Please try again in a moment.", username)
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
