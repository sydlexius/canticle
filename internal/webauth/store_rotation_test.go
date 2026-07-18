package webauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdatePasswordHash_UnknownUserReturnsNotFound(t *testing.T) {
	_, db := newTestService(t)
	store := NewSQLUserStore(db)

	err := store.UpdatePasswordHash(context.Background(), "nobody", "$argon2id$fake")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v; want ErrUserNotFound", err)
	}
}

func TestUpdatePasswordHash_RejectsEmptyInputs(t *testing.T) {
	_, db := newTestService(t)
	store := NewSQLUserStore(db)
	ctx := context.Background()

	if err := store.UpdatePasswordHash(ctx, "", "$argon2id$fake"); err == nil {
		t.Error("empty username accepted; want an error")
	}
	if err := store.UpdatePasswordHash(ctx, "admin", ""); err == nil {
		t.Error("empty hash accepted; want an error")
	}
}

// GetByUsername is case-insensitive, so the update must match the same way or a
// rotation could silently miss the row it just read.
func TestUpdatePasswordHash_IsCaseInsensitive(t *testing.T) {
	svc, db := newTestService(t)
	store := NewSQLUserStore(db)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "Admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if err := store.UpdatePasswordHash(ctx, "aDmIn", "$argon2id$rotated"); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}
	u, ok, err := store.GetByUsername(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("GetByUsername: ok=%v err=%v", ok, err)
	}
	if u.PasswordHash != "$argon2id$rotated" {
		t.Errorf("hash = %q; want the rotated value", u.PasswordHash)
	}
}

func TestDeleteSessionsForUser(t *testing.T) {
	svc, db := newTestService(t)
	sessions := NewSQLSessionStore(db)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	users := NewSQLUserStore(db)
	u, _, err := users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}

	for range 3 {
		if _, err := sessions.CreateSession(ctx, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	n, err := sessions.DeleteSessionsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d; want 3", n)
	}
}

// Deleting sessions for a user with none must be a no-op, not an error: a
// rotation on an account that was never logged in is perfectly normal.
func TestDeleteSessionsForUser_NoSessionsIsNoOp(t *testing.T) {
	_, db := newTestService(t)
	sessions := NewSQLSessionStore(db)

	n, err := sessions.DeleteSessionsForUser(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d; want 0", n)
	}
}

func TestSetPassword_RejectsEmptyUsername(t *testing.T) {
	svc, _ := newTestService(t)

	if err := svc.SetPassword(context.Background(), "   ", "a-new-password"); err == nil {
		t.Fatal("empty username accepted; want an error")
	}
}
