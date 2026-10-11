// Package dbtest opens fully migrated SQLite databases for tests without
// re-running every migration per database.
//
// The migrated image is built once per test binary from the real db.Open (so
// it always tracks the embedded migrations) and kept in memory. Each caller
// gets a private file written from those bytes, then opened with the real
// db.Open, where goose Up is a no-op. Tests whose point is the migration
// itself, the read-only path, or an empty database must keep calling db.Open.
//
// This package is for _test.go files only; production code must not import it.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// template returns the migrated database image, building it on first use.
func template() ([]byte, error) {
	templateOnce.Do(func() {
		templateBytes, templateErr = buildTemplate()
	})
	return templateBytes, templateErr
}

func buildTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "dbtest-template-*")
	if err != nil {
		return nil, fmt.Errorf("dbtest: create template dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "template.db")
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("dbtest: build template: %w", err)
	}
	// Fold the WAL into the main file so the .db alone is the whole image.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("dbtest: checkpoint template: %w", err)
	}
	if err := sqlDB.Close(); err != nil {
		return nil, fmt.Errorf("dbtest: close template: %w", err)
	}
	if err := requireNoWAL(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // reason: path is a temp file this function created
	if err != nil {
		return nil, fmt.Errorf("dbtest: read template: %w", err)
	}
	return b, nil
}

// requireNoWAL fails when a non-empty WAL remains beside the database at path.
// The image is only the .db file; a surviving WAL would hold committed pages
// the image does not contain, so fail loudly instead of shipping a silently
// truncated template.
func requireNoWAL(path string) error {
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("dbtest: stat template WAL: %w", err)
	}
	if fi.Size() > 0 {
		return fmt.Errorf("dbtest: template still has a %d-byte WAL after checkpoint and close", fi.Size())
	}
	return nil
}

// materialize writes the migrated image to path when path does not exist yet.
// An existing file is left alone so a test can close and reopen its database.
func materialize(path string) error {
	if path == "" {
		return fmt.Errorf("dbtest: path must not be empty")
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("dbtest: stat %s: %w", path, err)
	}
	b, err := template()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("dbtest: create data dir: %w", err)
	}
	return writeExclusive(path, b)
}

// writeExclusive creates path with b, mode 0644 (what a fresh db.Open creates,
// minus the umask). O_EXCL means two callers racing on one path cannot both
// write it: the loser gets nil and opens the winner's file. A failed write
// removes the partial file.
func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644) //nolint:gosec // reason: test database; mode must match a fresh db.Open
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("dbtest: create %s: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("dbtest: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("dbtest: close %s: %w", path, err)
	}
	return nil
}

// OpenAt has the shape of db.Open: it opens the database at path, first
// seeding the file from the migrated template when it does not exist. The
// caller owns the returned handle.
//
// A path that already exists is never overwritten, including a zero-byte file:
// that file is handed to the real db.Open, which migrates it from scratch. A
// caller relying on "an empty file means a fresh full migration" therefore
// keeps exactly that behavior.
//
// The path must name a file. An empty path, ":memory:" and any "file:" URI
// cannot be seeded by writing a file, so OpenAt fails the test (t.Fatalf)
// rather than silently opening an unseeded database.
func OpenAt(ctx context.Context, t testing.TB, path string) (*sql.DB, error) {
	t.Helper()
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		t.Fatalf("dbtest.OpenAt: path %q cannot be seeded from the template (empty, :memory: and file: URIs are unsupported); use db.Open", path)
	}
	if err := materialize(path); err != nil {
		return nil, err
	}
	return db.Open(ctx, path)
}

// Open returns a migrated database under t.TempDir(), closed on cleanup.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	sqlDB, err := OpenAt(context.Background(), t, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("dbtest.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// Path returns the path of a migrated database copy under t.TempDir() without
// opening it, for tests that hand a path to the code under test.
func Path(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	if err := materialize(path); err != nil {
		t.Fatalf("dbtest.Path: %v", err)
	}
	return path
}
