package auth

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// MemoryStore stores key metadata in memory for tests and single-process use.
type MemoryStore struct {
	mu   sync.RWMutex
	keys map[string]Key
}

// NewMemoryStore returns an empty in-memory key store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: map[string]Key{}}
}

// Create stores key metadata by hash.
func (s *MemoryStore) Create(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key.Hash == "" {
		return fmt.Errorf("auth: key hash must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[key.Hash]; ok {
		return ErrDuplicateKey
	}
	s.keys[key.Hash] = cloneKey(key)
	return nil
}

// FindByHash returns key metadata by hash.
func (s *MemoryStore) FindByHash(ctx context.Context, hash string) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[hash]
	if !ok {
		return Key{}, ErrInvalidKey
	}
	return cloneKey(key), nil
}

// RevokeByHash records a revocation timestamp for hash.
func (s *MemoryStore) RevokeByHash(ctx context.Context, hash string, revokedAt time.Time) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[hash]
	if !ok {
		return Key{}, ErrInvalidKey
	}
	at := revokedAt.UTC()
	key.RevokedAt = &at
	s.keys[hash] = cloneKey(key)
	return cloneKey(key), nil
}

// RevokeByID records a revocation timestamp for the key with the given public ID
// (Key.ID). IDs are unique (the SQL primary key), so the in-memory store mirrors
// that by revoking the single matching entry.
func (s *MemoryStore) RevokeByID(ctx context.Context, id string, revokedAt time.Time) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, key := range s.keys {
		if key.ID != id {
			continue
		}
		// Idempotent: re-revoking an already-revoked key preserves the original
		// revocation timestamp rather than re-stamping it to now.
		if key.RevokedAt != nil {
			return cloneKey(key), nil
		}
		at := revokedAt.UTC()
		key.RevokedAt = &at
		s.keys[hash] = cloneKey(key)
		return cloneKey(key), nil
	}
	return Key{}, ErrInvalidKey
}

// List returns all key metadata in stable creation order.
func (s *MemoryStore) List(ctx context.Context) ([]Key, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]Key, 0, len(s.keys))
	for _, v := range s.keys {
		keys = append(keys, cloneKey(v))
	}
	slices.SortFunc(keys, func(a Key, b Key) int {
		switch {
		case a.CreatedAt.Before(b.CreatedAt):
			return -1
		case a.CreatedAt.After(b.CreatedAt):
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	return keys, nil
}

func cloneKey(key Key) Key {
	key.Scopes = append([]Scope(nil), key.Scopes...)
	if key.RevokedAt != nil {
		at := *key.RevokedAt
		key.RevokedAt = &at
	}
	return key
}
