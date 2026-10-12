package webauth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// These cover the web half of #545: a signed-in user changes their own password
// by proving the current one. All of them run against real SQLite; the arms a
// healthy, quiet database cannot produce wrap the real stores and fail or
// precede exactly one call.

const (
	changeOldPassword = "initial-password"
	changeNewPassword = "a-new-password"
)

// changeFixture is an admin with two live sessions, as a browser and a second
// device would hold them.
type changeFixture struct {
	svc      *Service
	db       *sql.DB
	users    *SQLUserStore
	user     User
	sessionA string
	sessionB string
}

// newChangeFixture builds the fixture. wrap, when non-nil, substitutes the
// stores the service is built over (the fault-injection tests use it).
func newChangeFixture(t *testing.T, wrap func(UserStore, SessionStore) (UserStore, SessionStore)) changeFixture {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	f := changeFixture{db: db, users: NewSQLUserStore(db)}
	var users UserStore = f.users
	var sessions SessionStore = NewSQLSessionStore(db)
	if wrap != nil {
		users, sessions = wrap(users, sessions)
	}
	f.svc = NewService(users, sessions)

	var err error
	if f.user, err = f.svc.Setup(ctx, "admin", changeOldPassword); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if f.sessionA, err = f.svc.Login(ctx, "admin", changeOldPassword); err != nil {
		t.Fatalf("Login A: %v", err)
	}
	if f.sessionB, err = f.svc.Login(ctx, "admin", changeOldPassword); err != nil {
		t.Fatalf("Login B: %v", err)
	}
	return f
}

func (f changeFixture) storedHash(t *testing.T) string {
	t.Helper()
	u, ok, err := f.users.GetByID(context.Background(), f.user.ID)
	if err != nil || !ok {
		t.Fatalf("GetByID: ok=%v err=%v", ok, err)
	}
	return u.PasswordHash
}

func (f changeFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// assertUntouched fails unless the credential and both sessions are exactly as
// the fixture left them: the shape every refused change must leave behind.
func (f changeFixture) assertUntouched(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if got := f.storedHash(t); got != f.user.PasswordHash {
		t.Error("stored password hash changed by a refused change")
	}
	for name, token := range map[string]string{"A": f.sessionA, "B": f.sessionB} {
		if _, err := f.svc.ValidateSession(ctx, token); err != nil {
			t.Errorf("session %s revoked by a refused change: %v", name, err)
		}
	}
	if n := f.sessionCount(t); n != 2 {
		t.Errorf("sessions = %d after a refused change; want the original 2", n)
	}
}

func TestChangePassword_Succeeds(t *testing.T) {
	f := newChangeFixture(t, nil)
	ctx := context.Background()

	token, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if _, err := f.svc.Login(ctx, "admin", changeNewPassword); err != nil {
		t.Fatalf("login with the new password failed: %v", err)
	}
	if _, err := f.svc.Login(ctx, "admin", changeOldPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still works: err = %v; want ErrInvalidCredentials", err)
	}
	got, err := f.svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("returned token does not validate: %v", err)
	}
	if got.ID != f.user.ID {
		t.Fatalf("user id changed: %s -> %s; a change must not recreate the row", f.user.ID, got.ID)
	}
}

// Every session minted under the old password stops working, and exactly one
// new session exists: the one handed back to the browser that made the change.
func TestChangePassword_RevokesPriorSessionsAndIssuesOne(t *testing.T) {
	f := newChangeFixture(t, nil)
	ctx := context.Background()

	token, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	for name, old := range map[string]string{"A": f.sessionA, "B": f.sessionB} {
		if _, err := f.svc.ValidateSession(ctx, old); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("prior session %s: err = %v; want ErrInvalidSession", name, err)
		}
	}
	if token == f.sessionA || token == f.sessionB {
		t.Fatal("returned token is one of the prior sessions, not a fresh one")
	}
	if _, err := f.svc.ValidateSession(ctx, token); err != nil {
		t.Fatalf("returned token does not validate: %v", err)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("sessions = %d after a change; want exactly the 1 fresh session", n)
	}
}

func TestChangePassword_WrongCurrentPasswordChangesNothing(t *testing.T) {
	f := newChangeFixture(t, nil)

	token, err := f.svc.ChangePassword(context.Background(), f.user.ID, "not-the-password", changeNewPassword)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v; want ErrInvalidCredentials", err)
	}
	if token != "" {
		t.Error("a session token was returned for a wrong current password")
	}
	f.assertUntouched(t)
}

