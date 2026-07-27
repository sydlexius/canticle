package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// openAtVersion opens a fresh database migrated to exactly `version`, returning
// the handle and the goose provider so the caller can step it forward.
//
// This exists so a migration test RUNS THE MIGRATION FILE. An earlier version of
// this test opened a fully-migrated DB, inserted rows after the fact, and
// hand-executed its own copy of the UPDATE -- which meant it asserted that SQLite
// applies a WHERE clause correctly and could not fail if the .sql file were
// edited or emptied. Verified: replacing 038's body with `SELECT 1;` still passed.
func openAtVersion(t *testing.T, version int64) (*sql.DB, *goose.Provider) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	migFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatalf("sub migrations fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, migFS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return sqlDB, provider
}

// Migration 038 re-keys stored detector verdicts from the app version to the
// model identity (#684). Two behaviors matter, and both are asserted against the
// REAL migration: it rewrites rows that carry a verdict, and it leaves rows that
// never had one alone -- a NULL detector_version means "never detected" and must
// keep meaning that, since writing a version onto a scoreless row would claim a
// verdict that was never computed.
func TestMigration038RekeysOnlyRowsWithAVerdict(t *testing.T) {
	const modelKey = "b80da2a1a56926fb0767205051a200dd7b3beaf3ea1ea126c42a53943996e5e0"

	ctx := context.Background()
	dbh, provider := openAtVersion(t, 37) // the state a real deployment upgrades FROM

	insert := `INSERT INTO work_queue (artist_key, title_key, artist, title, source_path, status, detector_version, instrumental_result)
	           VALUES (?, ?, ?, ?, ?, 'deferred', ?, ?)`
	rows := []struct {
		key       string
		version   any
		verdict   any
		wantModel bool // true = must be re-keyed to the model key
	}{
		{"a1", "1.14.0", 0, true},    // old app version + not-instrumental verdict
		{"a2", "1.30.0", 1, true},    // current app version + instrumental verdict
		{"a3", nil, nil, false},      // never detected -> untouched
		{"a4", "1.28.0", nil, false}, // version but NO verdict -> untouched
		// Pre-migration-025: instrumental_result existed before the telemetry
		// columns did, so a real population carries a verdict with a NULL
		// detector_version. There is no version to re-key, and inventing one would
		// claim a model produced a score it never saw.
		{"a5", nil, 1, false},
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert, r.key, r.key, r.key, r.key, "/m/"+r.key, r.version, r.verdict); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
	}

	// Run the migration under test -- the actual .sql file, not a copy of it.
	if _, err := provider.UpTo(ctx, 38); err != nil {
		t.Fatalf("migrate to 38: %v", err)
	}

	for _, r := range rows {
		var got *string
		if err := dbh.QueryRowContext(ctx,
			`SELECT detector_version FROM work_queue WHERE artist_key = ?`, r.key).Scan(&got); err != nil {
			t.Fatalf("select %s: %v", r.key, err)
		}
		switch {
		case r.wantModel && (got == nil || *got != modelKey):
			t.Errorf("%s: detector_version = %v; want the model key. A verdict-bearing row must be "+
				"re-keyed, or it re-infers and the disks stay awake (#684)", r.key, deref(got))
		case !r.wantModel && got != nil && *got == modelKey:
			t.Errorf("%s: detector_version was re-keyed to the model key, but this row has no verdict; "+
				"a row that was never detected must not claim one", r.key)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}
