package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// writeFromSecondConn attempts a write on an independent connection that does
// not wait for locks, standing in for serve racing a CLI transaction.
func writeFromSecondConn(t *testing.T, path string) error {
	t.Helper()
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open second conn: %v", err)
	}
	defer func() { _ = other.Close() }()
	_, err = other.ExecContext(context.Background(), `INSERT INTO libraries (path, name) VALUES ('/other', 'Other')`)
	return err
}

// TestOpenImmediate_TxTakesWriteLockAtBegin proves #978's layer 1: a
// transaction on an OpenImmediate handle holds the write lock from BEGIN, before
// it has written anything, so a second connection's write is refused. On an Open
// handle (DEFERRED) the same untouched transaction holds no write lock and the
// second write succeeds -- the control that shows the assertion measures the
// handle, not the test.
func TestOpenImmediate_TxTakesWriteLockAtBegin(t *testing.T) {
	cases := []struct {
		name     string
		open     func(context.Context, string) (*sql.DB, error)
		wantBusy bool
	}{
		{"immediate", OpenImmediate, true},
		{"deferred control", Open, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "lock.db")
			sqlDB, err := tc.open(ctx, path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })

			tx, err := sqlDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM libraries`).Scan(&n); err != nil {
				t.Fatalf("read in tx: %v", err)
			}

			err = writeFromSecondConn(t, path)
			if got := IsSQLiteBusy(err); got != tc.wantBusy {
				t.Fatalf("second connection write err = %v; busy = %v, want %v", err, got, tc.wantBusy)
			}
		})
	}
}

// TestOpenImmediate_KeepsOpenSetup verifies the DSN query does not bypass the
// setup OpenImmediate shares with Open: WAL, busy_timeout, foreign keys, and
// migrations all still apply.
func TestOpenImmediate_KeepsOpenSetup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "setup.db")
	sqlDB, err := OpenImmediate(ctx, path)
	if err != nil {
		t.Fatalf("OpenImmediate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var mode string
	var busy, fk int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if mode != "wal" || busy != 5000 || fk != 1 {
		t.Errorf("journal_mode=%q busy_timeout=%d foreign_keys=%d; want wal, 5000, 1", mode, busy, fk)
	}
	// The file on disk is exactly path (the query string is not part of the name)
	// and it carries the migrated schema.
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen same path with Open: %v", err)
	}
	_ = reopened.Close()
	var tables int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='work_queue'`).Scan(&tables); err != nil || tables != 1 {
		t.Errorf("work_queue table count = %d (err %v); want 1", tables, err)
	}
}
