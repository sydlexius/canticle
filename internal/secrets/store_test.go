package secrets

import (
	"context"
	"database/sql"
	"errors"
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

// TestSetOperatorMusixmatchTokenClearsIdentity pins the pairing #934 depends
// on: an operator-set token replaces a minted one AND removes the record that
// said the stored token was minted, so startup never judges the operator token
// by a minted token's identity.
func TestSetOperatorMusixmatchTokenClearsIdentity(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	if err := store.Set(ctx, NameMusixmatchToken, "minted"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, NameMusixmatchClientIdentity, "host|app"); err != nil {
		t.Fatal(err)
	}

	if err := SetOperatorMusixmatchToken(ctx, store, "operator"); err != nil {
		t.Fatalf("SetOperatorMusixmatchToken: %v", err)
	}
	if v, _, _ := store.Get(ctx, NameMusixmatchToken); v != "operator" {
		t.Errorf("token = %q, want operator", v)
	}
	if _, ok, _ := store.Get(ctx, NameMusixmatchClientIdentity); ok {
		t.Error("client identity record survived an operator token write")
	}
}

// scriptedWriter is a TokenWriter whose Delete/Set fail on demand and which
// records the token it was asked to write.
type scriptedWriter struct {
	deleteErr, setErr error
	wrote             map[string]string
}

func (w *scriptedWriter) Delete(context.Context, string) error { return w.deleteErr }

func (w *scriptedWriter) Set(_ context.Context, name, v string) error {
	if w.setErr != nil {
		return w.setErr
	}
	w.wrote[name] = v
	return nil
}

// TestSetOperatorMusixmatchTokenErrors pins the two failure branches: a failed
// identity delete must NOT write the token (it would sit beside a stale
// minted-for record), and a failed token write is surfaced to the caller.
func TestSetOperatorMusixmatchTokenErrors(t *testing.T) {
	ctx := context.Background()
	deleteErr := errors.New("delete refused")
	w := &scriptedWriter{deleteErr: deleteErr, wrote: map[string]string{}}
	if err := SetOperatorMusixmatchToken(ctx, w, "operator"); !errors.Is(err, deleteErr) {
		t.Fatalf("err = %v, want wrapping %v", err, deleteErr)
	}
	if _, ok := w.wrote[NameMusixmatchToken]; ok {
		t.Error("token written although the identity record could not be cleared")
	}

	setErr := errors.New("set refused")
	w = &scriptedWriter{setErr: setErr, wrote: map[string]string{}}
	if err := SetOperatorMusixmatchToken(ctx, w, "operator"); !errors.Is(err, setErr) {
		t.Fatalf("err = %v, want %v", err, setErr)
	}
}

// failTokenWrites installs triggers that abort any insert or update of the
// token row, so the token half of a pair write fails inside SQLite.
func failTokenWrites(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	for _, op := range []string{"INSERT", "UPDATE"} {
		stmt := `CREATE TRIGGER fail_token_` + op + ` BEFORE ` + op + ` ON secrets
			WHEN NEW.name = '` + NameMusixmatchToken + `' BEGIN SELECT RAISE(ABORT, 'token write refused'); END`
		if _, err := sqlDB.Exec(stmt); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}
}

// TestSQLStoreTokenPairWriteIsAtomic pins #934 round-2 fixes 1-3: when the
// token write fails, the identity record is left exactly as it was, both for
// a minted write (record set) and an operator write (record deleted). Two
// independent writes would leave the new record, or no record, beside the old
// token.
func TestSQLStoreTokenPairWriteIsAtomic(t *testing.T) {
	ctx := context.Background()
	for name, write := range map[string]func(*SQLStore) error{
		"minted": func(s *SQLStore) error { return s.SetTokenWithIdentity(ctx, "new-tok", "new|id") },
		"operator": func(s *SQLStore) error {
			return SetOperatorMusixmatchToken(ctx, s, "operator-tok")
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, sqlDB := newTestStore(t)
			if err := store.SetTokenWithIdentity(ctx, "old-tok", "old|id"); err != nil {
				t.Fatal(err)
			}
			failTokenWrites(t, sqlDB)
			if err := write(store); err == nil {
				t.Fatal("pair write succeeded; want the token write's error")
			}
			if v, _, _ := store.Get(ctx, NameMusixmatchToken); v != "old-tok" {
				t.Errorf("token = %q, want old-tok", v)
			}
			if v, ok, _ := store.Get(ctx, NameMusixmatchClientIdentity); !ok || v != "old|id" {
				t.Errorf("identity = (%q, %v), want (old|id, true): a failed token write must roll the record back", v, ok)
			}
		})
	}
}

