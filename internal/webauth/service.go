package webauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// DefaultSessionTTL is how long a new session stays valid (design default: 7 days).
const DefaultSessionTTL = 7 * 24 * time.Hour

// MinPasswordLength is the minimum admin password length enforced at the service
// boundary. The onboarding UI (a later lane) also checks length; enforcing it
// here keeps the core safe-by-default regardless of caller.
const MinPasswordLength = 8

var (
	// ErrInvalidCredentials is returned by Login for any failure (unknown user or
	// wrong password). It is deliberately identical for both so the caller cannot
	// distinguish them and enumerate usernames.
	ErrInvalidCredentials = errors.New("webauth: invalid credentials")
	// ErrInvalidSession is returned by ValidateSession for an unknown, expired, or
	// orphaned session token.
	ErrInvalidSession = errors.New("webauth: invalid session")
	// ErrPasswordTooShort is returned by Setup, SetPassword and ChangePassword
	// when the password is shorter than MinPasswordLength.
	ErrPasswordTooShort = fmt.Errorf("webauth: password must be at least %d characters", MinPasswordLength)
	// ErrUserNotFound is returned by SetPassword and ChangePassword when the
	// named user does not exist. Unlike Login, both are reached only by an
	// operator who already controls the host or an authenticated session, so
	// naming the failure is an aid rather than an enumeration risk.
	ErrUserNotFound = errors.New("webauth: user not found")
	// ErrReauthRequired is returned by ChangePassword when the rotation committed
	// but the fresh session could not be created. The password HAS changed and
	// every earlier session is revoked, so the caller must not report the change
	// as failed: it sends the user to sign in again with the new password.
	ErrReauthRequired = errors.New("webauth: password changed, sign in again")
)

// Service ties the user and session stores together into the browser-auth core:
// first-run setup, login, session validation, logout, and expiry cleanup. It is
// designed to be wrapped (e.g. by a per-IP rate limiter in a later lane); it does
// not implement lockout itself.
type Service struct {
	users      UserStore
	sessions   SessionStore
	sessionTTL time.Duration
	now        func() time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithSessionTTL overrides the session lifetime (default DefaultSessionTTL).
func WithSessionTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.sessionTTL = ttl
		}
	}
}

// WithClock overrides the time source (for tests).
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// NewService returns a Service backed by the given stores.
func NewService(users UserStore, sessions SessionStore, opts ...Option) *Service {
	if users == nil || sessions == nil {
		panic("webauth: NewService: users and sessions must not be nil")
	}
	s := &Service{
		users:      users,
		sessions:   sessions,
		sessionTTL: DefaultSessionTTL,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Setup creates the first admin user. It rejects with ErrUserExists if any user
// already exists, so it can be called at most once. Atomicity is guaranteed by
// CreateFirstUser (a single conditional insert), not by the check below: the
// HasUsers call is only a fast path that avoids an expensive Argon2id hash when
// an admin already exists, and is safe to lose a race on.
func (s *Service) Setup(ctx context.Context, username, password string) (User, error) {
	if strings.TrimSpace(username) == "" {
		return User{}, fmt.Errorf("webauth: username must not be empty")
	}
	if len(password) < MinPasswordLength {
		return User{}, ErrPasswordTooShort
	}
	exists, err := s.users.HasUsers(ctx)
	if err != nil {
		return User{}, fmt.Errorf("webauth: setup: %w", err)
	}
	if exists {
		return User{}, ErrUserExists
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("webauth: setup: %w", err)
	}
	user, err := s.users.CreateFirstUser(ctx, username, hash)
	if err != nil {
		return User{}, fmt.Errorf("webauth: setup: %w", err)
	}
	return user, nil
}

// SetPassword replaces an existing user's password and revokes their sessions.
//
// This is the supported credential-rotation path (#545). Before it existed the
// env bootstrap was the only way a password ever got set, and it deliberately
// refuses to overwrite an existing admin -- so an operator who edited the
// bootstrap value and restarted saw a healthy service, an INFO line reading
// "skipped", and the OLD password still live. The only remaining route was
// deleting the user row from the database.
//
// Sessions are revoked as part of the rotation, ATOMICALLY with the hash update.
// An operator changing a compromised credential expects tokens minted under the
// old one to stop working, and the two writes must not be separable: applying
// the hash but failing the revoke would leave the new password active while old
// sessions stayed valid, and the returned error would tell the operator that
// nothing had happened. RotateCredential performs both in one transaction, so
// the rotation either fully applies or leaves the account untouched.
func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("webauth: username must not be empty")
	}
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("webauth: set password: %w", err)
	}
	// RotateCredential reports ErrUserNotFound itself (its UPDATE matches zero
	// rows), so no separate existence check is needed -- and a check-then-write
	// pair would reintroduce a race the single transaction closes.
	if _, err := s.users.RotateCredential(ctx, username, hash); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("webauth: set password: %w", err)
	}
	return nil
}

