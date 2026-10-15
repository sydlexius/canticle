package db

import (
	"context"
	"testing"
)

// Migration 075 (#1559) adds work_queue.edition and swaps the two-column unique
// key for (artist_key, title_key, edition). Up keeps every existing row with
// an empty edition and a two-key collision still collapses; Down deletes edition rows,
// returns their scan results to 'pending' and restores the two-column key.
// Runs from 073 so it does not depend on what 074 holds.
func TestMigration075RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 73)
	exec := func(q string, args ...any) error {
		t.Helper()
		_, err := dbh.ExecContext(ctx, q, args...)
		return err
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	mustExec(`INSERT INTO libraries (id, path, name) VALUES (1, '/lib', 'lib')`)
	for id, path := range map[int]string{1: "/lib/a.flac", 2: "/lib/b.flac", 3: "/lib/c.flac", 4: "/lib/d.flac"} {
		mustExec(`INSERT INTO scan_results (id, library_id, file_path, status) VALUES (?, 1, ?, 'done')`, id, path)
	}
	mustExec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, status, scan_result_id)
	          VALUES (1, 'A', 'T', 'a', 't', 'done', 1)`)
	mustExec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (1, 1)`)

	if _, err := provider.UpTo(ctx, 75); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM work_queue WHERE id = 1 AND edition = ''`); n != 1 {
		t.Fatal("existing row does not have edition '' after Up")
	}
	if n := count(`SELECT COUNT(*) FROM pragma_index_list('work_queue') WHERE name = 'idx_work_queue_identity' AND [unique] = 1`); n != 1 {
		t.Fatal("unique identity index missing after Up")
	}
	// The two-key collision still collapses for edition ''.
	mustExec(`INSERT INTO work_queue (artist, title, artist_key, title_key) VALUES ('A', 'T', 'a', 't')
	          ON CONFLICT(artist_key, title_key, edition) DO UPDATE SET priority = 7`)
	if n := count(`SELECT COUNT(*) FROM work_queue WHERE artist_key = 'a' AND title_key = 't'`); n != 1 {
		t.Fatalf("rows for the key after a '' collision = %d; want 1", n)
	}
	if err := exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, edition) VALUES ('A', 'T', 'a', 't', 'karaoke')`); err == nil {
		t.Fatal("edition outside the CHECK set was accepted")
	}
	// An instrumental row beside its twin: one scan result of its own (2), one
	// shared with the '' row (1), and one only on its scan_result_id (3).
	mustExec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, edition, status, scan_result_id)
	          VALUES (2, 'A', 'T', 'a', 't', 'instrumental', 'done', 3)`)
	mustExec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (2, 2), (2, 1)`)

	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'edition'`); n != 0 {
		t.Fatal("edition column still present after Down")
	}
	if n := count(`SELECT COUNT(*) FROM work_queue`); n != 1 {
		t.Fatalf("rows after Down = %d; want only the '' row", n)
	}
	if n := count(`SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = 2`); n != 0 {
		t.Fatalf("edition row links after Down = %d; want 0", n)
	}
	for id, want := range map[int]string{1: "done", 2: "pending", 3: "pending", 4: "done"} {
		var got string
		if err := dbh.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("scan result %d after Down = %q, %v; want %q", id, got, err, want)
		}
	}
	if n := count(`SELECT COUNT(*) FROM pragma_index_list('work_queue') WHERE name = 'idx_work_queue_artist_title_key' AND [unique] = 1`); n != 1 {
		t.Fatal("two-column unique index not restored after Down")
	}
	if err := exec(`INSERT INTO work_queue (artist, title, artist_key, title_key) VALUES ('A', 'T', 'a', 't')`); err == nil {
		t.Fatal("two-column key does not reject a duplicate after Down")
	}
}
