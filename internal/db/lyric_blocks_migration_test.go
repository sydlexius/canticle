package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration069LyricBlocksUpAndDown(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m069.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)
	migFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, migFS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 69); err != nil {
		t.Fatalf("UpTo(69): %v", err)
	}
	const ins = `INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES ('a','t','f')`
	if _, err := sqlDB.ExecContext(ctx, ins); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, ins); err == nil {
		t.Error("unique (artist_key, title_key, fingerprint) not enforced")
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='lyric_blocks'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("lyric_blocks still present after Down")
	}
}