func TestChangePassword_RejectsShortNewPassword(t *testing.T) {
	f := newChangeFixture(t, nil)

	short := strings.Repeat("x", MinPasswordLength-1)
	token, err := f.svc.ChangePassword(context.Background(), f.user.ID, changeOldPassword, short)
	if !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("err = %v; want ErrPasswordTooShort", err)
	}
	if token != "" {
		t.Error("a session token was returned for a refused password")
	}
	f.assertUntouched(t)

	// The current password is judged first: a wrong one is never told that the
	// new one was too short as well.
	if _, err := f.svc.ChangePassword(context.Background(), f.user.ID, "not-the-password", short); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current + short new: err = %v; want ErrInvalidCredentials", err)
	}
	f.assertUntouched(t)
}

func TestChangePassword_UnknownUser(t *testing.T) {
	f := newChangeFixture(t, nil)

	token, err := f.svc.ChangePassword(context.Background(), "no-such-user-id", changeOldPassword, changeNewPassword)
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v; want ErrUserNotFound", err)
	}
	if token != "" {
		t.Error("a session token was returned for an unknown user")
	}
	f.assertUntouched(t)
}

// faultUserStore is the real user store with one call altered: GetByID fails
// while getErr is set, and the compare-and-swap rotation fails with rotateErr,
// or runs beforeRotate (a real competing write) and then delegates.
type faultUserStore struct {
	UserStore
	getErr       error
	rotateErr    error
	beforeRotate func()
}

func (s *faultUserStore) GetByID(ctx context.Context, id string) (User, bool, error) {
	if s.getErr != nil {
		return User{}, false, s.getErr
	}
	return s.UserStore.GetByID(ctx, id)
}

func (s *faultUserStore) RotateCredentialIfCurrent(ctx context.Context, username, current, hash string) (int64, error) {
	if s.rotateErr != nil {
		return 0, s.rotateErr
	}
	if s.beforeRotate != nil {
		s.beforeRotate()
	}
	return s.UserStore.RotateCredentialIfCurrent(ctx, username, current, hash)
}

// newFaultFixture is the fixture over a faultUserStore, armed after setup.
func newFaultFixture(t *testing.T) (changeFixture, *faultUserStore) {
	t.Helper()
	fault := &faultUserStore{}
	f := newChangeFixture(t, func(u UserStore, s SessionStore) (UserStore, SessionStore) {
		fault.UserStore = u
		return fault, s
	})
	return f, fault
}

// A store failure before the rotation commits is an ordinary failure, not
// ErrReauthRequired: nothing changed, so the user is still signed in under the
// old password.
func TestChangePassword_StoreFailureChangesNothing(t *testing.T) {
	sentinel := errors.New("store failed")
	arms := map[string]func(*faultUserStore){
		"lookup":   func(s *faultUserStore) { s.getErr = sentinel },
		"rotation": func(s *faultUserStore) { s.rotateErr = sentinel },
	}
	for name, arm := range arms {
		t.Run(name, func(t *testing.T) {
			f, fault := newFaultFixture(t)
			arm(fault)
			token, err := f.svc.ChangePassword(context.Background(), f.user.ID, changeOldPassword, changeNewPassword)
			if !errors.Is(err, sentinel) || errors.Is(err, ErrReauthRequired) {
				t.Fatalf("err = %v; want the store error wrapped, and not ErrReauthRequired", err)
			}
			if token != "" {
				t.Error("a session token was returned although the store failed")
			}
			*fault = faultUserStore{UserStore: fault.UserStore}
			f.assertUntouched(t)
		})
	}
}

// The operator rotates a compromised credential while a change verified
// against the OLD password sits between its verify and its rotation. The stale
// change must lose: it would otherwise overwrite the operator's rotation and
// hand a fresh session to whoever held the old password.
func TestChangePassword_LosesToACompetingRotation(t *testing.T) {
	const operatorPassword = "the-operators-password"
	f, fault := newFaultFixture(t)
	ctx := context.Background()
	fault.beforeRotate = func() {
		if err := f.svc.SetPassword(ctx, "admin", operatorPassword); err != nil {
			t.Fatalf("competing SetPassword: %v", err)
		}
	}

	token, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword)
	if !errors.Is(err, ErrInvalidCredentials) || token != "" {
		t.Fatalf("token set = %v, err = %v; want no token and ErrInvalidCredentials", token != "", err)
	}
	if n := f.sessionCount(t); n != 0 {
		t.Errorf("sessions = %d; want 0 (the operator's rotation revoked all, the stale change issued none)", n)
	}
	if _, err := f.svc.Login(ctx, "admin", changeNewPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("the stale change's password works: err = %v; want ErrInvalidCredentials", err)
	}
	if _, err := f.svc.Login(ctx, "admin", operatorPassword); err != nil {
		t.Fatalf("the operator's password was overwritten: %v", err)
	}
}

