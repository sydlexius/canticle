package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	"github.com/sydlexius/canticle/internal/normalize"
	"modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(
		"normalize_key",
		1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if len(args) != 1 || args[0] == nil {
				return "", nil
			}
			s, ok := args[0].(string)
			if !ok {
				return "", fmt.Errorf("normalize_key: expected string, got %T", args[0])
			}
			return normalize.NormalizeKey(s), nil
		},
	)
}

// Open opens (or creates) the SQLite database at path, applies pragmas,
// and runs any pending goose migrations. Returns a ready-to-use *sql.DB.
// The caller must close the returned DB when done.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	return open(ctx, path, "")
}

// OpenImmediate is Open, except every read-write transaction on the returned
// handle begins with BEGIN IMMEDIATE instead of SQLite's default DEFERRED
// (#978). It is for one-shot batch CLI commands that mutate a database a
// running `serve` may be writing to at the same time; serve itself keeps Open.
//
// A DEFERRED transaction that reads before it writes takes its WAL snapshot at
// the first read. If another connection commits before the first write, the
// lock upgrade fails with SQLITE_BUSY at once and busy_timeout does not apply:
// no amount of waiting makes the stale snapshot valid. IMMEDIATE takes the
// write lock at BEGIN, where busy_timeout does apply, so contention becomes a
// bounded wait at BEGIN (and, past the timeout, a busy error before the
// transaction has done anything, which is safe to retry) rather than an abort
// part-way through.
func OpenImmediate(ctx context.Context, path string) (*sql.DB, error) {
	return open(ctx, path, "?_txlock=immediate")
}

// OpenForBatch opens a batch CLI command's database: OpenImmediate when apply
// is true, Open otherwise. A dry run only reads, but several of them still
// read inside a transaction; on an immediate handle that BEGIN would take the
// writer lock and stall a live serve for the whole preview.
func OpenForBatch(ctx context.Context, path string, apply bool) (*sql.DB, error) {
	if apply {
		return OpenImmediate(ctx, path)
	}
	return Open(ctx, path)
}

// open is the shared body of Open and OpenImmediate. dsnQuery is appended to
// path as the driver DSN's query string; the driver strips it back off a
// non-"file:" DSN before opening, so the file opened is path either way.
func open(ctx context.Context, path, dsnQuery string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db: path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("db: create data dir: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", path+dsnQuery)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}

	// Limit to one connection so per-connection PRAGMAs are reliable.
	sqlDB.SetMaxOpenConns(1)

	// PRAGMA journal_mode returns the mode actually applied, not just acknowledged,
	// so read it back and warn if WAL was not enabled (e.g. on a read-only FS).
	var journalMode string
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: set WAL mode: %w", err)
	}
	if journalMode != "wal" {
		slog.Warn("db: WAL mode not enabled; running in fallback mode", "actual_mode", journalMode)
	}

	// Apply remaining pragmas. These do not require read-back verification;
	// any application failure surfaces as a non-nil error from ExecContext.
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := sqlDB.ExecContext(ctx, p); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("db: pragma %q: %w", p, err)
		}
	}

	// Run embedded migrations via goose NewProvider (thread-safe, non-global API).
	// fs.Sub roots the FS at the migrations/ subdirectory so goose can find *.sql at root.
	migFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: sub migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, migFS)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: migrate: %w", err)
	}

	return sqlDB, nil
}

// OpenReadOnly opens an existing database for query-only access. Unlike Open it
// does NOT create the data directory, does NOT enable WAL, and does NOT run
// migrations, so it is safe for side-effect-free callers such as shell
// completion: a tab-press must never mutate schema or data. The connection is
// set query_only, so any accidental write is rejected. The caller is responsible
// for ensuring the file exists; opening a missing database returns an error
// rather than creating one.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db: path must not be empty")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("db: open read-only %s: %w", path, err)
	}

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: open read-only %s: %w", path, err)
	}
	// Single connection so the query_only PRAGMA reliably applies to every query.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: set query_only: %w", err)
	}
	return sqlDB, nil
}
