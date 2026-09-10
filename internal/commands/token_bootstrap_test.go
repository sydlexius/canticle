package commands

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/secrets"
)

type fakeMinter struct {
	token string
	err   error
	calls int
}

func (f *fakeMinter) Mint(context.Context) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// failingStore is a secrets.Store whose Set always fails, to exercise the
// persist-failure path without a broken database.
type failingStore struct {
	secrets.Store
}

func (failingStore) Set(context.Context, string, string) error {
	return errors.New("disk full")
}

// TestBootstrapToken_OperatorTokenIsNeverOverwritten pins AC4: a supplied token
// short-circuits before minting, and nothing is persisted.
func TestBootstrapToken_OperatorTokenIsNeverOverwritten(t *testing.T) {
	store := secrets.NewMemoryStore()
	m := &fakeMinter{token: "minted"}

	got, minted := bootstrapToken(context.Background(), "operator-token", false, store, m)

	if got != "operator-token" {
		t.Errorf("token = %q; want the operator token", got)
	}
	if minted {
		t.Error("minted = true; want false")
	}
	if m.calls != 0 {
		t.Errorf("minter called %d times; want 0", m.calls)
	}
	if _, ok, _ := store.Get(context.Background(), secrets.NameMusixmatchToken); ok {
		t.Error("a token was persisted; an operator-supplied token must never be written")
	}
}

// TestBootstrapToken_StoredTokenIsNotReminted pins AC2, the constraint that
// keeps a restart loop from locking itself out of the mint endpoint.
func TestBootstrapToken_StoredTokenIsNotReminted(t *testing.T) {
	m := &fakeMinter{token: "minted"}

	got, minted := bootstrapToken(context.Background(), "stored-token", true, secrets.NewMemoryStore(), m)

	if got != "stored-token" {
		t.Errorf("token = %q; want the stored token", got)
	}
	if minted {
		t.Error("minted = true; want false")
	}
	if m.calls != 0 {
		t.Errorf("minter called %d times; want 0 (a restart must never re-mint)", m.calls)
	}
}

// TestBootstrapToken_MintsAndPersists pins AC1.
func TestBootstrapToken_MintsAndPersists(t *testing.T) {
	store := secrets.NewMemoryStore()
	m := &fakeMinter{token: "fresh-token"}

	got, minted := bootstrapToken(context.Background(), "", false, store, m)

	if got != "fresh-token" {
		t.Errorf("token = %q; want the minted token", got)
	}
	if !minted {
		t.Error("minted = false; want true")
	}
	if m.calls != 1 {
		t.Errorf("minter called %d times; want 1", m.calls)
	}
	v, ok, err := store.Get(context.Background(), secrets.NameMusixmatchToken)
	if err != nil || !ok {
		t.Fatalf("token not persisted (ok=%v err=%v)", ok, err)
	}
	if v != "fresh-token" {
		t.Errorf("persisted %q; want the minted token", v)
	}
}

// TestBootstrapToken_ClientIdentityRetiredDegradesWithoutRetry: a degenerate
// token from an apparently-retired client identity (#934) degrades to no
// token, exactly like a rate-limited refusal -- one call, nothing persisted.
func TestBootstrapToken_ClientIdentityRetiredDegradesWithoutRetry(t *testing.T) {
	store := secrets.NewMemoryStore()
	m := &fakeMinter{err: musixmatch.ErrClientIdentityRetired}

	got, minted := bootstrapToken(context.Background(), "", false, store, m)

	if got != "" || minted {
		t.Errorf("got (%q, %v); want (\"\", false)", got, minted)
	}
	if m.calls != 1 {
		t.Errorf("minter called %d times; want exactly 1 (no retry loop)", m.calls)
	}
	if _, ok, _ := store.Get(context.Background(), secrets.NameMusixmatchToken); ok {
		t.Error("something was persisted after a client-identity-retired mint failure")
	}
}

// identityFailingStore fails every write of the client-identity record and
// passes everything else through, to exercise #934 finding 2.
type identityFailingStore struct {
	secrets.Store
}

func (s identityFailingStore) Set(ctx context.Context, name, v string) error {
	if name == secrets.NameMusixmatchClientIdentity {
		return errors.New("identity write refused")
	}
	return s.Store.Set(ctx, name, v)
}

// TestMintedTokenPersistsClientIdentity pins that both mint paths (bootstrap
// and hint=renew renewal) record the identity a token was minted for.
func TestMintedTokenPersistsClientIdentity(t *testing.T) {
	ctx := context.Background()
	for name, mint := range map[string]func(secrets.Store){
		"bootstrap": func(s secrets.Store) { bootstrapToken(ctx, "", false, s, &fakeMinter{token: "minted-tok"}) },
		"renew": func(s secrets.Store) {
			_, _ = (&persistingRenewer{minter: &fakeMinter{token: "minted-tok"}, store: s}).Renew(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := secrets.NewMemoryStore()
			mint(store)
			if v, _, _ := store.Get(ctx, secrets.NameMusixmatchToken); v != "minted-tok" {
				t.Fatalf("token = %q, want minted-tok", v)
			}
			if v, _, _ := store.Get(ctx, secrets.NameMusixmatchClientIdentity); v != musixmatch.ClientIdentityKey() {
				t.Fatalf("identity = %q, want %q", v, musixmatch.ClientIdentityKey())
			}
		})
	}
}

