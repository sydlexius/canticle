package webauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// userIDBytes is the length of the random user id before hex-encoding.
const userIDBytes = 16

// ErrUserExists is returned when creating a user whose username is already taken
// (the username UNIQUE constraint fired).
var ErrUserExists = errors.New("webauth: user already exists")

// User is a web-UI admin account. PasswordHash is an Argon2id PHC string; the
// plaintext password is never held here.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UserStore persists admin accounts. Implementations are storage-swappable
// (repository-over-interface, mirroring internal/cache and internal/secrets).
type UserStore interface {
	// CreateUser inserts a new user with the given username and Argon2id hash and
	// returns the stored row. It returns ErrUserExists if the username is taken.
	CreateUser(ctx context.Context, username, passwordHash string) (User, error)
	// CreateFirstUser atomically inserts the first admin: the insert succeeds only
	// when the users table is empty. It returns ErrUserExists if any user already
	// exists, closing the check-then-insert race that a HasUsers+CreateUser pair
	// would leave open (two concurrent first-run setups with different usernames).
	CreateFirstUser(ctx context.Context, username, passwordHash string) (User, error)
	// GetByUsername returns the user for username (case-insensitive). ok is false
	// when no such user exists; that is not an error.
	GetByUsername(ctx context.Context, username string) (user User, ok bool, err error)
	// GetByID returns the user for id. ok is false when no such user exists.
	GetByID(ctx context.Context, id string) (user User, ok bool, err error)
	// HasUsers reports whether any user exists (used for first-run detection).
	HasUsers(ctx context.Context) (bool, error)
	// UpdatePasswordHash replaces the stored Argon2id hash for username
	// (case-insensitive), leaving the user's identity and id intact. It returns
	// ErrUserNotFound when no such user exists. This is the durable half of
	// credential rotation (#545); before it existed the only way to change an
	// admin password was deleting the row from the database.
	UpdatePasswordHash(ctx context.Context, username, passwordHash string) error
}

// SQLUserStore persists users in the SQLite `users` table.
type SQLUserStore struct {
	db   *sql.DB
	rand io.Reader
}

// NewSQLUserStore returns a SQL-backed user store.
func NewSQLUserStore(db *sql.DB) *SQLUserStore {
	return &SQLUserStore{db: db, rand: rand.Reader}
}

// CreateUser inserts a user with a fresh random id. A username collision (the
// UNIQUE constraint) is reported as ErrUserExists.
func (s *SQLUserStore) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, fmt.Errorf("webauth: username must not be empty")
	}
	if passwordHash == "" {
		return User{}, fmt.Errorf("webauth: password hash must not be empty")
	}
	id, err := s.newID()
	if err != nil {
		return User{}, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)`,
		id, username, passwordHash,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("webauth: create user: %w", err)
	}
	return s.requireByID(ctx, id)
}

// CreateFirstUser inserts the admin only if no user exists yet, in a single
// atomic statement (INSERT ... SELECT ... WHERE NOT EXISTS). Because the check
// and the insert happen in one Exec, two concurrent first-run setups cannot both
// succeed: the second sees a non-empty table, inserts zero rows, and gets
// ErrUserExists. This is the authoritative guard against multi-admin creation.
func (s *SQLUserStore) CreateFirstUser(ctx context.Context, username, passwordHash string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, fmt.Errorf("webauth: username must not be empty")
	}
	if passwordHash == "" {
		return User{}, fmt.Errorf("webauth: password hash must not be empty")
	}
	id, err := s.newID()
	if err != nil {
		return User{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash)
		 SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		id, username, passwordHash,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("webauth: create first user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return User{}, fmt.Errorf("webauth: create first user rows: %w", err)
	}
	if n == 0 {
		return User{}, ErrUserExists
	}
	return s.requireByID(ctx, id)
}

// GetByUsername returns the user for username (case-insensitive via COLLATE NOCASE).
func (s *SQLUserStore) GetByUsername(ctx context.Context, username string) (User, bool, error) {
	return s.scanOne(ctx,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE username = ?`,
		strings.TrimSpace(username),
	)
}

// GetByID returns the user for id.
func (s *SQLUserStore) GetByID(ctx context.Context, id string) (User, bool, error) {
	return s.scanOne(ctx,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE id = ?`,
		id,
	)
}

// HasUsers reports whether the users table holds at least one row.
func (s *SQLUserStore) HasUsers(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("webauth: has users: %w", err)
	}
	return exists == 1, nil
}

// UpdatePasswordHash replaces the stored hash for username, matching the
// case-insensitive lookup GetByUsername uses so a rotation cannot miss the row
// it just read. The user's id and created_at are untouched: rotation changes a
// credential, not an identity, so sessions, audit trails, and any future
// per-user rows stay attached to the same account.
func (s *SQLUserStore) UpdatePasswordHash(ctx context.Context, username, passwordHash string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("webauth: username must not be empty")
	}
	if passwordHash == "" {
		return fmt.Errorf("webauth: password hash must not be empty")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE username = ? COLLATE NOCASE`,
		passwordHash, username,
	)
	if err != nil {
		return fmt.Errorf("webauth: update password hash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("webauth: update password hash: %w", err)
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (s *SQLUserStore) requireByID(ctx context.Context, id string) (User, error) {
	user, ok, err := s.GetByID(ctx, id)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, fmt.Errorf("webauth: created user %q not found on read-back", id)
	}
	return user, nil
}

func (s *SQLUserStore) scanOne(ctx context.Context, query string, arg string) (User, bool, error) {
	var (
		u                    User
		createdAt, updatedAt string
	)
	err := s.db.QueryRowContext(ctx, query, arg).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("webauth: get user: %w", err)
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return User{}, false, err
	}
	if u.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (s *SQLUserStore) newID() (string, error) {
	b := make([]byte, userIDBytes)
	if _, err := io.ReadFull(s.rand, b); err != nil {
		return "", fmt.Errorf("webauth: generate user id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// isUniqueViolation reports whether err is a SQLite constraint violation
// (extended result codes carry SQLITE_CONSTRAINT == 19 in the low byte). It is
// used to map a username collision onto ErrUserExists.
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == 19 // SQLITE_CONSTRAINT
	}
	return false
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("webauth: parse time %q: %w", s, err)
	}
	return t, nil
}
