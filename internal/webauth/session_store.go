package webauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// sessionTokenBytes is the entropy of a raw session token before base64url
// encoding (256 bits, matching the API-key generator).
const sessionTokenBytes = 32

// Session is a server-side login session. TokenHash is the SHA-256 hex of the
// raw token; the raw token itself is returned to the caller only at creation and
// is never persisted.
type Session struct {
	TokenHash string    `json:"-"`
	UserID    string    `json:"user_id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// SessionStore persists login sessions keyed by the SHA-256 of the raw token.
type SessionStore interface {
	// CreateSession mints a raw token, stores only its SHA-256, and returns the
	// raw token to the caller (the only time it exists outside the browser).
	CreateSession(ctx context.Context, userID string, expiresAt time.Time) (rawToken string, err error)
	// GetSessionByToken resolves rawToken via its SHA-256 and returns the session
	// only if it has not expired. ok is false for unknown or expired tokens.
	GetSessionByToken(ctx context.Context, rawToken string) (session Session, ok bool, err error)
	// DeleteSession removes the session for rawToken. Deleting an unknown token is
	// a no-op (used for logout).
	DeleteSession(ctx context.Context, rawToken string) error
	// CleanExpiredSessions deletes every session whose expires_at is at or before
	// now and returns the number removed.
	CleanExpiredSessions(ctx context.Context) (int64, error)
	// DeleteSessionsForUser removes every session belonging to userID and returns
	// the number removed. Deleting sessions for a user with none is a no-op.
	// Rotation uses this so a password change actually ends existing logins: an
	// operator rotating a compromised credential expects sessions minted with the
	// old one to stop working immediately (#545).
	DeleteSessionsForUser(ctx context.Context, userID string) (int64, error)
}

// SQLSessionStore persists sessions in the SQLite `sessions` table.
type SQLSessionStore struct {
	db   *sql.DB
	rand io.Reader
	now  func() time.Time
}

// NewSQLSessionStore returns a SQL-backed session store.
func NewSQLSessionStore(db *sql.DB) *SQLSessionStore {
	return &SQLSessionStore{db: db, rand: rand.Reader, now: time.Now}
}

// CreateSession generates a 32-byte base64url token, persists its SHA-256 hash
// with the given expiry, and returns the raw token.
func (s *SQLSessionStore) CreateSession(ctx context.Context, userID string, expiresAt time.Time) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("webauth: session user id must not be empty")
	}
	raw, err := s.newToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(raw), userID, formatTime(expiresAt),
	)
	if err != nil {
		return "", fmt.Errorf("webauth: create session: %w", err)
	}
	return raw, nil
}

// GetSessionByToken returns the unexpired session for rawToken, or ok=false.
func (s *SQLSessionStore) GetSessionByToken(ctx context.Context, rawToken string) (Session, bool, error) {
	var (
		sess                 Session
		createdAt, expiresAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at
		 FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		hashToken(rawToken), formatTime(s.now()),
	).Scan(&sess.TokenHash, &sess.UserID, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("webauth: get session: %w", err)
	}
	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, false, err
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// DeleteSession removes the session for rawToken (no-op if absent).
func (s *SQLSessionStore) DeleteSession(ctx context.Context, rawToken string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`, hashToken(rawToken),
	); err != nil {
		return fmt.Errorf("webauth: delete session: %w", err)
	}
	return nil
}

// DeleteSessionsForUser removes every session belonging to userID, regardless of
// expiry. Used by credential rotation so a password change ends existing logins
// rather than leaving tokens minted under the old password usable until they
// expire on their own.
func (s *SQLSessionStore) DeleteSessionsForUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("webauth: delete sessions for user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("webauth: delete sessions for user rows: %w", err)
	}
	return n, nil
}

// CleanExpiredSessions deletes sessions whose expiry is at or before now.
func (s *SQLSessionStore) CleanExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, formatTime(s.now()),
	)
	if err != nil {
		return 0, fmt.Errorf("webauth: clean expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("webauth: clean expired sessions rows: %w", err)
	}
	return n, nil
}

func (s *SQLSessionStore) newToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(s.rand, b); err != nil {
		return "", fmt.Errorf("webauth: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the lowercase SHA-256 hex of a raw token. This is the value
// stored at rest and used for lookup, so the raw bearer token never touches disk.
func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
