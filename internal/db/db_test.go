package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
)

// TestOpen_CreatesDatabaseAndAppliesMigrations verifies that Open succeeds,
// returns a usable *sql.DB, and has run the initial migrations (all expected
// tables exist).
func TestOpen_CreatesDatabaseAndAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	// Verify expected tables were created by the migration.
	tables := []string{"libraries", "scan_results", "lyrics_cache", "work_queue", "api_keys", "api_key_metadata"}
	for _, tbl := range tables {
		var count int
		row := sqlDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl)
		if err := row.Scan(&count); err != nil {
			t.Errorf("query table %q: %v", tbl, err)
			continue
		}
		if count != 1 {
			t.Errorf("table %q not found after migration", tbl)
		}
	}
}

// TestOpen_WALModeEnabled verifies that the journal_mode PRAGMA was applied.
func TestOpen_WALModeEnabled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	var mode string
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q; want %q", mode, "wal")
	}
}

// TestOpen_ForeignKeysEnabled verifies that PRAGMA foreign_keys=ON was applied.
func TestOpen_ForeignKeysEnabled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	var enabled int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if enabled != 1 {
		t.Errorf("foreign_keys = %d; want 1 (ON)", enabled)
	}
}

// TestOpen_BusyTimeoutAndSynchronous verifies the remaining two pragmas set by
// Open: busy_timeout=5000ms and synchronous=NORMAL (1).
func TestOpen_BusyTimeoutAndSynchronous(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	var busy int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d; want 5000", busy)
	}

	var sync int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if sync != 1 {
		t.Errorf("synchronous = %d; want 1 (NORMAL)", sync)
	}
}

// TestOpen_EmptyPathReturnsError verifies that an empty path is rejected.
func TestOpen_EmptyPathReturnsError(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, "")
	if err == nil {
		t.Fatal("Open(\"\") returned nil error; want an error")
	}
}

// TestOpen_IdempotentMigrations verifies that opening the same DB a second time
// does not fail (goose Up is idempotent).
func TestOpen_IdempotentMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idempotent.db")

	db1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open (idempotency check): %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close second db: %v", err)
	}
}

// TestOpen_ScanResultsUniqueIndex verifies that the scan result upsert key
// migration has been applied.
func TestOpen_ScanResultsUniqueIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scan-index.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	var count int
	row := sqlDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_scan_results_library_file'")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query scan result index: %v", err)
	}
	if count != 1 {
		t.Fatalf("scan result unique index count = %d; want 1", count)
	}

	var unique int
	row = sqlDB.QueryRowContext(ctx,
		"SELECT [unique] FROM pragma_index_list('scan_results') WHERE name = 'idx_scan_results_library_file'")
	if err := row.Scan(&unique); err != nil {
		t.Fatalf("query scan result index uniqueness: %v", err)
	}
	if unique != 1 {
		t.Fatalf("scan result index unique = %d; want 1", unique)
	}

	rows, err := sqlDB.QueryContext(ctx,
		"SELECT name FROM pragma_index_info('idx_scan_results_library_file') ORDER BY seqno")
	if err != nil {
		t.Fatalf("query scan result index columns: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close index columns rows: %v", err)
		}
	}()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan result index column: %v", err)
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate scan result index columns: %v", err)
	}
	if len(cols) != 2 || cols[0] != "library_id" || cols[1] != "file_path" {
		t.Fatalf("scan result index columns = %v; want [library_id file_path]", cols)
	}
}

func TestOpen_ScanResultsOutputColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scan-outputs.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	for _, v := range []string{"outdir", "filename"} {
		var count int
		row := sqlDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pragma_table_info('scan_results') WHERE name = ?", v)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("query scan_results column %q: %v", v, err)
		}
		if count != 1 {
			t.Fatalf("scan_results column %q count = %d; want 1", v, count)
		}
	}
}

func TestOpen_WorkQueueBackoffMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "work-queue.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	columns := []string{"artist_key", "title_key", "filename", "attempts", "next_attempt_at", "last_error", "completed_at"}
	for _, v := range columns {
		var count int
		row := sqlDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = ?", v)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("query work_queue column %q: %v", v, err)
		}
		if count != 1 {
			t.Fatalf("work_queue column %q count = %d; want 1", v, count)
		}
	}

	var unique int
	row := sqlDB.QueryRowContext(ctx,
		"SELECT [unique] FROM pragma_index_list('work_queue') WHERE name = 'idx_work_queue_artist_title_key'")
	if err := row.Scan(&unique); err != nil {
		t.Fatalf("query work queue dedupe index: %v", err)
	}
	if unique != 1 {
		t.Fatalf("work queue dedupe index unique = %d; want 1", unique)
	}

	var dequeueIndexCount int
	row = sqlDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_work_queue_dequeue'")
	if err := row.Scan(&dequeueIndexCount); err != nil {
		t.Fatalf("query work queue dequeue index: %v", err)
	}
	if dequeueIndexCount != 1 {
		t.Fatalf("work queue dequeue index count = %d; want 1", dequeueIndexCount)
	}
}

// TestMigration011BackfillsKeysAndDownMigrates drives migration 011 directly:
// it migrates to v10, seeds a pre-011 row with non-ASCII metadata, applies 011,
// and asserts the artist_key/title_key backfill and index, then down-migrates
// and asserts the columns are removed.
func TestMigration011BackfillsKeysAndDownMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mig011.db")
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(1)

	migFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatalf("sub migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, migFS)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	// Migrate to v10: scan_results has no artist_key/title_key yet.
	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("UpTo(10): %v", err)
	}

	// Seed a pre-011 row with non-ASCII, space-padded metadata.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?)`, "/music", "Music"); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title) VALUES (?, ?, ?, ?)`,
		1, "/music/x.flac", "  Beyoncé ", " Café Tacvba "); err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}

	// Apply 011 and verify the best-effort backfill (lower(trim())).
	if _, err := provider.UpTo(ctx, 11); err != nil {
		t.Fatalf("UpTo(11): %v", err)
	}
	var artistKey, titleKey string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT artist_key, title_key FROM scan_results WHERE file_path = ?`, "/music/x.flac").
		Scan(&artistKey, &titleKey); err != nil {
		t.Fatalf("query keys: %v", err)
	}
	if artistKey != "beyoncé" {
		t.Errorf("artist_key = %q; want %q (lower(trim(artist)))", artistKey, "beyoncé")
	}
	if titleKey != "café tacvba" {
		t.Errorf("title_key = %q; want %q", titleKey, "café tacvba")
	}

	var idxCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_scan_results_keys'`).
		Scan(&idxCount); err != nil {
		t.Fatalf("query index: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("idx_scan_results_keys count = %d; want 1", idxCount)
	}

	// Down-migrate 011 and confirm the added columns are gone.
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var colCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('scan_results') WHERE name IN ('artist_key','title_key')`).
		Scan(&colCount); err != nil {
		t.Fatalf("query columns after down: %v", err)
	}
	if colCount != 0 {
		t.Errorf("artist_key/title_key columns after down-migration = %d; want 0", colCount)
	}
}

func TestOpen_NormalizeKeySQLFunction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "normalize-key.db")

	sqlDB, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})

	var got string
	if err := sqlDB.QueryRowContext(ctx, `SELECT normalize_key(?)`, "  Beyoncé  ").Scan(&got); err != nil {
		t.Fatalf("query normalize_key: %v", err)
	}
	if want := normalize.NormalizeKey("  Beyoncé  "); got != want {
		t.Fatalf("normalize_key SQL result = %q; want %q", got, want)
	}
}
