package webauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// rotationStore is a UserStore whose lookup succeeds but whose write fails, so
// the rotation's failure branches can be exercised independently.
type rotationStore struct {
	user      User
	lookupErr error
	updateErr error
}

func (s rotationStore) CreateUser(context.Context, string, string) (User, error) {
	return s.user, nil
}
func (s rotationStore) CreateFirstUser(context.Context, string, string) (User, error) {
	return s.user, nil
}
func (s rotationStore) GetByUsername(context.Context, string) (User, bool, error) {
	if s.lookupErr != nil {
		return User{}, false, s.lookupErr
	}
	return s.user, true, nil
}
func (s rotationStore) GetByID(context.Context, string) (User, bool, error) {
	return s.user, true, nil
}
func (s rotationStore) HasUsers(context.Context) (bool, error) { return true, nil }
func (s rotationStore) RotateCredential(context.Context, string, string) (int64, error) {
	if s.lookupErr != nil {
		return 0, s.lookupErr
	}
	return 0, s.updateErr
}

// okSessionStore succeeds at everything.
type okSessionStore struct{}

func (okSessionStore) CreateSession(context.Context, string, time.Time) (string, error) {
	return "token", nil
}
func (okSessionStore) GetSessionByToken(context.Context, string) (Session, bool, error) {
	return Session{}, false, nil
}
func (okSessionStore) DeleteSession(context.Context, string) error         { return nil }
func (okSessionStore) CleanExpiredSessions(context.Context) (int64, error) { return 0, nil }

func TestSetPassword_LookupFailurePropagates(t *testing.T) {
	sentinel := errors.New("store down")
	svc := NewService(rotationStore{lookupErr: sentinel}, okSessionStore{})

	err := svc.SetPassword(context.Background(), "admin", "a-new-password")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want the store error wrapped", err)
	}
}

func TestSetPassword_WriteFailurePropagates(t *testing.T) {
	sentinel := errors.New("write failed")
	svc := NewService(rotationStore{user: User{ID: "u1"}, updateErr: sentinel}, okSessionStore{})

	err := svc.SetPassword(context.Background(), "admin", "a-new-password")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want the write error wrapped", err)
	}
}

// Rotation is atomic, so a failure cannot half-apply. Exercised against a real
// database: a rotation naming an unknown user must leave an EXISTING user's
// credential and sessions completely untouched. If the UPDATE and DELETE were
// separable, a mistargeted rotation could still clear sessions.
func TestRotateCredential_FailureLeavesEverythingUntouched(t *testing.T) {
	svc, db := newTestService(t)
	users := NewSQLUserStore(db)
	sessions := NewSQLSessionStore(db)
	ctx := context.Background()

	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	before, _, err := users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if _, err := sessions.CreateSession(ctx, before.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := users.RotateCredential(ctx, "nobody", "$argon2id$other"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v; want ErrUserNotFound", err)
	}

	after, ok, err := users.GetByUsername(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("GetByUsername after: ok=%v err=%v", ok, err)
	}
	if after.PasswordHash != before.PasswordHash {
		t.Error("an unrelated user's password hash changed during a failed rotation")
	}
	if _, err := svc.Login(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("existing credential broken by a failed rotation: %v", err)
	}
}

// A successful rotation reports how many sessions it revoked, so a caller can
// tell the operator that existing logins are actually dead.
func TestRotateCredential_ReportsRevokedCount(t *testing.T) {
	svc, db := newTestService(t)
	users := NewSQLUserStore(db)
	sessions := NewSQLSessionStore(db)
	ctx := context.Background()

	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	u, _, err := users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	for range 2 {
		if _, err := sessions.CreateSession(ctx, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	revoked, err := users.RotateCredential(ctx, "admin", "$argon2id$rotated")
	if err != nil {
		t.Fatalf("RotateCredential: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked = %d; want 2", revoked)
	}
}
