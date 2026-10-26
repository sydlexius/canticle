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
	// RotateCredential replaces the password hash AND revokes every session for
	// that user ATOMICALLY, returning the number of sessions revoked.
	//
	// The two writes must not be separable. Done sequentially, a failure of the
	// revoke after a successful hash update leaves the new password active while
	// sessions minted under the OLD password stay valid -- and the caller reports
	// failure, so the operator believes neither happened. That is the exact state
	// a rotation exists to prevent.
	//
	// It deliberately spans the users and sessions tables, which sit in the same
	// database, rather than plumbing a transaction handle through both store
	// interfaces. Atomicity here is a correctness requirement of rotation, not an
	// implementation detail of either table.
	RotateCredential(ctx context.Context, username, passwordHash string) (revoked int64, err error)
	// RotateCredentialIfCurrent is RotateCredential as a compare-and-swap: it
	// rotates only while the stored hash is still currentHash, the one the caller
	// verified a password against. If the hash changed underneath (a rotation
	// committed in between) it returns ErrInvalidCredentials and writes nothing;
	// an unknown user is ErrUserNotFound. The unconditional RotateCredential
	// stays for the operator's override, which has no current password to compare.
	RotateCredentialIfCurrent(ctx context.Context, username, currentHash, passwordHash string) (revoked int64, err error)
}

// SQLUserStore persists users in the SQLite `users` table.
type SQLUserStore struct {
	db   *sql.DB
	read *sql.DB
	rand io.Reader
}

// NewSQLUserStore returns a SQL-backed user store.
func NewSQLUserStore(db *sql.DB) *SQLUserStore {
	return &SQLUserStore{db: db, read: db, rand: rand.Reader}
}

// WithReadDB routes the store's lookups (GetByUsername, GetByID, HasUsers) to
// read, a query-only pool from db.OpenReadPool (#1652), so a per-request session
// check never queues behind the writer's single connection. Every write, and the
// read-back of a row this store just created, stays on the writer. A nil read
// leaves the store on the writer. It returns the store for chaining.
func (s *SQLUserStore) WithReadDB(read *sql.DB) *SQLUserStore {
	if read != nil {
		s.read = read
	}
	return s
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
	return s.scanOne(ctx, s.read,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE username = ?`,
		strings.TrimSpace(username),
	)
}

// GetByID returns the user for id.
func (s *SQLUserStore) GetByID(ctx context.Context, id string) (User, bool, error) {
	return s.scanOne(ctx, s.read,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE id = ?`,
		id,
	)
}

// HasUsers reports whether the users table holds at least one row.
func (s *SQLUserStore) HasUsers(ctx context.Context) (bool, error) {
	var exists int
	err := s.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("webauth: has users: %w", err)
	}
	return exists == 1, nil
}

// RotateCredential updates the password hash and deletes that user's sessions in
// a single transaction, so the pair can never half-apply. See the interface doc
// for why atomicity is a correctness requirement rather than a nicety.
//
// The session delete resolves the user by the same case-insensitive match as the
// update, inside the transaction, so a concurrent rename cannot leave the two
// statements acting on different rows.
func (s *SQLUserStore) RotateCredential(ctx context.Context, username, passwordHash string) (int64, error) {
	return s.rotate(ctx, username, nil, passwordHash)
}

// RotateCredentialIfCurrent is the compare-and-swap form (see the interface
// doc): the same transaction, with the UPDATE also matching on the stored hash.
func (s *SQLUserStore) RotateCredentialIfCurrent(ctx context.Context, username, currentHash, passwordHash string) (int64, error) {
	return s.rotate(ctx, username, &currentHash, passwordHash)
}

// rotate is the one rotation transaction. A nil onlyIf rotates unconditionally;
// otherwise the hash is replaced only while it still equals *onlyIf.
func (s *SQLUserStore) rotate(ctx context.Context, username string, onlyIf *string, passwordHash string) (int64, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, fmt.Errorf("webauth: username must not be empty")
	}
	if passwordHash == "" {
		return 0, fmt.Errorf("webauth: password hash must not be empty")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: begin: %w", err)
	}
	// Rollback is a no-op once the transaction has committed.
	defer tx.Rollback() //nolint:errcheck // reason: no-op after commit; the commit error is what callers act on

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE username = ? COLLATE NOCASE AND (? IS NULL OR password_hash = ?)`,
		passwordHash, username, onlyIf, onlyIf,
	)
	if err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: update hash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: update rows: %w", err)
	}
	if n == 0 {
		if onlyIf == nil {
			return 0, ErrUserNotFound
		}
		// "No such user" or "the hash changed underneath": told apart in this tx.
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE username = ? COLLATE NOCASE`, username).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrUserNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("webauth: rotate credential: recheck user: %w", err)
		}
		return 0, ErrInvalidCredentials
	}

	del, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id IN
		 (SELECT id FROM users WHERE username = ? COLLATE NOCASE)`,
		username,
	)
	if err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: revoke sessions: %w", err)
	}
	revoked, err := del.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: revoke rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("webauth: rotate credential: commit: %w", err)
	}
	return revoked, nil
}

func (s *SQLUserStore) requireByID(ctx context.Context, id string) (User, error) {
	// Reads back on the writer, never the pool: the caller just wrote this row.
	user, ok, err := s.scanOne(ctx, s.db,
		`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE id = ?`, id)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, fmt.Errorf("webauth: created user %q not found on read-back", id)
	}
	return user, nil
}

func (s *SQLUserStore) scanOne(ctx context.Context, q *sql.DB, query string, arg string) (User, bool, error) {
	var (
		u                    User
		createdAt, updatedAt string
	)
	err := q.QueryRowContext(ctx, query, arg).
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
