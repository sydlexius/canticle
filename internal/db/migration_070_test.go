package db

import (
	"context"
	"testing"
)

// Migration 070 (#511) adds the nullable work_queue.gate_complete column. It
// applies, keeps an existing row's data (NULL gate_complete), and rolls back.
func TestMigration070RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 69)
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (id, artist, title, artist_key, title_key, status)
	      VALUES (1, 'A', 'T', 'a', 't', 'done')`); err != nil {
		t.Fatal(err)
	}
	hasCol := func() (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'gate_complete'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := provider.UpTo(ctx, 70); err != nil {
		t.Fatal(err)
	}
	if hasCol() != 1 {
		t.Fatal("gate_complete column missing after Up")
	}
	var up *int
	if err := dbh.QueryRowContext(ctx, `SELECT gate_complete FROM work_queue WHERE id = 1`).Scan(&up); err != nil || up != nil {
		t.Fatalf("existing row gate_complete = %v, %v; want NULL", up, err)
	}
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET gate_complete = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 69); err != nil {
		t.Fatal(err)
	}
	if hasCol() != 0 {
		t.Fatal("gate_complete column still present after Down")
	}
	var status string
	if err := dbh.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = 1`).Scan(&status); err != nil || status != "done" {
		t.Fatalf("row after Down = %q, %v", status, err)
	}
}
