package webauth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These cover #545: there was no supported way to change an existing admin's
// password. The only route was deleting the user row from the production
// database, because the env bootstrap refuses to overwrite an existing admin
// and no service method could set a new hash.

func TestSetPassword_ChangesTheStoredCredential(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := svc.SetPassword(ctx, "admin", "a-new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := svc.Login(ctx, "admin", "a-new-password"); err != nil {
		t.Fatalf("login with the new password failed: %v", err)
	}
	if _, err := svc.Login(ctx, "admin", "initial-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still works after rotation: err = %v; want ErrInvalidCredentials", err)
	}
}

// The rotation must be durable rather than in-memory only: the whole point is
// that it survives the restart that previously reverted operators' attempts.
func TestSetPassword_IsPersisted(t *testing.T) {
	svc, db := newTestService(t)
	store := NewSQLUserStore(db)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	before, _, err := store.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}

	if err := svc.SetPassword(ctx, "admin", "a-new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	after, ok, err := store.GetByUsername(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("GetByUsername after: ok=%v err=%v", ok, err)
	}
	if after.PasswordHash == before.PasswordHash {
		t.Fatal("stored password hash unchanged after SetPassword")
	}
	if after.ID != before.ID {
		t.Fatalf("user identity changed: %s -> %s; rotation must not recreate the row", before.ID, after.ID)
	}
}

func TestSetPassword_RejectsShortPassword(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	short := strings.Repeat("x", MinPasswordLength-1)
	if err := svc.SetPassword(ctx, "admin", short); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("err = %v; want ErrPasswordTooShort", err)
	}
	// The existing credential must survive a rejected rotation.
	if _, err := svc.Login(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("original password broken by a rejected rotation: %v", err)
	}
}

func TestSetPassword_UnknownUser(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := svc.SetPassword(ctx, "nobody", "a-new-password"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v; want ErrUserNotFound", err)
	}
}

// A rotation must not leave old sessions usable: the operator rotating a
// compromised credential expects existing logins to stop working.
func TestSetPassword_RevokesExistingSessions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := svc.Login(ctx, "admin", "initial-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, token); err != nil {
		t.Fatalf("session should be valid before rotation: %v", err)
	}

	if err := svc.SetPassword(ctx, "admin", "a-new-password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, token); err == nil {
		t.Fatal("session from before the rotation is still valid; a rotation must revoke existing sessions")
	}
}