// TestSQLStoreSetTokenWithIdentity covers the success paths: a record is
// written beside the token, and identity "" deletes it.
func TestSQLStoreSetTokenWithIdentity(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	if err := store.SetTokenWithIdentity(ctx, "tok", "host|app"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.Get(ctx, NameMusixmatchClientIdentity); v != "host|app" {
		t.Fatalf("identity = %q, want host|app", v)
	}
	if err := store.SetTokenWithIdentity(ctx, "tok2", ""); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.Get(ctx, NameMusixmatchToken); v != "tok2" {
		t.Errorf("token = %q, want tok2", v)
	}
	if _, ok, _ := store.Get(ctx, NameMusixmatchClientIdentity); ok {
		t.Error("identity record survived SetTokenWithIdentity with an empty identity")
	}
}

// TestMemoryStoreSetTokenWithIdentity mirrors the SQL success paths.
func TestMemoryStoreSetTokenWithIdentity(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.SetTokenWithIdentity(ctx, "tok", "host|app"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.Get(ctx, NameMusixmatchClientIdentity); v != "host|app" {
		t.Fatalf("identity = %q, want host|app", v)
	}
	if err := SetOperatorMusixmatchToken(ctx, store, "op"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, NameMusixmatchClientIdentity); ok {
		t.Error("identity record survived an operator write")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.SetTokenWithIdentity(cctx, "x", ""); err == nil {
		t.Error("canceled context: want error")
	}
}

// nameWriter is a TokenWriter (no atomic pair method, so it takes the
// sequential fallback) whose writes fail per secret name.
type nameWriter struct {
	failSet, failDelete map[string]bool
	vals                map[string]string
}

func (w *nameWriter) Set(_ context.Context, name, v string) error {
	if w.failSet[name] {
		return errors.New("set refused")
	}
	w.vals[name] = v
	return nil
}

func (w *nameWriter) Delete(_ context.Context, name string) error {
	if w.failDelete[name] {
		return errors.New("delete refused")
	}
	delete(w.vals, name)
	return nil
}

// TestSetMusixmatchTokenWithIdentityFallback pins the sequential fallback for a
// minted write: an identity write failure still stores the token and clears any
// previous record (so the token reads as absent-identity and is kept), and a
// failed clear is logged, not fatal.
func TestSetMusixmatchTokenWithIdentityFallback(t *testing.T) {
	ctx := context.Background()
	w := &nameWriter{failSet: map[string]bool{NameMusixmatchClientIdentity: true},
		vals: map[string]string{NameMusixmatchClientIdentity: "old|id"}}
	if err := SetMusixmatchTokenWithIdentity(ctx, w, "tok", "new|id"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if w.vals[NameMusixmatchToken] != "tok" {
		t.Errorf("token = %q, want tok", w.vals[NameMusixmatchToken])
	}
	if _, ok := w.vals[NameMusixmatchClientIdentity]; ok {
		t.Error("previous identity record survived a failed identity write")
	}

	w = &nameWriter{failSet: map[string]bool{NameMusixmatchClientIdentity: true},
		failDelete: map[string]bool{NameMusixmatchClientIdentity: true}, vals: map[string]string{}}
	if err := SetMusixmatchTokenWithIdentity(ctx, w, "tok", "new|id"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if w.vals[NameMusixmatchToken] != "tok" {
		t.Errorf("token = %q, want tok", w.vals[NameMusixmatchToken])
	}
}

// TestSQLStoreSetTokenWithIdentityErrors covers the transaction's failure
// branches: a refused identity-row write rolls back with the token unchanged,
// a closed DB fails to begin, and an invalid key fails before touching the DB.
func TestSQLStoreSetTokenWithIdentityErrors(t *testing.T) {
	ctx := context.Background()
	store, sqlDB := newTestStore(t)
	if err := store.SetTokenWithIdentity(ctx, "old-tok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`CREATE TRIGGER fail_identity BEFORE INSERT ON secrets
		WHEN NEW.name = '` + NameMusixmatchClientIdentity + `' BEGIN SELECT RAISE(ABORT, 'identity write refused'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := store.SetTokenWithIdentity(ctx, "new-tok", "host|app"); err == nil {
		t.Fatal("want the identity write's error")
	}
	if v, _, _ := store.Get(ctx, NameMusixmatchToken); v != "old-tok" {
		t.Errorf("token = %q, want old-tok", v)
	}

	closed, closedDB := newTestStore(t)
	_ = closedDB.Close()
	if err := closed.SetTokenWithIdentity(ctx, "tok", "host|app"); err == nil {
		t.Error("closed DB: want error")
	}

	badKey := NewSQLStore(sqlDB, []byte("short"))
	if err := badKey.SetTokenWithIdentity(ctx, "tok", "host|app"); err == nil {
		t.Error("invalid key: want error")
	}
}

// TestSetMusixmatchTokenWithIdentityFallbackTokenFailure pins the fallback's
// write ORDER for a minted write: the token is written before the identity
// record, so a failed token write leaves the previous token+identity pair
// untouched. Identity-first would leave the OLD token beside the NEW identity,
// which startup accepts as minted for the current identity.
func TestSetMusixmatchTokenWithIdentityFallbackTokenFailure(t *testing.T) {
	ctx := context.Background()
	w := &nameWriter{failSet: map[string]bool{NameMusixmatchToken: true},
		vals: map[string]string{NameMusixmatchToken: "old-tok", NameMusixmatchClientIdentity: "old|id"}}
	if err := SetMusixmatchTokenWithIdentity(ctx, w, "new-tok", "new|id"); err == nil {
		t.Fatal("err = nil, want the token write's error")
	}
	if v := w.vals[NameMusixmatchClientIdentity]; v != "old|id" {
		t.Errorf("identity = %q after a failed token write, want old|id (untouched)", v)
	}
	if v := w.vals[NameMusixmatchToken]; v != "old-tok" {
		t.Errorf("token = %q, want old-tok", v)
	}

	// No previous record: a failed token write must not create one.
	w = &nameWriter{failSet: map[string]bool{NameMusixmatchToken: true}, vals: map[string]string{}}
	if err := SetMusixmatchTokenWithIdentity(ctx, w, "new-tok", "new|id"); err == nil {
		t.Fatal("err = nil, want the token write's error")
	}
	if v, ok := w.vals[NameMusixmatchClientIdentity]; ok {
		t.Errorf("identity record %q created beside a failed token write", v)
	}

	// Success path still writes both.
	w = &nameWriter{vals: map[string]string{NameMusixmatchToken: "old-tok", NameMusixmatchClientIdentity: "old|id"}}
	if err := SetMusixmatchTokenWithIdentity(ctx, w, "new-tok", "new|id"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if w.vals[NameMusixmatchToken] != "new-tok" || w.vals[NameMusixmatchClientIdentity] != "new|id" {
		t.Errorf("vals = %v, want new-tok + new|id", w.vals)
	}
}
