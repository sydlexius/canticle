package db

import (
	"context"
	"database/sql"
	"testing"
)

// Migration 067 (#1285) adds work_queue.failure_class, backfills it for every
// status that can hold a failure, installs the clearing trigger, and rolls back.
func TestMigration067RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 66)
	want := map[string]string{"pending": "", "processing": "network", "deferred": "network",
		"failed": "network", "unavailable": "network", "done": ""}
	for status := range want {
		if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist_key, title_key, artist, title, status, last_error)
            VALUES (?, 'k', 'a', 't', ?, 'lane a: connection refused')`, status, status); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}
	// A whitespace-only message is no reason: the backfill stamps none, not other.
	for key, msg := range map[string]string{"blank spaces": "   ", "blank tab": "\t", "blank newline": "\n"} {
		if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist_key, title_key, artist, title, status, last_error)
            VALUES (?, 'k', 'a', 't', 'failed', ?)`, key, msg); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		want[key] = "none"
	}
	if _, err := provider.UpTo(ctx, 67); err != nil {
		t.Fatal(err)
	}
	class := func(status string) string {
		t.Helper()
		var c sql.NullString
		if err := dbh.QueryRowContext(ctx, `SELECT failure_class FROM work_queue WHERE artist_key = ?`, status).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c.String
	}
	for status, w := range want {
		if got := class(status); got != w {
			t.Errorf("backfill, status %s: failure_class = %q, want %q", status, got, w)
		}
	}
	for row, stmt := range map[string]string{
		"failed":   `UPDATE work_queue SET status = 'done' WHERE artist_key = 'failed'`,
		"deferred": `UPDATE work_queue SET last_error = '' WHERE artist_key = 'deferred'`,
	} {
		if _, err := dbh.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
		if got := class(row); got != "" {
			t.Errorf("trigger, %s row: failure_class = %q after %s, want it cleared", row, got, stmt)
		}
	}
	if _, err := provider.DownTo(ctx, 66); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'failure_class')
        + (SELECT COUNT(*) FROM sqlite_master WHERE name = 'clear_work_queue_failure_class')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("failure_class column or its trigger still present after Down (%d)", n)
	}
}
