package commands

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/secrets"
)

// tokenMinter is the seam for obtaining a fresh Musixmatch token.
// musixmatch.TokenMinter satisfies it in production; tests inject a fake.
type tokenMinter interface {
	Mint(ctx context.Context) (string, error)
}

// persistingRenewer mints a replacement token on the explicit hint=renew signal
// and persists it, so the replacement survives a restart exactly as a
// bootstrapped token does.
//
// It is installed ONLY when no operator-supplied token is in play. An operator
// token is never overwritten (AC4), and a deployment that supplies its own
// credential should fail loudly rather than silently swap in a minted one.
type persistingRenewer struct {
	minter tokenMinter
	store  secrets.Store
}

// Renew mints a replacement and persists it. A persist failure is logged at
// ERROR but the token is still returned: the current run recovers, and the log
// says the replacement will not survive a restart.
func (r *persistingRenewer) Renew(ctx context.Context) (string, error) {
	tok, err := r.minter.Mint(ctx)
	if err != nil {
		return "", err
	}
	if err := r.store.Set(ctx, secrets.NameMusixmatchToken, tok); err != nil {
		slog.Error("renewed the musixmatch token but could not persist it; the next start will use the old one",
			"error", err)
	}
	return tok, nil
}

// dropDegenerateStoredToken deletes a stored token that is degenerate (#934),
// called only after a failed startup mint. bootstrapToken reaches the mint only
// when no operator token is in play and resolveTokenWithStore returned nothing
// from the DB, so a stored row at this point is either absent or the degenerate
// token that resolveTokenWithStore discarded. A successful mint overwrites that
// row; a failed one would otherwise leave a known-bad credential in the store.
// This saves no mint: the next start mints either way. A non-degenerate row is
// never touched, and a store error is logged, not fatal.
func dropDegenerateStoredToken(ctx context.Context, store secrets.Store) {
	v, ok, err := store.Get(ctx, secrets.NameMusixmatchToken)
	if err != nil || !ok || !musixmatch.IsDegenerateToken(v) {
		return
	}
	if err := store.Delete(ctx, secrets.NameMusixmatchToken); err != nil {
		slog.Warn("could not delete the degenerate stored musixmatch token after a failed mint", "error", err)
		return
	}
	slog.Info("deleted the degenerate stored musixmatch token after a failed mint (#934)")
}

// bootstrapToken is the LOWEST-precedence token source (#554): it mints a token
// only when every higher tier came up empty, then persists it so the existing
// secret-store tier serves it on the next start.
//
// Precedence and safety rules, in order:
//
//   - An operator-supplied token (CLI / env / TOML) is returned untouched and is
//     NEVER overwritten, matching resolveTokenWithStore's contract.
//   - A token already read from the secret store is returned untouched. This is
//     what makes a restart never re-mint, which matters because the token
//     endpoint rate-limits: #554 measured three rapid mints from one egress all
//     refused. A re-minting restart loop would lock itself out.
//   - A refused mint degrades to no token rather than retrying. Serve mode
//     already handles an absent token by disabling lyrics while keeping the web
//     UI up (#385), so degrading is strictly better than spinning.
//
// A persist failure is logged at ERROR but the freshly minted token is still
// returned: the current run works, and the loud log surfaces that the next start
// will have to mint again.
func bootstrapToken(ctx context.Context, higher string, fromDB bool, store secrets.Store, minter tokenMinter) (token string, minted bool) {
	if strings.TrimSpace(higher) != "" {
		return higher, false
	}
	if fromDB {
		return higher, false
	}
	if store == nil || minter == nil {
		return higher, false
	}

	tok, err := minter.Mint(ctx)
	if err != nil {
		dropDegenerateStoredToken(ctx, store)
		if errors.Is(err, musixmatch.ErrTokenMintRefused) {
			slog.Warn("musixmatch token bootstrap refused (rate limited); continuing without a token",
				"hint", "supply MUSIXMATCH_TOKEN or retry later; do not restart in a loop")
			return higher, false
		}
		if errors.Is(err, musixmatch.ErrClientIdentityRetired) {
			// Not an ordinary mint hiccup: the client identity this build
			// impersonates appears to have stopped working upstream (#934).
			// Nothing is persisted; degrade to no token like a refused mint.
			slog.Error("musixmatch client identity appears retired upstream (the token endpoint issued a degenerate token); continuing without a token -- see internal/musixmatch/token.go currentClientIdentity")
			return higher, false
		}
		slog.Warn("musixmatch token bootstrap failed; continuing without a token", "error", err)
		return higher, false
	}

	if err := store.Set(ctx, secrets.NameMusixmatchToken, tok); err != nil {
		slog.Error("minted a musixmatch token but could not persist it; the next start will mint again",
			"error", err)
		return tok, true
	}
	slog.Info("bootstrapped a musixmatch token and persisted it to the encrypted secret store")
	return tok, true
}
