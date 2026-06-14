package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Stable secret names used as the `secrets` table primary keys. v1 wires only
// these two; the table is a general store so future credentials reuse it.
const (
	// NameMusixmatchToken is the secret name for the Musixmatch API token.
	NameMusixmatchToken = "musixmatch_token"
	// NameWebhookAPIKey is the secret name for the serve-mode webhook API key.
	NameWebhookAPIKey = "webhook_api_key" //nolint:gosec // G101: this is a stable secret-store row name (a lookup key), not a hardcoded credential value
)

// Store is the secret repository. Callers Set/Get/Delete plaintext values by
// name; encryption and decryption happen inside the implementation so callers
// never see ciphertext or the key. Get reports absence via ok=false (no error).
type Store interface {
	Set(ctx context.Context, name, plaintext string) error
	Get(ctx context.Context, name string) (plaintext string, ok bool, err error)
	Delete(ctx context.Context, name string) error
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

// Set encrypts plaintext and upserts it under name, refreshing updated_at.
func (s *SQLStore) Set(ctx context.Context, name, plaintext string) error {
	if name == "" {
		return errors.New("secrets: name must not be empty")
	}
	blob, err := Encrypt(s.key, []byte(plaintext), name)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets (name, ciphertext, updated_at)
         VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
         ON CONFLICT(name) DO UPDATE SET
             ciphertext = excluded.ciphertext,
             updated_at = excluded.updated_at`,
		name, blob,
	)
	if err != nil {
		return fmt.Errorf("secrets: set %q: %w", name, err)
	}
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
