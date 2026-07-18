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
func (s rotationStore) UpdatePasswordHash(context.Context, string, string) error {
	return s.updateErr
}

// okSessionStore succeeds at everything except, optionally, the revoke.
type okSessionStore struct{ revokeErr error }

func (okSessionStore) CreateSession(context.Context, string, time.Time) (string, error) {
	return "token", nil
}
func (okSessionStore) GetSessionByToken(context.Context, string) (Session, bool, error) {
	return Session{}, false, nil
}
func (okSessionStore) DeleteSession(context.Context, string) error         { return nil }
func (okSessionStore) CleanExpiredSessions(context.Context) (int64, error) { return 0, nil }
func (s okSessionStore) DeleteSessionsForUser(context.Context, string) (int64, error) {
	return 0, s.revokeErr
}

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

// A failed session revoke must be reported, not swallowed. The hash is already
// written at that point, so the new password works -- but the operator needs to
// know that sessions minted under the old one may still be live, which is
// exactly the thing a rotation is meant to end.
func TestSetPassword_RevokeFailureIsReported(t *testing.T) {
	sentinel := errors.New("revoke failed")
	svc := NewService(rotationStore{user: User{ID: "u1"}}, okSessionStore{revokeErr: sentinel})

	err := svc.SetPassword(context.Background(), "admin", "a-new-password")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want the revoke failure surfaced", err)
	}
}
