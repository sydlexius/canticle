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

// TestRenewerNeverOverwritesOperatorSavedToken pins #942: a renewer installed
// at startup must not replace a token the operator saved afterwards (the web
// settings/onboarding path, secrets.SetOperatorMusixmatchToken), and the
// operator token and its absent identity record survive the renewal hint.
func TestRenewerNeverOverwritesOperatorSavedToken(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewMemoryStore()
	m := &fakeMinter{token: "minted-tok"}
	r := &persistingRenewer{minter: m, store: store}

	if err := secrets.SetOperatorMusixmatchToken(ctx, store, "operator-tok"); err != nil {
		t.Fatal(err)
	}
	tok, err := r.Renew(ctx)
	if !errors.Is(err, errOperatorTokenStored) || tok != "" {
		t.Fatalf("Renew = (%q, %v), want (\"\", errOperatorTokenStored)", tok, err)
	}
	if m.calls != 0 {
		t.Errorf("minter called %d times, want 0", m.calls)
	}
	if v, _, _ := store.Get(ctx, secrets.NameMusixmatchToken); v != "operator-tok" {
		t.Errorf("stored token = %q, want operator-tok", v)
	}
	if _, ok, _ := store.Get(ctx, secrets.NameMusixmatchClientIdentity); ok {
		t.Error("an identity record was written beside the operator token")
	}
}

// TestRenewerStillRenewsMintedAndLegacyTokens pins that the #942 guard does
// not disable renewal for the tokens it is meant to replace: a token canticle
// minted (identity record present), a pre-#934 token the process started with
// (no record, equals legacyToken), and an empty store.
func TestRenewerStillRenewsMintedAndLegacyTokens(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(secrets.Store) bool{
		"empty store": func(secrets.Store) bool { return false },
		"minted": func(s secrets.Store) bool {
			_ = secrets.SetMusixmatchTokenWithIdentity(ctx, s, "old-minted", musixmatch.ClientIdentityKey())
			return false
		},
		"legacy": func(s secrets.Store) bool {
			_ = s.Set(ctx, secrets.NameMusixmatchToken, "legacy-tok")
			return true
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			store := secrets.NewMemoryStore()
			r := &persistingRenewer{minter: &fakeMinter{token: "minted-tok"}, store: store}
			if seed(store) {
				r.legacy = startupLegacyState(t, store)
			}
			tok, err := r.Renew(ctx)
			if err != nil || tok != "minted-tok" {
				t.Fatalf("Renew = (%q, %v), want (minted-tok, nil)", tok, err)
			}
			if v, _, _ := store.Get(ctx, secrets.NameMusixmatchToken); v != "minted-tok" {
				t.Errorf("stored token = %q, want minted-tok", v)
			}
		})
	}
}

// startupLegacyState captures the legacy state the way runServe does.
func startupLegacyState(t *testing.T, store secrets.Store) *secrets.MusixmatchTokenState {
	t.Helper()
	st, err := secrets.ReadMusixmatchTokenState(context.Background(), store)
	if err != nil || !st.HasToken || st.HasIdentity || st.HasStamp {
		t.Fatalf("seeded state %+v (err %v) is not the legacy shape", st, err)
	}
	return &st
}

// renewerStores runs f against the production SQLite store and the in-memory
// store, which must behave the same.
func renewerStores(t *testing.T, f func(t *testing.T, store secrets.Store)) {
	t.Run("sqlite", func(t *testing.T) { f(t, newSecretStore(t)) })
	t.Run("memory", func(t *testing.T) { f(t, secrets.NewMemoryStore()) })
}

// assertOperatorTokenKept checks the stored token is want with no identity.
func assertOperatorTokenKept(t *testing.T, store secrets.Store, want string) {
	t.Helper()
	ctx := context.Background()
	if v, _, _ := store.Get(ctx, secrets.NameMusixmatchToken); v != want {
		t.Errorf("stored token = %q, want the operator's %q", v, want)
	}
	if _, ok, _ := store.Get(ctx, secrets.NameMusixmatchClientIdentity); ok {
		t.Error("an identity record was written beside the operator token")
	}
}

// TestRenewerRefusesOperatorResaveOfLegacyToken pins #942 review finding 1: an
// operator re-saving the byte-identical startup legacy token through the
// operator path is an operator credential, not the untouched legacy token, so
// the renewer must not mint over it.
func TestRenewerRefusesOperatorResaveOfLegacyToken(t *testing.T) {
	renewerStores(t, func(t *testing.T, store secrets.Store) {
		ctx := context.Background()
		seedToken(t, store, "legacy-tok", "")
		m := &fakeMinter{token: "minted-tok"}
		r := &persistingRenewer{minter: m, store: store, legacy: startupLegacyState(t, store)}

		if err := secrets.SetOperatorMusixmatchToken(ctx, store, "legacy-tok"); err != nil {
			t.Fatal(err)
		}
		tok, err := r.Renew(ctx)
		if !errors.Is(err, errOperatorTokenStored) || tok != "" {
			t.Fatalf("Renew = (%q, %v), want (\"\", errOperatorTokenStored)", tok, err)
		}
		if m.calls != 0 {
			t.Errorf("minter called %d times, want 0", m.calls)
		}
		assertOperatorTokenKept(t, store, "legacy-tok")
	})
}

// savingMinter performs an operator save from inside Mint: the save lands
// after Renew's guard has passed and before its persist runs.
type savingMinter struct {
	store secrets.Store
	save  string
	calls int
}

func (m *savingMinter) Mint(ctx context.Context) (string, error) {
	m.calls++
	if err := secrets.SetOperatorMusixmatchToken(ctx, m.store, m.save); err != nil {
		return "", err
	}
	return "minted-tok", nil
}

// TestRenewerDropsMintWhenOperatorSavesMidRenewal pins #942 review finding 2:
// an operator save that lands between the guard and the persist is not
// overwritten; the minted token is dropped. The "same value" case saves the
// token the guard already read, so only the write stamp can tell them apart.
func TestRenewerDropsMintWhenOperatorSavesMidRenewal(t *testing.T) {
	cases := map[string]struct {
		legacy bool
		save   string
	}{
		"minted, new value":  {save: "operator-tok"},
		"minted, same value": {save: "old-tok"},
		"legacy, new value":  {legacy: true, save: "operator-tok"},
		"legacy, same value": {legacy: true, save: "old-tok"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			renewerStores(t, func(t *testing.T, store secrets.Store) {
				ctx := context.Background()
				m := &savingMinter{store: store, save: tc.save}
				r := &persistingRenewer{minter: m, store: store}
				if tc.legacy {
					seedToken(t, store, "old-tok", "")
					r.legacy = startupLegacyState(t, store)
				} else if err := secrets.SetMusixmatchTokenWithIdentity(ctx, store, "old-tok", musixmatch.ClientIdentityKey()); err != nil {
					t.Fatal(err)
				}

				tok, err := r.Renew(ctx)
				if !errors.Is(err, errOperatorTokenStored) || tok != "" {
					t.Fatalf("Renew = (%q, %v), want (\"\", errOperatorTokenStored)", tok, err)
				}
				if m.calls != 1 {
					t.Fatalf("minter called %d times, want 1 (the save must land mid-renewal)", m.calls)
				}
				assertOperatorTokenKept(t, store, tc.save)
			})
		})
	}
}
