package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// triggerBusy forces a real SQLITE_BUSY by holding a write transaction on one
// connection (busy_timeout=0) and attempting a write on a second connection to
// the same file. Returns the resulting busy error.
func triggerBusy(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := path + "?_pragma=busy_timeout(0)"

	a, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.SetMaxOpenConns(1)

	b, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.SetMaxOpenConns(1)

	ctx := context.Background()
	if _, err := a.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS t (id INTEGER)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin a: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("write a (acquire write lock): %v", err)
	}

	// B attempts a write while A holds the write lock -> SQLITE_BUSY immediately
	// because busy_timeout is 0.
	_, err = b.ExecContext(ctx, "INSERT INTO t (id) VALUES (2)")
	if err == nil {
		t.Fatal("expected SQLITE_BUSY on second writer, got nil")
	}
	return err
}

func TestIsSQLiteBusy(t *testing.T) {
	busy := triggerBusy(t)
	if !IsSQLiteBusy(busy) {
		t.Fatalf("IsSQLiteBusy(%v) = false; want true", busy)
	}
	if !IsSQLiteBusy(fmt.Errorf("scan: upsert x: %w", busy)) {
		t.Fatal("IsSQLiteBusy(wrapped busy) = false; want true")
	}
	if IsSQLiteBusy(errors.New("not a busy error")) {
		t.Fatal("IsSQLiteBusy(plain error) = true; want false")
	}
	if IsSQLiteBusy(nil) {
		t.Fatal("IsSQLiteBusy(nil) = true; want false")
	}
}

func TestRetryOnBusy_RetriesThenSucceeds(t *testing.T) {
	busy := triggerBusy(t)
	attempts := 0
	err := RetryOnBusy(context.Background(), 5, func() error {
		attempts++
		if attempts < 3 {
			return busy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryOnBusy = %v; want nil", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d; want 3", attempts)
	}
}

func TestRetryOnBusy_NonBusyReturnsImmediately(t *testing.T) {
	sentinel := errors.New("hard failure")
	attempts := 0
	err := RetryOnBusy(context.Background(), 5, func() error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RetryOnBusy = %v; want sentinel", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d; want 1 (no retry on non-busy error)", attempts)
	}
}

func TestRetryOnBusy_ExhaustsReturnsLastBusy(t *testing.T) {
	busy := triggerBusy(t)
	attempts := 0
	err := RetryOnBusy(context.Background(), 3, func() error {
		attempts++
		return busy
	})
	if !IsSQLiteBusy(err) {
		t.Fatalf("RetryOnBusy = %v; want busy after exhaustion", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d; want 3 (maxAttempts)", attempts)
	}
}
