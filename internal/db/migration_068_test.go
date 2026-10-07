package db

import (
	"context"
	"testing"
)

// Migration 068 (#1400) adds the nullable work_queue.manual_instrumental_at
// and rolls back; an existing row reads NULL (not marked).
func TestMigration068RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 67)
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist_key, title_key, artist, title, status)
        VALUES ('k', 'k', 'a', 't', 'done')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 68); err != nil {
		t.Fatal(err)
	}
	var marked int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE manual_instrumental_at IS NOT NULL`).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("marked rows after up = (%d, %v); want 0", marked, err)
	}
	if _, err := provider.DownTo(ctx, 67); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx, `SELECT manual_instrumental_at FROM work_queue`); err == nil {
		t.Error("column survived the down migration")
	}
}
