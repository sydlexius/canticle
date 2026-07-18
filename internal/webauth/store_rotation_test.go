package webauth

import (
	"context"
	"testing"
)

func TestSetPassword_RejectsEmptyUsername(t *testing.T) {
	svc, _ := newTestService(t)

	if err := svc.SetPassword(context.Background(), "   ", "a-new-password"); err == nil {
		t.Fatal("empty username accepted; want an error")
	}
}

func TestRotateCredential_RejectsEmptyInputs(t *testing.T) {
	_, db := newTestService(t)
	store := NewSQLUserStore(db)
	ctx := context.Background()

	if _, err := store.RotateCredential(ctx, "", "$argon2id$fake"); err == nil {
		t.Error("empty username accepted; want an error")
	}
	if _, err := store.RotateCredential(ctx, "admin", ""); err == nil {
		t.Error("empty hash accepted; want an error")
	}
}

// A rotation against an unusable database must fail cleanly rather than panic
// or half-apply. Closing the handle is the cheapest way to make BeginTx fail.
func TestRotateCredential_UnusableDatabaseErrors(t *testing.T) {
	_, db := newTestService(t)
	store := NewSQLUserStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := store.RotateCredential(context.Background(), "admin", "$argon2id$fake"); err == nil {
		t.Fatal("rotation against a closed database returned nil; want an error")
	}
}

func TestRotateCredential_IsCaseInsensitive(t *testing.T) {
	svc, db := newTestService(t)
	store := NewSQLUserStore(db)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "Admin", "initial-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if _, err := store.RotateCredential(ctx, "aDmIn", "$argon2id$rotated"); err != nil {
		t.Fatalf("RotateCredential: %v", err)
	}
	u, ok, err := store.GetByUsername(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("GetByUsername: ok=%v err=%v", ok, err)
	}
	if u.PasswordHash != "$argon2id$rotated" {
		t.Errorf("hash = %q; want the rotated value", u.PasswordHash)
	}
}
