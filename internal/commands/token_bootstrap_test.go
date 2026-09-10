package commands

import (
	"context"
	"errors"
	"testing"

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
