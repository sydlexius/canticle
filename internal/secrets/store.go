package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Stable secret names used as the `secrets` table primary keys. v1 wires only
// these two; the table is a general store so future credentials reuse it.
const (
	// NameMusixmatchToken is the secret name for the Musixmatch API token.
	NameMusixmatchToken = "musixmatch_token"
	// NameMusixmatchClientIdentity records the client identity (host + app_id)
	// a Musixmatch token was MINTED for by canticle (#934). It describes a
	// canticle-minted token and nothing else: the mint and renewal paths write
	// it next to the token, and every operator path that sets
	// NameMusixmatchToken goes through SetOperatorMusixmatchToken, which deletes
	// it. Absence therefore means "operator-set or pre-upgrade", never "minted".
	NameMusixmatchClientIdentity = "musixmatch_client_identity"
	// NameWebhookAPIKey is the secret name for the serve-mode webhook API key.
	NameWebhookAPIKey = "webhook_api_key" //nolint:gosec // G101: this is a stable secret-store row name (a lookup key), not a hardcoded credential value
)

// SecretInfo is the non-sensitive metadata for one stored secret. It carries the
// name and its last-write timestamp only; it never carries the value (plaintext
// or ciphertext), so it is safe to print in `secrets list`.
type SecretInfo struct {
	Name      string
	UpdatedAt string
}

// Store is the secret repository. Callers Set/Get/Delete plaintext values by
// name; encryption and decryption happen inside the implementation so callers
// never see ciphertext or the key. Get reports absence via ok=false (no error).
//
// List returns metadata (name + updated_at) for every stored secret, ordered by
// name. It never returns values, so a caller listing secrets cannot leak them.
type Store interface {
	Set(ctx context.Context, name, plaintext string) error
	Get(ctx context.Context, name string) (plaintext string, ok bool, err error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]SecretInfo, error)
}

// TokenWriter is the subset of Store SetOperatorMusixmatchToken needs, so a
// caller holding a narrower setter (web onboarding) can use it too.
type TokenWriter interface {
	Set(ctx context.Context, name, plaintext string) error
	Delete(ctx context.Context, name string) error
}

// TokenPairWriter is implemented by a store that can write the Musixmatch token
// and its client-identity record as ONE atomic operation (#934). identity ""
// means "delete the record". SQLStore does it in one SQLite transaction and
// MemoryStore under one lock, so a concurrent operator save and minted-token
// renewal cannot interleave into a mismatched pair, and a failed write leaves
// both names as they were.
type TokenPairWriter interface {
	SetTokenWithIdentity(ctx context.Context, token, identity string) error
}

// SetOperatorMusixmatchToken stores an OPERATOR-supplied Musixmatch token (web
// settings, onboarding, `secrets import`, `secrets set`) and clears the
// client-identity record, so the pair cannot drift: the identity record only
// ever describes a token canticle minted (#934).
func SetOperatorMusixmatchToken(ctx context.Context, s TokenWriter, token string) error {
	return SetMusixmatchTokenWithIdentity(ctx, s, token, "")
}

// SetMusixmatchTokenWithIdentity writes the token and its identity record (""
// deletes the record). A TokenPairWriter does both atomically; any other store
// falls back to sequential writes, logged at WARN because that path has no
// atomicity:
//   - identity "": the record is deleted FIRST and the token is not written if
//     that fails, so an operator token never sits beside a stale record.
//   - identity set: an identity write failure does not stop the token write;
//     the record is deleted instead (best effort) so the token reads as
//     absent-identity and is kept, rather than discarded and re-minted on every
//     start by a leftover mismatched record. Only the token write's error is
//     returned.
func SetMusixmatchTokenWithIdentity(ctx context.Context, s TokenWriter, token, identity string) error {
	if pw, ok := s.(TokenPairWriter); ok {
		return pw.SetTokenWithIdentity(ctx, token, identity)
	}
	slog.Warn("secret store cannot write the musixmatch token and its client identity atomically; writing them sequentially")
	if identity == "" {
		if err := s.Delete(ctx, NameMusixmatchClientIdentity); err != nil {
			return fmt.Errorf("secrets: clear musixmatch client identity: %w", err)
		}
		return s.Set(ctx, NameMusixmatchToken, token)
	}
	if err := s.Set(ctx, NameMusixmatchClientIdentity, identity); err != nil {
		slog.Error("could not record the client identity of a minted musixmatch token; it will be treated as operator-set until the next mint",
			"error", err)
		if derr := s.Delete(ctx, NameMusixmatchClientIdentity); derr != nil {
			slog.Error("could not clear a previous musixmatch client identity record either; the next start may re-mint",
				"error", derr)
		}
	}
	return s.Set(ctx, NameMusixmatchToken, token)
}

