package secrets

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

// newTestStore opens a migrated SQLite DB (temp file) and returns a SQLStore
// keyed with a fresh random key. Real SQLite, no mocks, per repo convention.
func newTestStore(t *testing.T) (*SQLStore, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return NewSQLStore(sqlDB, testKey(t)), sqlDB
}

func TestSQLStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	if err := store.Set(ctx, "musixmatch_token", "tok-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := store.Get(ctx, "musixmatch_token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got != "tok-123" {
		t.Fatalf("Get = (%q, %v), want (%q, true)", got, ok, "tok-123")
	}
}

func TestNewSQLStoreCopiesKey(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	key := testKey(t)
	store := NewSQLStore(sqlDB, key)

	// Zero the caller's original key slice after construction. If the store held
	// the slice by reference, this would corrupt its key and break the round-trip.
	for i := range key {
		key[i] = 0
	}

	if err := store.Set(ctx, "k", "plain-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := store.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got != "plain-value" {
		t.Fatalf("Get = (%q, %v), want (%q, true); store key not independent of caller slice", got, ok, "plain-value")
	}
}

func TestSQLStoreCiphertextNotPlaintext(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)
	if err := store.Set(ctx, "webhook_api_key", "plain-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var blob []byte
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT ciphertext FROM secrets WHERE name = ?`, "webhook_api_key").Scan(&blob); err != nil {
		t.Fatalf("scan ciphertext: %v", err)
	}
	if string(blob) == "plain-value" {
		t.Fatal("stored ciphertext equals plaintext")
	}
}

func TestSQLStoreUpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)

	if err := store.Set(ctx, "k", "first"); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	var firstUpdated string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT updated_at FROM secrets WHERE name = ?`, "k").Scan(&firstUpdated); err != nil {
		t.Fatalf("scan updated_at: %v", err)
	}

	// Force a strictly later timestamp so the advance is observable despite the
	// 1-second strftime resolution.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE secrets SET updated_at = '2000-01-01T00:00:00Z' WHERE name = ?`, "k"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := store.Set(ctx, "k", "second"); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	got, ok, err := store.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	if got != "second" {
		t.Fatalf("Get = %q, want %q (upsert did not overwrite)", got, "second")
	}

	// Exactly one row for the name (upsert, not insert).
	var count int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secrets WHERE name = ?`, "k").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}

	var secondUpdated string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT updated_at FROM secrets WHERE name = ?`, "k").Scan(&secondUpdated); err != nil {
		t.Fatalf("scan updated_at: %v", err)
	}
	if secondUpdated <= "2000-01-01T00:00:00Z" {
		t.Fatalf("updated_at did not advance on re-set: %q", secondUpdated)
	}
}

func TestSQLStoreGetAbsentNotFound(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	got, ok, err := store.Get(ctx, "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("Get absent = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestSQLStoreDelete(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	if err := store.Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "k"); ok {
		t.Fatal("secret present after Delete")
	}
	// Deleting an absent name is a no-op.
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

func TestSQLStoreSetEmptyNameFails(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	if err := store.Set(ctx, "", "v"); err == nil {
		t.Fatal("Set with empty name succeeded; want error")
	}
}

func TestSQLStoreWrongKeyGetFails(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)
	if err := store.Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A store with a different key cannot decrypt existing ciphertext.
	other := NewSQLStore(sqlDB, testKey(t))
	if _, _, err := other.Get(ctx, "k"); err == nil {
		t.Fatal("Get with wrong key succeeded; want decrypt error")
	}
}

func TestSQLStoreList(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	// Empty store lists nothing.
	infos, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("List empty = %d rows, want 0", len(infos))
	}

	// Insert out of name order; List must return ordered by name and never values.
	if err := store.Set(ctx, NameWebhookAPIKey, "webhook-secret"); err != nil {
		t.Fatalf("Set webhook: %v", err)
	}
	if err := store.Set(ctx, NameMusixmatchToken, "token-secret"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	infos, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List = %d rows, want 2", len(infos))
	}
	if infos[0].Name != NameMusixmatchToken || infos[1].Name != NameWebhookAPIKey {
		t.Fatalf("List order = [%q, %q], want sorted by name", infos[0].Name, infos[1].Name)
	}
	for _, info := range infos {
		if info.UpdatedAt == "" {
			t.Fatalf("List %q has empty updated_at", info.Name)
		}
		// SecretInfo carries no value field; assert names are not the secret values.
		if info.Name == "token-secret" || info.Name == "webhook-secret" {
			t.Fatalf("List leaked a secret value as a name: %q", info.Name)
		}
	}
}

func TestSQLStoreListQueryError(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.List(ctx); err == nil {
		t.Fatal("List on a closed DB: expected an error")
	}
}

func TestSQLStoreDeleteError(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete on a closed DB: expected an error")
	}
}

func TestMemoryStoreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemoryStore()
	if _, err := store.List(ctx); err == nil {
		t.Fatal("List with canceled context: expected an error")
	}
	if err := store.Set(ctx, "k", "v"); err == nil {
		t.Fatal("Set with canceled context: expected an error")
	}
	if _, _, err := store.Get(ctx, "k"); err == nil {
		t.Fatal("Get with canceled context: expected an error")
	}
	if err := store.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete with canceled context: expected an error")
	}
}

func TestMemoryStoreList(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	infos, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("List empty = %d rows, want 0", len(infos))
	}

	if err := store.Set(ctx, NameWebhookAPIKey, "wv"); err != nil {
		t.Fatalf("Set webhook: %v", err)
	}
	if err := store.Set(ctx, NameMusixmatchToken, "tv"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	infos, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List = %d rows, want 2", len(infos))
	}
	// Ordering parity with SQLStore: sorted by name.
	if infos[0].Name != NameMusixmatchToken || infos[1].Name != NameWebhookAPIKey {
		t.Fatalf("List order = [%q, %q], want sorted by name", infos[0].Name, infos[1].Name)
	}
	for _, info := range infos {
		if info.UpdatedAt == "" {
			t.Fatalf("List %q has empty updated_at", info.Name)
		}
	}

	// Delete clears both value and updated_at.
	if err := store.Delete(ctx, NameMusixmatchToken); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	infos, _ = store.List(ctx)
	if len(infos) != 1 || infos[0].Name != NameWebhookAPIKey {
		t.Fatalf("List after delete = %+v, want only webhook", infos)
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	var store Store = NewMemoryStore()

	if _, ok, _ := store.Get(ctx, "k"); ok {
		t.Fatal("empty store reported a secret")
	}
	if err := store.Set(ctx, "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := store.Get(ctx, "k")
	if err != nil || !ok || got != "v1" {
		t.Fatalf("Get = (%q, %v, %v), want (v1, true, nil)", got, ok, err)
	}
	if err := store.Set(ctx, "k", "v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if got, _, _ := store.Get(ctx, "k"); got != "v2" {
		t.Fatalf("Get after overwrite = %q, want v2", got)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "k"); ok {
		t.Fatal("secret present after Delete")
	}
	if err := store.Set(ctx, "", "v"); err == nil {
		t.Fatal("Set empty name succeeded; want error")
	}
}