// TestIdentityWriteFailureDoesNotMintEveryStart pins #934 finding 2: when the
// identity record can never be written, the startup chain (resolve ->
// bootstrap) must mint ONCE and then reuse the token, not mint on every start.
// The seeded mismatched record is the hard case: left in place, it would get
// every freshly minted token discarded on the next start.
func TestIdentityWriteFailureDoesNotMintEveryStart(t *testing.T) {
	ctx := context.Background()
	mem := secrets.NewMemoryStore()
	_ = mem.Set(ctx, secrets.NameMusixmatchClientIdentity, "retired.example.com|old-app-v1.0")
	_ = mem.Set(ctx, secrets.NameMusixmatchToken, "old-identity-tok")
	store := identityFailingStore{Store: mem}
	m := &fakeMinter{token: "minted-tok"}

	for start := 1; start <= 3; start++ {
		tok, fromDB, err := resolveTokenWithStore(ctx, "", store)
		if err != nil {
			t.Fatalf("start %d: resolveTokenWithStore: %v", start, err)
		}
		tok, _ = bootstrapToken(ctx, tok, fromDB, store, m)
		if tok != "minted-tok" {
			t.Fatalf("start %d: token = %q, want minted-tok", start, tok)
		}
	}
	if m.calls != 1 {
		t.Fatalf("minter called %d times across 3 starts; want exactly 1", m.calls)
	}
}

// TestBootstrapToken_RefusedMintDegrades verifies a rate-limited mint yields no
// token and no retry, rather than spinning against the endpoint.
func TestBootstrapToken_RefusedMintDegrades(t *testing.T) {
	store := secrets.NewMemoryStore()
	m := &fakeMinter{err: musixmatch.ErrTokenMintRefused}

	got, minted := bootstrapToken(context.Background(), "", false, store, m)

	if got != "" {
		t.Errorf("token = %q; want empty", got)
	}
	if minted {
		t.Error("minted = true; want false")
	}
	if m.calls != 1 {
		t.Errorf("minter called %d times; want exactly 1 (no retry loop)", m.calls)
	}
	if _, ok, _ := store.Get(context.Background(), secrets.NameMusixmatchToken); ok {
		t.Error("something was persisted after a refused mint")
	}
}

func TestBootstrapToken_MintErrorDegrades(t *testing.T) {
	m := &fakeMinter{err: errors.New("network unreachable")}

	got, minted := bootstrapToken(context.Background(), "", false, secrets.NewMemoryStore(), m)

	if got != "" || minted {
		t.Errorf("got (%q, %v); want (\"\", false)", got, minted)
	}
}

// TestBootstrapToken_PersistFailureStillReturnsToken keeps the current run
// working when the store cannot be written; the ERROR log carries the warning
// that the next start will mint again.
func TestBootstrapToken_PersistFailureStillReturnsToken(t *testing.T) {
	m := &fakeMinter{token: "fresh-token"}

	got, minted := bootstrapToken(context.Background(), "", false, failingStore{secrets.NewMemoryStore()}, m)

	if got != "fresh-token" {
		t.Errorf("token = %q; want the minted token despite the persist failure", got)
	}
	if !minted {
		t.Error("minted = false; want true")
	}
}

func TestBootstrapToken_NilStoreOrMinterIsANoOp(t *testing.T) {
	if got, minted := bootstrapToken(context.Background(), "", false, nil, &fakeMinter{token: "x"}); got != "" || minted {
		t.Errorf("nil store: got (%q, %v); want (\"\", false)", got, minted)
	}
	if got, minted := bootstrapToken(context.Background(), "", false, secrets.NewMemoryStore(), nil); got != "" || minted {
		t.Errorf("nil minter: got (%q, %v); want (\"\", false)", got, minted)
	}
}

// TestPersistMintedTokenIsAtomic pins #934 round-2 fix 2 on the production
// store: a failed token write during a mint or renewal persist leaves the
// previous identity record UNCHANGED, rather than pairing a current record
// with the old token.
func TestPersistMintedTokenIsAtomic(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	store := secrets.NewSQLStore(sqlDB, bytes.Repeat([]byte{0x42}, 32))
	seedToken(t, store, "old-tok", "retired.example.com|old-app-v1.0")
	if _, err := sqlDB.Exec(`CREATE TRIGGER fail_token BEFORE UPDATE ON secrets
		WHEN NEW.name = '` + secrets.NameMusixmatchToken + `' BEGIN SELECT RAISE(ABORT, 'token write refused'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := persistMintedToken(ctx, store, "minted-tok"); err == nil {
		t.Fatal("persistMintedToken succeeded; want the token write's error")
	}
	if v, _, _ := store.Get(ctx, secrets.NameMusixmatchToken); v != "old-tok" {
		t.Errorf("token = %q, want old-tok", v)
	}
	if v, _, _ := store.Get(ctx, secrets.NameMusixmatchClientIdentity); v != "retired.example.com|old-app-v1.0" {
		t.Errorf("identity = %q, want the previous record: a failed token write must roll the record back", v)
	}
}
