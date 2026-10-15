package db

import (
	"context"
	"testing"
)

// Migration 074 (#1597) adds the nullable work_queue.source_gone_at column. It
// applies, keeps an existing row's data (NULL source_gone_at), and rolls back.
func TestMigration074RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 73)
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (id, artist, title, artist_key, title_key, status)
	      VALUES (1, 'A', 'T', 'a', 't', 'done')`); err != nil {
		t.Fatal(err)
	}
	hasCol := func() (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'source_gone_at'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := provider.UpTo(ctx, 74); err != nil {
		t.Fatal(err)
	}
	if hasCol() != 1 {
		t.Fatal("source_gone_at column missing after Up")
	}
	var up *string
	if err := dbh.QueryRowContext(ctx, `SELECT source_gone_at FROM work_queue WHERE id = 1`).Scan(&up); err != nil || up != nil {
		t.Fatalf("existing row source_gone_at = %v, %v; want NULL", up, err)
	}
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET source_gone_at = '2026-01-01T00:00:00Z' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 73); err != nil {
		t.Fatal(err)
	}
	if hasCol() != 0 {
		t.Fatal("source_gone_at column still present after Down")
	}
	var status string
	if err := dbh.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = 1`).Scan(&status); err != nil || status != "done" {
		t.Fatalf("row after Down = %q, %v", status, err)
	}
}
