package auth

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	// KeyPrefix is prepended to every generated API key.
	KeyPrefix = "mxlrc_"

	keyBytes       = 32
	hashIterations = 210_000
	hashSalt       = "mxlrcgo-svc api key hash v1"
)

var (
	// ErrDuplicateKey is returned when a generated key hash already exists.
	ErrDuplicateKey = errors.New("auth: duplicate key")
	// ErrInvalidKey is returned when a raw key is malformed or unknown.
	ErrInvalidKey = errors.New("auth: invalid key")
	// ErrRevokedKey is returned when a key was revoked before validation.
	ErrRevokedKey = errors.New("auth: revoked key")
	// ErrForbiddenScope is returned when a key lacks the required scope.
	ErrForbiddenScope = errors.New("auth: forbidden scope")
)

// Scope identifies an API key permission.
type Scope string

const (
	// ScopeWebhook permits webhook ingestion endpoints.
	ScopeWebhook Scope = "webhook"
	// ScopeAdmin permits every operation.
	ScopeAdmin Scope = "admin"
)

// Key stores API key metadata. Raw key material is never stored here.
type Key struct {
	ID        string
	Name      string
	Hash      string
	Scopes    []Scope
	CreatedAt time.Time
	RevokedAt *time.Time
}

// CreatedKey returns the one-time raw key with its stored metadata.
type CreatedKey struct {
	Raw string
	Key Key
}

// Store persists API key metadata by derived hash.
type Store interface {
	Create(ctx context.Context, key Key) error
	FindByHash(ctx context.Context, hash string) (Key, error)
	RevokeByHash(ctx context.Context, hash string, revokedAt time.Time) (Key, error)
	RevokeByID(ctx context.Context, id string, revokedAt time.Time) (Key, error)
	List(ctx context.Context) ([]Key, error)
}

// Service manages API key generation, validation, revocation, and listing.
type Service struct {
	store Store
	rand  io.Reader
	now   func() time.Time
}

// NewService returns an API key service backed by store.
func NewService(store Store) *Service {
	return &Service{
		store: store,
		rand:  rand.Reader,
		now:   time.Now,
	}
}

// CreateKey generates a new raw key and stores only its derived hash.
func (s *Service) CreateKey(ctx context.Context, name string, scopes []Scope) (CreatedKey, error) {
	if s.store == nil {
		return CreatedKey{}, fmt.Errorf("auth: store dependency is nil")
	}
	if s.rand == nil {
		return CreatedKey{}, fmt.Errorf("auth: rand dependency is nil")
	}
	if s.now == nil {
		return CreatedKey{}, fmt.Errorf("auth: now dependency is nil")
	}
	norm, err := NormalizeScopes(scopes)
	if err != nil {
		return CreatedKey{}, err
	}

	raw, err := generateKey(s.rand)
	if err != nil {
		return CreatedKey{}, err
	}
	hash, err := HashKey(raw)
	if err != nil {
		return CreatedKey{}, err
	}
	if len(hash) < 16 {
		return CreatedKey{}, fmt.Errorf("auth: derived key hash is too short")
	}
	key := Key{
		ID:        hash[:16],
		Name:      strings.TrimSpace(name),
		Hash:      hash,
		Scopes:    norm,
		CreatedAt: s.now().UTC(),
	}
	if err := s.store.Create(ctx, key); err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{Raw: raw, Key: key}, nil
}

// ValidateKey returns stored key metadata if raw has the required scope.
func (s *Service) ValidateKey(ctx context.Context, raw string, required Scope) (Key, error) {
	if s.store == nil {
		return Key{}, fmt.Errorf("auth: store dependency is nil")
	}
	if !strings.HasPrefix(raw, KeyPrefix) {
		return Key{}, ErrInvalidKey
	}
	hash, err := HashKey(raw)
	if err != nil {
		return Key{}, err
	}
	key, err := s.store.FindByHash(ctx, hash)
	if err != nil {
		return Key{}, err
	}
	if subtle.ConstantTimeCompare([]byte(key.Hash), []byte(hash)) != 1 {
		return Key{}, ErrInvalidKey
	}
	if key.RevokedAt != nil {
		return Key{}, ErrRevokedKey
	}
	if required != "" && !HasScope(key.Scopes, required) {
		return Key{}, ErrForbiddenScope
	}
	return key, nil
}

// RevokeKey revokes raw and returns the updated metadata.
func (s *Service) RevokeKey(ctx context.Context, raw string) (Key, error) {
	if s.store == nil {
		return Key{}, fmt.Errorf("auth: store dependency is nil")
	}
	if s.now == nil {
		return Key{}, fmt.Errorf("auth: now dependency is nil")
	}
	if !strings.HasPrefix(raw, KeyPrefix) {
		return Key{}, ErrInvalidKey
	}
	hash, err := HashKey(raw)
	if err != nil {
		return Key{}, err
	}
	return s.store.RevokeByHash(ctx, hash, s.now().UTC())
}

// RevokeKeyByID revokes the key identified by id (the public Key.ID) and returns
// the updated metadata. It exists so the management UI can revoke a key it only
// knows by its displayed ID: the raw key is never recoverable after creation, so
// RevokeKey (which derives the hash from the raw key) cannot be used there. The
// nil-dependency guards mirror RevokeKey.
func (s *Service) RevokeKeyByID(ctx context.Context, id string) (Key, error) {
	if s.store == nil {
		return Key{}, fmt.Errorf("auth: store dependency is nil")
	}
	if s.now == nil {
		return Key{}, fmt.Errorf("auth: now dependency is nil")
	}
	if strings.TrimSpace(id) == "" {
		return Key{}, ErrInvalidKey
	}
	return s.store.RevokeByID(ctx, id, s.now().UTC())
}

// ListKeys returns stored key metadata without raw key material.
func (s *Service) ListKeys(ctx context.Context) ([]Key, error) {
	if s.store == nil {
		return nil, fmt.Errorf("auth: store dependency is nil")
	}
	return s.store.List(ctx)
}

// HashKey returns the lowercase hex PBKDF2-SHA256 hash for raw.
func HashKey(raw string) (string, error) {
	key, err := pbkdf2.Key(sha256.New, raw, []byte(hashSalt), hashIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("auth: derive key hash: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// NormalizeScopes validates, deduplicates, and sorts scopes.
func NormalizeScopes(scopes []Scope) ([]Scope, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("auth: at least one scope is required")
	}
	seen := map[Scope]bool{}
	var norm []Scope
	for _, v := range scopes {
		scope := Scope(strings.TrimSpace(string(v)))
		switch scope {
		case ScopeWebhook, ScopeAdmin:
		default:
			return nil, fmt.Errorf("auth: unsupported scope %q", v)
		}
		if !seen[scope] {
			seen[scope] = true
			norm = append(norm, scope)
		}
	}
	slices.Sort(norm)
	return norm, nil
}

// HasScope reports whether scopes grant required. Admin grants every scope.
func HasScope(scopes []Scope, required Scope) bool {
	if required == "" {
		return true
	}
	for _, v := range scopes {
		if v == ScopeAdmin || v == required {
			return true
		}
	}
	return false
}

func generateKey(r io.Reader) (string, error) {
	b := make([]byte, keyBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("auth: generate key: %w", err)
	}
	return KeyPrefix + hex.EncodeToString(b), nil
}