// ChangePassword replaces the password of the user with id userID after
// verifying currentPassword, revokes every session of that user, and returns the
// raw token of ONE fresh session for the browser that made the change (#545).
//
// A wrong current password returns ErrInvalidCredentials and changes nothing.
// The new password's length is checked before any hash is computed, so a refused
// change costs no Argon2id pass beyond the verify. The hash update and the
// revocation are one transaction, and a compare-and-swap on the hash that was
// verified (RotateCredentialIfCurrent): if another rotation committed in
// between, this one loses with ErrInvalidCredentials rather than overwriting
// it, so a stale password cannot undo an operator's rotation. The session is
// created after it commits, through the same path Login uses rather than Login
// itself, which would spend a second Argon2id pass and add a second failure
// point. If that session cannot be created the password has still changed, and
// the error wraps ErrReauthRequired so the caller can say so.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) (string, error) {
	user, ok, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("webauth: change password: %w", err)
	}
	if !ok {
		return "", ErrUserNotFound
	}
	valid, err := VerifyPassword(user.PasswordHash, currentPassword)
	if err != nil {
		// As in Login: an unparsable stored hash is a server-side defect, logged
		// without detail reaching the caller.
		slog.Error("webauth: stored password hash is unparsable", "user_id", user.ID, "error", err)
		return "", ErrInvalidCredentials
	}
	if !valid {
		return "", ErrInvalidCredentials
	}
	if len(newPassword) < MinPasswordLength {
		return "", ErrPasswordTooShort
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return "", fmt.Errorf("webauth: change password: %w", err)
	}
	// The swap is keyed by username; the one just loaded by id names the same
	// row. It reports ErrUserNotFound if the user vanished since the load and
	// ErrInvalidCredentials if the verified hash is no longer the stored one.
	if _, err := s.users.RotateCredentialIfCurrent(ctx, user.Username, user.PasswordHash, hash); err != nil {
		if errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrInvalidCredentials) {
			return "", err
		}
		return "", fmt.Errorf("webauth: change password: %w", err)
	}
	token, err := s.newSession(ctx, user.ID)
	if err != nil {
		return "", fmt.Errorf("webauth: change password: %w: %w", ErrReauthRequired, err)
	}
	return token, nil
}

// Login verifies credentials and, on success, creates a session and returns its
// raw token. It is constant-time and enumeration-safe: an unknown username still
// runs an Argon2id verify against a dummy hash, and both the unknown-user and
// wrong-password paths return the identical ErrInvalidCredentials.
func (s *Service) Login(ctx context.Context, username, password string) (string, error) {
	user, ok, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		return "", fmt.Errorf("webauth: login: %w", err)
	}
	if !ok {
		// Spend comparable time so a missing user is indistinguishable from a
		// wrong password by timing. The result is intentionally discarded.
		_, _ = VerifyPassword(dummyHash, password)
		return "", ErrInvalidCredentials
	}
	valid, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		// A stored hash we cannot parse is a server-side defect, not a credential
		// problem; log it but do not leak detail to the caller.
		slog.Error("webauth: stored password hash is unparsable", "user_id", user.ID, "error", err)
		return "", ErrInvalidCredentials
	}
	if !valid {
		return "", ErrInvalidCredentials
	}
	token, err := s.newSession(ctx, user.ID)
	if err != nil {
		return "", fmt.Errorf("webauth: login: %w", err)
	}
	return token, nil
}

// newSession creates a session for userID that expires one session TTL from now
// and returns its raw token. Login and ChangePassword both issue through it.
func (s *Service) newSession(ctx context.Context, userID string) (string, error) {
	return s.sessions.CreateSession(ctx, userID, s.now().Add(s.sessionTTL))
}

// ValidateSession resolves a raw session token to its owning user. It returns
// ErrInvalidSession for an unknown, expired, or orphaned token.
func (s *Service) ValidateSession(ctx context.Context, rawToken string) (*User, error) {
	if rawToken == "" {
		return nil, ErrInvalidSession
	}
	sess, ok, err := s.sessions.GetSessionByToken(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("webauth: validate session: %w", err)
	}
	if !ok {
		return nil, ErrInvalidSession
	}
	user, ok, err := s.users.GetByID(ctx, sess.UserID)
	if err != nil {
		return nil, fmt.Errorf("webauth: validate session: %w", err)
	}
	if !ok {
		// Session references a user that no longer exists; treat as invalid.
		return nil, ErrInvalidSession
	}
	return &user, nil
}

// Logout revokes the session for rawToken by deleting it. Logging out an unknown
// token is a no-op (no error), so a double-logout is harmless.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	if err := s.sessions.DeleteSession(ctx, rawToken); err != nil {
		return fmt.Errorf("webauth: logout: %w", err)
	}
	return nil
}

// CleanExpiredSessions deletes expired sessions and returns the count removed.
// Intended to be called periodically by a background sweeper (a later lane).
func (s *Service) CleanExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.sessions.CleanExpiredSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("webauth: clean expired sessions: %w", err)
	}
	return n, nil
}

// HasUsers reports whether an admin account exists (first-run detection).
func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	has, err := s.users.HasUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("webauth: has users: %w", err)
	}
	return has, nil
}
