package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readPoolSize is the pool size these tests pin every connection of.
const readPoolSize = 4

// openReadPoolFixture migrates a fresh database through Open (the writer, left
// open and returned) and opens a read pool on the same file.
func openReadPoolFixture(t *testing.T) (writer, pool *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES ('/music', 'Music')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool, err = OpenReadPool(ctx, path, readPoolSize)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return writer, pool
}

// pinAll checks out every connection the pool allows at once, so each one is
// a distinct physical connection rather than the first one reused.
func pinAll(t *testing.T, pool *sql.DB) []*sql.Conn {
	t.Helper()
	conns := make([]*sql.Conn, readPoolSize)
	for i := range conns {
		// A bounded wait, so a pool that hands out fewer connections fails here
		// rather than blocking until the test binary times out.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := pool.Conn(ctx)
		cancel()
		if err != nil {
			t.Fatalf("pool did not hand out %d connections: conn %d: %v", readPoolSize, i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		conns[i] = c
	}
	if got := pool.Stats().OpenConnections; got != readPoolSize {
		t.Fatalf("open connections = %d, want %d distinct", got, readPoolSize)
	}
	return conns
}

// TestOpenReadPool_RejectsWritesOnEveryConn pins query_only on every pooled
// connection, not only the first one the handle opened.
func TestOpenReadPool_RejectsWritesOnEveryConn(t *testing.T) {
	_, pool := openReadPoolFixture(t)
	for i, c := range pinAll(t, pool) {
		if _, err := c.ExecContext(context.Background(), `INSERT INTO libraries (path, name) VALUES ('/x', 'X')`); err == nil {
			t.Errorf("conn %d: write through the read pool succeeded; want a query_only rejection", i)
		}
		var n int
		if err := c.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM libraries`).Scan(&n); err != nil || n != 1 {
			t.Errorf("conn %d: read = %d, %v; want 1, nil", i, n, err)
		}
	}
}

// TestOpenReadPool_BusyTimeoutOnEveryConn pins busy_timeout on every pooled
// connection.
func TestOpenReadPool_BusyTimeoutOnEveryConn(t *testing.T) {
	_, pool := openReadPoolFixture(t)
	for i, c := range pinAll(t, pool) {
		var ms int
		if err := c.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&ms); err != nil {
			t.Fatalf("conn %d: read busy_timeout: %v", i, err)
		}
		if ms != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, ms)
		}
	}
}

// TestOpenReadPool_ReadsWhileWriterHeld is the #1649 symptom: with the single
// writer connection held by an open write transaction, a read on the pool
// still completes, well inside a short deadline, and sees the last commit.
func TestOpenReadPool_ReadsWhileWriterHeld(t *testing.T) {
	writer, pool := openReadPoolFixture(t)
	ctx := context.Background()
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES ('/held', 'Held')`); err != nil {
		t.Fatalf("uncommitted insert: %v", err)
	}

	// Control: the writer handle's only connection is in the transaction, so a
	// read through it cannot get a connection before the deadline.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := writer.QueryRowContext(short, `SELECT 1`).Scan(new(int)); err == nil {
		t.Fatal("read on the held writer handle succeeded; the fixture is not holding it")
	}

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	var n int
	if err := pool.QueryRowContext(readCtx, `SELECT COUNT(*) FROM libraries`).Scan(&n); err != nil {
		t.Fatalf("read on the pool while the writer is held: %v", err)
	}
	if n != 1 {
		t.Errorf("pool read = %d rows, want 1 (the committed seed, not the uncommitted insert)", n)
	}
}

// TestOpenReadPool_MissingPath errors rather than creating an empty database.
func TestOpenReadPool_MissingPath(t *testing.T) {
	if _, err := OpenReadPool(context.Background(), filepath.Join(t.TempDir(), "absent.db"), readPoolSize); err == nil {
		t.Fatal("OpenReadPool on a missing file succeeded; want an error")
	}
}

// TestOpenReadPool_RejectsEmptyPath names the empty path rather than letting
// the stat of "" produce a generic not-found error.
func TestOpenReadPool_RejectsEmptyPath(t *testing.T) {
	_, err := OpenReadPool(context.Background(), "", readPoolSize)
	if err == nil || !strings.Contains(err.Error(), "path must not be empty") {
		t.Fatalf("OpenReadPool(\"\") error = %v; want the empty-path error", err)
	}
}

// TestOpenReadPool_RejectsPoolSizeBelowOne refuses a size database/sql would
// read as "unlimited" (0) or clamp, on a file that is otherwise openable.
func TestOpenReadPool_RejectsPoolSizeBelowOne(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	for _, conns := range []int{0, -1} {
		pool, err := OpenReadPool(ctx, path, conns)
		if err == nil {
			_ = pool.Close()
			t.Fatalf("OpenReadPool(conns=%d) succeeded; want a pool-size error", conns)
		}
		if !strings.Contains(err.Error(), "at least 1") {
			t.Errorf("OpenReadPool(conns=%d) error = %v; want the pool-size error", conns, err)
		}
	}
}

// TestOpenReadPool_RejectsNonWALDatabase refuses a file in rollback-journal
// mode: there a reader blocks the writer's commit and vice versa, so the pool's
// premise (readers beside the single writer) does not hold.
func TestOpenReadPool_RejectsNonWALDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	var mode string
	if err := raw.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil || mode != "delete" {
		_ = raw.Close()
		t.Fatalf("set journal_mode=DELETE = %q, %v", mode, err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE t (x INTEGER)`); err != nil {
		_ = raw.Close()
		t.Fatalf("create table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	pool, err := OpenReadPool(ctx, path, readPoolSize)
	if err == nil {
		_ = pool.Close()
		t.Fatal("OpenReadPool on a rollback-journal database succeeded; want a journal_mode error")
	}
	if !strings.Contains(err.Error(), "journal_mode") {
		t.Errorf("OpenReadPool error = %v; want the journal_mode error", err)
	}
}
