package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLStore stores API key metadata in SQLite.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore returns an API key store backed by db.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// Create stores key metadata by hash.
func (s *SQLStore) Create(ctx context.Context, key Key) error {
	if key.Hash == "" {
		return fmt.Errorf("auth: key hash must not be empty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_key_metadata (id, name, hash, scopes, created_at)
         VALUES (?, ?, ?, ?, ?)`,
		key.ID,
		key.Name,
		key.Hash,
		encodeScopes(key.Scopes),
		formatTime(key.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("auth: create key: %w", err)
	}
	return nil
}

// CreateIfNotExists stores key metadata by hash when it is not already present.
// It returns true when a row was inserted and false when the hash already exists.
func (s *SQLStore) CreateIfNotExists(ctx context.Context, key Key) (bool, error) {
	if key.Hash == "" {
		return false, fmt.Errorf("auth: key hash must not be empty")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_key_metadata (id, name, hash, scopes, created_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(hash) DO NOTHING`,
		key.ID,
		key.Name,
		key.Hash,
		encodeScopes(key.Scopes),
		formatTime(key.CreatedAt),
	)
	if err != nil {
		return false, fmt.Errorf("auth: create key if not exists: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("auth: create key rows affected: %w", err)
	}
	return n > 0, nil
}

// FindByHash returns key metadata by hash.
func (s *SQLStore) FindByHash(ctx context.Context, hash string) (Key, error) {
	key, err := s.scanKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, hash, scopes, created_at, revoked_at
         FROM api_key_metadata
         WHERE hash = ?`,
		hash,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, fmt.Errorf("auth: find key: %w", err)
	}
	return key, nil
}

// RevokeByHash records a revocation timestamp for hash.
func (s *SQLStore) RevokeByHash(ctx context.Context, hash string, revokedAt time.Time) (Key, error) {
	key, err := s.FindByHash(ctx, hash)
	if err != nil {
		return Key{}, err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE api_key_metadata SET revoked_at = ? WHERE hash = ?`,
		formatTime(revokedAt),
		hash,
	)
	if err != nil {
		return Key{}, fmt.Errorf("auth: revoke key: %w", err)
	}
	key.RevokedAt = ptrTime(revokedAt.UTC())
	return key, nil
}

// findByID returns key metadata by its public ID (hash[:16], the table's primary
// key), mapping a missing row to ErrInvalidKey like FindByHash.
func (s *SQLStore) findByID(ctx context.Context, id string) (Key, error) {
	key, err := s.scanKey(s.db.QueryRowContext(ctx,
		`SELECT id, name, hash, scopes, created_at, revoked_at
         FROM api_key_metadata
         WHERE id = ?`,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, fmt.Errorf("auth: find key by id: %w", err)
	}
	return key, nil
}

// RevokeByID records a revocation timestamp for the key with the given public ID.
// id is the table's primary key, so it matches at most one row.
func (s *SQLStore) RevokeByID(ctx context.Context, id string, revokedAt time.Time) (Key, error) {
	key, err := s.findByID(ctx, id)
	if err != nil {
		return Key{}, err
	}
	// Idempotent: re-revoking an already-revoked key is a no-op that preserves the
	// original revocation timestamp rather than re-stamping it to now.
	if key.RevokedAt != nil {
		return key, nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE api_key_metadata SET revoked_at = ? WHERE id = ?`,
		formatTime(revokedAt),
		id,
	)
	if err != nil {
		return Key{}, fmt.Errorf("auth: revoke key by id: %w", err)
	}
	key.RevokedAt = ptrTime(revokedAt.UTC())
	return key, nil
}

// List returns all key metadata in stable creation order.
func (s *SQLStore) List(ctx context.Context) (keys []Key, retErr error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, hash, scopes, created_at, revoked_at
         FROM api_key_metadata
         ORDER BY created_at ASC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: list keys: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("auth: close key rows: %w", err)
		}
	}()
	for rows.Next() {
		key, err := s.scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: key rows: %w", err)
	}
	return keys, nil
}

type keyScanner interface {
	Scan(dest ...any) error
}

func (s *SQLStore) scanKey(row keyScanner) (Key, error) {
	var key Key
	var scopes, createdAt string
	var revokedAt sql.NullString
	if err := row.Scan(&key.ID, &key.Name, &key.Hash, &scopes, &createdAt, &revokedAt); err != nil {
		return Key{}, err
	}
	key.Scopes = decodeScopes(scopes)
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return Key{}, fmt.Errorf("auth: parse created_at: %w", err)
	}
	key.CreatedAt = t
	if revokedAt.Valid {
		t, err := time.Parse(time.RFC3339, revokedAt.String)
		if err != nil {
			return Key{}, fmt.Errorf("auth: parse revoked_at: %w", err)
		}
		key.RevokedAt = &t
	}
	return key, nil
}

func encodeScopes(scopes []Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, v := range scopes {
		parts = append(parts, string(v))
	}
	return strings.Join(parts, ",")
}

func decodeScopes(scopes string) []Scope {
	var ret []Scope
	for _, v := range strings.Split(scopes, ",") {
		if v = strings.TrimSpace(v); v != "" {
			ret = append(ret, Scope(v))
		}
	}
	return ret
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
