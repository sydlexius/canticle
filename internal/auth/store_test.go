package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/db"
)

func TestSQLStore_CreateListFindAndRevoke(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	store := NewSQLStore(sqlDB)
	createdAt := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	key := Key{
		ID:        "key-id",
		Name:      "webhook",
		Hash:      "0123456789abcdef",
		Scopes:    []Scope{ScopeWebhook},
		CreatedAt: createdAt,
	}
	if err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.FindByHash(ctx, key.Hash)
	if err != nil {
		t.Fatalf("FindByHash: %v", err)
	}
	if got.ID != key.ID || got.Name != key.Name || got.Hash != key.Hash {
		t.Fatalf("found key = %+v; want %+v", got, key)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != ScopeWebhook {
		t.Fatalf("scopes = %+v; want webhook", got.Scopes)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %s; want %s", got.CreatedAt, createdAt)
	}

	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != key.ID {
		t.Fatalf("List = %+v; want created key", keys)
	}

	revokedAt := time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC)
	revoked, err := store.RevokeByHash(ctx, key.Hash, revokedAt)
	if err != nil {
		t.Fatalf("RevokeByHash: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("RevokedAt = %v; want %s", revoked.RevokedAt, revokedAt)
	}
}

func TestSQLStore_RevokeByID(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	store := NewSQLStore(sqlDB)
	key := Key{
		ID:        "key-id",
		Name:      "webhook",
		Hash:      "0123456789abcdef",
		Scopes:    []Scope{ScopeWebhook},
		CreatedAt: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	revokedAt := time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC)
	revoked, err := store.RevokeByID(ctx, key.ID, revokedAt)
	if err != nil {
		t.Fatalf("RevokeByID: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("RevokedAt = %v; want %s", revoked.RevokedAt, revokedAt)
	}
	// The persisted row reflects the revocation.
	got, err := store.FindByHash(ctx, key.Hash)
	if err != nil {
		t.Fatalf("FindByHash: %v", err)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(revokedAt) {
		t.Fatalf("persisted RevokedAt = %v; want %s", got.RevokedAt, revokedAt)
	}

	if _, err := store.RevokeByID(ctx, "missing-id", revokedAt); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("RevokeByID missing error = %v; want ErrInvalidKey", err)
	}

	// Idempotent: a second revoke at a LATER time preserves the original
	// revoked_at rather than re-stamping it.
	later := revokedAt.Add(time.Hour)
	again, err := store.RevokeByID(ctx, key.ID, later)
	if err != nil {
		t.Fatalf("RevokeByID (second) : %v", err)
	}
	if again.RevokedAt == nil || !again.RevokedAt.Equal(revokedAt) {
		t.Fatalf("re-revoke RevokedAt = %v; want preserved original %s", again.RevokedAt, revokedAt)
	}
}

func TestSQLStore_InvalidRows(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	store := NewSQLStore(sqlDB)
	if err := store.Create(ctx, Key{}); err == nil {
		t.Fatal("Create empty hash returned nil error")
	}
	if _, err := store.FindByHash(ctx, "missing"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("FindByHash missing error = %v; want ErrInvalidKey", err)
	}
	if _, err := store.RevokeByHash(ctx, "missing", time.Now()); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("RevokeByHash missing error = %v; want ErrInvalidKey", err)
	}
}

func TestSQLStore_CreateIfNotExistsIgnoresDuplicateHash(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	store := NewSQLStore(sqlDB)
	key := Key{
		ID:        "key-id",
		Name:      "webhook",
		Hash:      "0123456789abcdef",
		Scopes:    []Scope{ScopeWebhook},
		CreatedAt: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}
	inserted, err := store.CreateIfNotExists(ctx, key)
	if err != nil {
		t.Fatalf("CreateIfNotExists first: %v", err)
	}
	if !inserted {
		t.Fatal("CreateIfNotExists first inserted = false; want true")
	}
	inserted, err = store.CreateIfNotExists(ctx, key)
	if err != nil {
		t.Fatalf("CreateIfNotExists duplicate: %v", err)
	}
	if inserted {
		t.Fatal("CreateIfNotExists duplicate inserted = true; want false")
	}
}
