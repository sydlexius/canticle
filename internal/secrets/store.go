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
