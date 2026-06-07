package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenReadOnly_ReadsWithoutMigratingOrCreating verifies that OpenReadOnly
// opens an existing, already-migrated database for queries but does NOT run
// migrations or otherwise mutate it: writes are rejected (query_only), so a
// side-effect-free caller such as shell completion cannot alter schema or data.
func TestOpenReadOnly_ReadsWithoutMigratingOrCreating(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	// Seed a real, migrated database with one row.
	rw, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := rw.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, "/music", "MyMusic"); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	// Reads work.
	var name string
	if err := ro.QueryRowContext(ctx, `SELECT name FROM libraries WHERE path = ?`, "/music").Scan(&name); err != nil {
		t.Fatalf("read query: %v", err)
	}
	if name != "MyMusic" {
		t.Fatalf("name = %q; want MyMusic", name)
	}

	// Writes are rejected: the connection is query-only.
	if _, err := ro.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, "/more", "More"); err == nil {
		t.Fatal("write through OpenReadOnly succeeded; want a query_only rejection")
	}
}

// TestOpenReadOnly_MissingPath errors rather than creating a database, so a
// completion tab-press against a never-initialized config cannot spawn a DB.
func TestOpenReadOnly_MissingPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := OpenReadOnly(ctx, path); err == nil {
		t.Fatal("OpenReadOnly on a missing file succeeded; want an error")
	}
}