// The user is deleted between the load and the swap. That is "user gone", told
// apart from "hash changed" inside the rotation's own transaction.
func TestChangePassword_UserVanishedBeforeRotation(t *testing.T) {
	f, fault := newFaultFixture(t)
	ctx := context.Background()
	fault.beforeRotate = func() {
		if _, err := f.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, f.user.ID); err != nil {
			t.Fatalf("delete user: %v", err)
		}
	}

	token, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword)
	if !errors.Is(err, ErrUserNotFound) || token != "" {
		t.Fatalf("token set = %v, err = %v; want no token and ErrUserNotFound", token != "", err)
	}
}

func TestRotateCredentialIfCurrent(t *testing.T) {
	f := newChangeFixture(t, nil)
	ctx := context.Background()
	const rotated = "$argon2id$rotated"

	if _, err := f.users.RotateCredentialIfCurrent(ctx, "nobody", f.user.PasswordHash, rotated); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user: err = %v; want ErrUserNotFound", err)
	}
	if n, err := f.users.RotateCredentialIfCurrent(ctx, "admin", "$argon2id$stale", rotated); !errors.Is(err, ErrInvalidCredentials) || n != 0 {
		t.Fatalf("stale hash: revoked = %d, err = %v; want 0 and ErrInvalidCredentials", n, err)
	}
	f.assertUntouched(t)

	n, err := f.users.RotateCredentialIfCurrent(ctx, "admin", f.user.PasswordHash, rotated)
	if err != nil || n != 2 {
		t.Fatalf("matching hash: revoked = %d, err = %v; want both sessions revoked", n, err)
	}
	if got := f.storedHash(t); got != rotated {
		t.Error("a matching swap did not replace the stored hash")
	}
	if left := f.sessionCount(t); left != 0 {
		t.Errorf("sessions = %d after a matching swap; want 0", left)
	}
}

// An unparsable stored hash is logged, without the stored value in the line.
func TestChangePassword_CorruptStoredHashIsLoggedNotLeaked(t *testing.T) {
	const corrupt = "corrupt-stored-value"
	f := newChangeFixture(t, nil)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, corrupt, f.user.ID); err != nil {
		t.Fatalf("corrupt the hash: %v", err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v; want ErrInvalidCredentials", err)
	}
	if !strings.Contains(logs.String(), "stored password hash is unparsable") {
		t.Errorf("no error line for an unparsable stored hash; log = %q", logs.String())
	}
	if strings.Contains(logs.String(), corrupt) {
		t.Error("the log carries the stored hash text")
	}
}

// faultSessionStore is the real session store with CreateSession failing once
// armed, so the fixture's own logins still succeed.
type faultSessionStore struct {
	SessionStore
	createErr error
}

func (s *faultSessionStore) CreateSession(ctx context.Context, userID string, expiresAt time.Time) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	return s.SessionStore.CreateSession(ctx, userID, expiresAt)
}

// The rotation committed but no session could be issued. The caller must be
// able to tell this apart from "nothing happened": the password HAS changed.
func TestChangePassword_SessionFailureAfterRotationIsReauthRequired(t *testing.T) {
	fault := &faultSessionStore{}
	f := newChangeFixture(t, func(u UserStore, s SessionStore) (UserStore, SessionStore) {
		fault.SessionStore = s
		return u, fault
	})
	ctx := context.Background()
	sentinel := errors.New("session insert failed")
	fault.createErr = sentinel

	token, err := f.svc.ChangePassword(ctx, f.user.ID, changeOldPassword, changeNewPassword)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err = %v; want ErrReauthRequired", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v; want the session-store error kept in the chain", err)
	}
	if token != "" {
		t.Error("a session token was returned alongside ErrReauthRequired")
	}

	// The change stands: prior sessions are gone and only the new password works.
	if n := f.sessionCount(t); n != 0 {
		t.Errorf("sessions = %d; want 0 (all revoked, none issued)", n)
	}
	fault.createErr = nil
	if _, err := f.svc.Login(ctx, "admin", changeNewPassword); err != nil {
		t.Fatalf("login with the new password failed: %v", err)
	}
	if _, err := f.svc.Login(ctx, "admin", changeOldPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still works: err = %v; want ErrInvalidCredentials", err)
	}
}
