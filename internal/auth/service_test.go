package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestService_CreateKeyGeneratesPrefixedKeyAndStoresHash(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store)
	svc.rand = bytes.NewReader(bytes.Repeat([]byte{0x42}, keyBytes))
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	created, err := svc.CreateKey(ctx, "webhook key", []Scope{ScopeWebhook})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !strings.HasPrefix(created.Raw, KeyPrefix) {
		t.Fatalf("raw key = %q; want %s prefix", created.Raw, KeyPrefix)
	}
	if len(strings.TrimPrefix(created.Raw, KeyPrefix)) != 64 {
		t.Fatalf("raw key hex length = %d; want 64", len(strings.TrimPrefix(created.Raw, KeyPrefix)))
	}
	hash, err := HashKey(created.Raw)
	if err != nil {
		t.Fatalf("HashKey: %v", err)
	}
	if created.Key.Hash != hash {
		t.Fatalf("stored hash = %q; want hash of raw key", created.Key.Hash)
	}
	if strings.Contains(created.Key.Hash, created.Raw) {
		t.Fatal("stored hash contains raw key material")
	}
	if !created.Key.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %s; want %s", created.Key.CreatedAt, now)
	}

	keys, err := svc.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("ListKeys returned %d keys; want 1", len(keys))
	}
	if keys[0].Hash != created.Key.Hash {
		t.Fatalf("listed hash = %q; want created hash", keys[0].Hash)
	}
}

func TestService_ValidateKeyScopesAndAdminImplication(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store)
	svc.rand = bytes.NewReader(bytes.Repeat([]byte{0x01}, keyBytes))

	created, err := svc.CreateKey(ctx, "admin", []Scope{ScopeAdmin})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	key, err := svc.ValidateKey(ctx, created.Raw, ScopeWebhook)
	if err != nil {
		t.Fatalf("ValidateKey admin for webhook: %v", err)
	}
	if key.ID != created.Key.ID {
		t.Fatalf("validated key ID = %q; want %q", key.ID, created.Key.ID)
	}

	svc.rand = bytes.NewReader(bytes.Repeat([]byte{0x02}, keyBytes))
	webhook, err := svc.CreateKey(ctx, "webhook", []Scope{ScopeWebhook})
	if err != nil {
		t.Fatalf("CreateKey webhook: %v", err)
	}
	if _, err := svc.ValidateKey(ctx, webhook.Raw, ScopeAdmin); !errors.Is(err, ErrForbiddenScope) {
		t.Fatalf("ValidateKey webhook for admin error = %v; want ErrForbiddenScope", err)
	}
}

func TestService_RevokeKeyPreventsValidation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store)
	svc.rand = bytes.NewReader(bytes.Repeat([]byte{0x03}, keyBytes))
	revokedAt := time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return revokedAt }

	created, err := svc.CreateKey(ctx, "webhook", []Scope{ScopeWebhook})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	revoked, err := svc.RevokeKey(ctx, created.Raw)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("RevokedAt = %v; want %s", revoked.RevokedAt, revokedAt)
	}
	if _, err := svc.ValidateKey(ctx, created.Raw, ScopeWebhook); !errors.Is(err, ErrRevokedKey) {
		t.Fatalf("ValidateKey revoked error = %v; want ErrRevokedKey", err)
	}
}

func TestService_RevokeKeyByID(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store)
	svc.rand = bytes.NewReader(bytes.Repeat([]byte{0x07}, keyBytes))
	revokedAt := time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return revokedAt }

	created, err := svc.CreateKey(ctx, "webhook", []Scope{ScopeWebhook})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	revoked, err := svc.RevokeKeyByID(ctx, created.Key.ID)
	if err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("RevokedAt = %v; want %s", revoked.RevokedAt, revokedAt)
	}
	// Validation by the raw key now fails: revocation by ID reached the same row.
	if _, err := svc.ValidateKey(ctx, created.Raw, ScopeWebhook); !errors.Is(err, ErrRevokedKey) {
		t.Fatalf("ValidateKey after RevokeKeyByID = %v; want ErrRevokedKey", err)
	}

	// Idempotent: re-revoking at a later time preserves the original timestamp.
	svc.now = func() time.Time { return revokedAt.Add(time.Hour) }
	again, err := svc.RevokeKeyByID(ctx, created.Key.ID)
	if err != nil {
		t.Fatalf("RevokeKeyByID (second): %v", err)
	}
	if again.RevokedAt == nil || !again.RevokedAt.Equal(revokedAt) {
		t.Fatalf("re-revoke RevokedAt = %v; want preserved %s", again.RevokedAt, revokedAt)
	}

	// A blank or unknown ID is a clean ErrInvalidKey, never a panic.
	if _, err := svc.RevokeKeyByID(ctx, "  "); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("RevokeKeyByID blank = %v; want ErrInvalidKey", err)
	}
	if _, err := svc.RevokeKeyByID(ctx, "nope"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("RevokeKeyByID unknown = %v; want ErrInvalidKey", err)
	}
}

func TestService_RevokeKeyByIDNilDeps(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Service{}).RevokeKeyByID(ctx, "id"); err == nil {
		t.Fatal("RevokeKeyByID with nil store returned nil error")
	}
	if _, err := (&Service{store: NewMemoryStore()}).RevokeKeyByID(ctx, "id"); err == nil {
		t.Fatal("RevokeKeyByID with nil now returned nil error")
	}
}

func TestService_InvalidKeysAndScopes(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store)

	if _, err := svc.ValidateKey(ctx, "not-mxlrc", ScopeWebhook); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("ValidateKey malformed error = %v; want ErrInvalidKey", err)
	}
	if _, err := svc.CreateKey(ctx, "empty", nil); err == nil {
		t.Fatal("CreateKey with no scopes returned nil error")
	}
	if _, err := svc.CreateKey(ctx, "bad", []Scope{"root"}); err == nil {
		t.Fatal("CreateKey with unsupported scope returned nil error")
	}
}

func TestNormalizeScopesDeduplicatesAndSorts(t *testing.T) {
	got, err := NormalizeScopes([]Scope{ScopeWebhook, ScopeAdmin, ScopeWebhook})
	if err != nil {
		t.Fatalf("NormalizeScopes: %v", err)
	}
	want := []Scope{ScopeAdmin, ScopeWebhook}
	if len(got) != len(want) {
		t.Fatalf("scopes = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %+v; want %+v", got, want)
		}
	}
}

func TestMemoryStoreRejectsDuplicateHashes(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	key := Key{ID: "id", Hash: "hash", Scopes: []Scope{ScopeWebhook}}
	if err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if err := store.Create(ctx, key); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("Create duplicate error = %v; want ErrDuplicateKey", err)
	}
}
