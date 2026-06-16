package webauth

import (
	"context"
	"errors"
	"testing"
)

func TestUserStoreCreateAndGet(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	created, err := store.CreateUser(ctx, "Admin", "$argon2id$hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created user has empty id")
	}
	if created.Username != "Admin" {
		t.Fatalf("username = %q, want %q", created.Username, "Admin")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatal("created/updated timestamps not populated")
	}

	// GetByUsername is case-insensitive (COLLATE NOCASE).
	got, ok, err := store.GetByUsername(ctx, "ADMIN")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if !ok {
		t.Fatal("GetByUsername (different case) found nothing")
	}
	if got.ID != created.ID || got.PasswordHash != "$argon2id$hash" {
		t.Fatalf("GetByUsername returned %+v, want id %q", got, created.ID)
	}

	gotByID, ok, err := store.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !ok || gotByID.Username != "Admin" {
		t.Fatalf("GetByID returned (%+v, %v)", gotByID, ok)
	}
}

func TestUserStoreHasUsers(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	has, err := store.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers: %v", err)
	}
	if has {
		t.Fatal("HasUsers true on an empty table")
	}

	if _, err := store.CreateUser(ctx, "admin", "$argon2id$hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	has, err = store.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers: %v", err)
	}
	if !has {
		t.Fatal("HasUsers false after a user was created")
	}
}

func TestUserStoreDuplicateUsername(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	if _, err := store.CreateUser(ctx, "admin", "$argon2id$hash"); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	// Same username in a different case must collide (COLLATE NOCASE unique).
	_, err := store.CreateUser(ctx, "ADMIN", "$argon2id$other")
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate CreateUser error = %v, want ErrUserExists", err)
	}
}

func TestUserStoreCreateFirstUser(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	first, err := store.CreateFirstUser(ctx, "admin", "$argon2id$hash")
	if err != nil {
		t.Fatalf("CreateFirstUser (empty table): %v", err)
	}
	if first.ID == "" || first.Username != "admin" {
		t.Fatalf("CreateFirstUser returned %+v", first)
	}

	// A second attempt, even with a different username, must be rejected because a
	// user already exists.
	if _, err := store.CreateFirstUser(ctx, "intruder", "$argon2id$other"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("second CreateFirstUser error = %v, want ErrUserExists", err)
	}

	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("users table has %d rows, want 1", count)
	}
}

func TestUserStoreCreateFirstUserRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	if _, err := store.CreateFirstUser(ctx, "  ", "$argon2id$hash"); err == nil {
		t.Fatal("CreateFirstUser with blank username should error")
	}
	if _, err := store.CreateFirstUser(ctx, "admin", ""); err == nil {
		t.Fatal("CreateFirstUser with empty password hash should error")
	}
}

func TestUserStoreGetMissing(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	if _, ok, err := store.GetByUsername(ctx, "nobody"); err != nil || ok {
		t.Fatalf("GetByUsername(missing) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if _, ok, err := store.GetByID(ctx, "deadbeef"); err != nil || ok {
		t.Fatalf("GetByID(missing) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestUserStoreRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	store := NewSQLUserStore(newTestDB(t))

	if _, err := store.CreateUser(ctx, "  ", "$argon2id$hash"); err == nil {
		t.Fatal("CreateUser with blank username should error")
	}
	if _, err := store.CreateUser(ctx, "admin", ""); err == nil {
		t.Fatal("CreateUser with empty password hash should error")
	}
}