// SQLStore persists secrets encrypted-at-rest in the SQLite `secrets` table.
// It holds the 32-byte master key in memory and seals/opens each value with
// AES-256-GCM (AAD bound to the secret name).
type SQLStore struct {
	db  *sql.DB
	key []byte
}

// NewSQLStore returns a SQL-backed secret store using key for AES-256-GCM. key
// must be 32 bytes; an invalid key surfaces at Set/Get time. The key is copied
// internally, so a later mutation or zeroing of the caller's slice does not
// affect the store's effective key.
func NewSQLStore(db *sql.DB, key []byte) *SQLStore {
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return &SQLStore{db: db, key: keyCopy}
}

// upsertSecretSQL inserts or replaces one secret row, refreshing updated_at.
//
//nolint:gosec // reason: G101 false positive; this is SQL statement text naming the secrets table, not a credential value
const upsertSecretSQL = `INSERT INTO secrets (name, ciphertext, updated_at)
         VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
         ON CONFLICT(name) DO UPDATE SET
             ciphertext = excluded.ciphertext,
             updated_at = excluded.updated_at`

// Set encrypts plaintext and upserts it under name, refreshing updated_at.
func (s *SQLStore) Set(ctx context.Context, name, plaintext string) error {
	if name == "" {
		return errors.New("secrets: name must not be empty")
	}
	blob, err := Encrypt(s.key, []byte(plaintext), name)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, upsertSecretSQL, name, blob)
	if err != nil {
		return fmt.Errorf("secrets: set %q: %w", name, err)
	}
	return nil
}

// SetTokenWithIdentity writes the Musixmatch token and its client-identity
// record in ONE transaction (identity "" deletes the record). Either both
// changes commit or neither does.
func (s *SQLStore) SetTokenWithIdentity(ctx context.Context, token, identity string) (err error) {
	tokBlob, err := Encrypt(s.key, []byte(token), NameMusixmatchToken)
	if err != nil {
		return err
	}
	var idBlob []byte
	if identity != "" {
		if idBlob, err = Encrypt(s.key, []byte(identity), NameMusixmatchClientIdentity); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("secrets: begin musixmatch token write: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if identity == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, NameMusixmatchClientIdentity)
	} else {
		_, err = tx.ExecContext(ctx, upsertSecretSQL, NameMusixmatchClientIdentity, idBlob)
	}
	if err != nil {
		return fmt.Errorf("secrets: write musixmatch client identity: %w", err)
	}
	if _, err = tx.ExecContext(ctx, upsertSecretSQL, NameMusixmatchToken, tokBlob); err != nil {
		return fmt.Errorf("secrets: set %q: %w", NameMusixmatchToken, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("secrets: commit musixmatch token write: %w", err)
	}
	return nil
}

// SetTokenWithIdentity is MemoryStore's TokenPairWriter: both names change
// under one lock, so no reader or writer observes half the pair.
func (s *MemoryStore) SetTokenWithIdentity(ctx context.Context, token, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if identity == "" {
		delete(s.secrets, NameMusixmatchClientIdentity)
		delete(s.updatedAt, NameMusixmatchClientIdentity)
	} else {
		s.secrets[NameMusixmatchClientIdentity] = identity
		s.updatedAt[NameMusixmatchClientIdentity] = now
	}
	s.secrets[NameMusixmatchToken] = token
	s.updatedAt[NameMusixmatchToken] = now
	return nil
}

// Get returns the decrypted plaintext for name. ok is false when no such secret
// exists; a decryption failure (tampering, wrong key) is returned as an error.
func (s *SQLStore) Get(ctx context.Context, name string) (string, bool, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM secrets WHERE name = ?`, name,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("secrets: get %q: %w", name, err)
	}
	plaintext, err := Decrypt(s.key, blob, name)
	if err != nil {
		return "", false, err
	}
	return string(plaintext), true, nil
}

// Delete removes the secret named name. Deleting an absent name is a no-op.
func (s *SQLStore) Delete(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name); err != nil {
		return fmt.Errorf("secrets: delete %q: %w", name, err)
	}
	return nil
}

// List returns name + updated_at for every stored secret, ordered by name. It
// reads no ciphertext and decrypts nothing, so it cannot leak secret values.
func (s *SQLStore) List(ctx context.Context) ([]SecretInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("secrets: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SecretInfo
	for rows.Next() {
		var info SecretInfo
		if err := rows.Scan(&info.Name, &info.UpdatedAt); err != nil {
			return nil, fmt.Errorf("secrets: list scan: %w", err)
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets: list rows: %w", err)
	}
	return out, nil
}
