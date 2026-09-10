package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 049 (#477) backfills ONLY status='done' AND last_error exactly
// 'miss limit reached' to 'unavailable'; every other row shape must survive
// unchanged. Asserted against the REAL migration via openAtVersion.
func TestMigration049BackfillsUnavailableStatus(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 48) // the state a real deployment upgrades FROM

	const sentinel = "miss limit reached"

	insert := `INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, attempts, miss_count, completed_at, updated_at)
	           VALUES (?, ?, ?, 'a', 't', '/x.mp3', ?, ?, ?, ?, ?, '2026-09-01T00:00:00Z')`
	for _, r := range []struct {
		id                  int64
		key, status, err    string
		attempts, missCount int
		completedAt         any
	}{
		// THE POPULATION THIS MIGRATION EXISTS TO CONVERT.
		{901, "k1", "done", sentinel, 0, 15, "2026-08-01T00:00:00Z"},
		{902, "k2", "done", sentinel, 0, 1, "2026-08-02T00:00:00Z"}, // max_miss_attempts=1 shape (#477 comment)
		// ROWS THAT MUST SURVIVE UNTOUCHED.
		{903, "k3", "done", "", 0, 0, "2026-08-03T00:00:00Z"},                                 // genuine completion, no error
		{904, "k4", "done", "some other error text", 0, 0, "2026-08-04T00:00:00Z"},            // done with an unrelated error
		{905, "k5", "done", "prefix miss limit reached suffix", 0, 0, "2026-08-05T00:00:00Z"}, // substring match must NOT trigger (exact-equality predicate)
		{906, "k6", "failed", sentinel, 3, 0, nil},                                            // wrong status: never converts regardless of last_error
		{907, "k7", "deferred", sentinel, 0, 2, nil},                                          // wrong status: never converts
		{908, "k8", "processing", sentinel, 0, 0, nil},                                        // wrong status: never converts
		{909, "k9", "pending", "", 0, 0, nil},
	} {
		if _, err := dbh.ExecContext(ctx, insert, r.id, r.key, r.key, r.status, r.err, r.attempts, r.missCount, r.completedAt); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	if _, err := provider.UpTo(ctx, 49); err != nil {
		t.Fatalf("migrate to 49: %v", err)
	}

	type row struct {
		status, lastError string
	}
	get := func(id int64) row {
		var r row
		if err := dbh.QueryRowContext(ctx, `SELECT status, last_error FROM work_queue WHERE id = ?`, id).Scan(&r.status, &r.lastError); err != nil {
			t.Fatalf("select %d: %v", id, err)
		}
		return r
	}

	// THE CONVERTED ROWS: status flips, last_error is untouched (the sentinel
	// text is what RecheckRetired's predicate matches after this migration).
	for _, id := range []int64{901, 902} {
		got := get(id)
		if got.status != "unavailable" {
			t.Errorf("row %d status = %q; want %q", id, got.status, "unavailable")
		}
		if got.lastError != sentinel {
			t.Errorf("row %d last_error = %q; want unchanged %q", id, got.lastError, sentinel)
		}
	}

	// THE UNTOUCHED ROWS, each a distinct way an over-broad predicate goes wrong.
	for _, tc := range []struct {
		id                  int64
		wantStatus, wantErr string
		why                 string
	}{
		{903, "done", "", "a genuine completion with no error must never be reclassified"},
		{904, "done", "some other error text", "a done row with an unrelated error must never be reclassified"},
		{905, "done", "prefix miss limit reached suffix", "the predicate is EXACT EQUALITY on last_error, not a substring match"},
		{906, "failed", sentinel, "wrong status: a failed row is never routed to unavailable regardless of last_error text"},
		{907, "deferred", sentinel, "wrong status: a deferred row is never routed to unavailable regardless of last_error text"},
		{908, "processing", sentinel, "wrong status: an in-flight row is never touched"},
		{909, "pending", "", "an ordinary pending row is untouched"},
	} {
		got := get(tc.id)
		if got.status != tc.wantStatus || got.lastError != tc.wantErr {
			t.Errorf("row %d (%s): got {status=%q, last_error=%q}; want {status=%q, last_error=%q}",
				tc.id, tc.why, got.status, got.lastError, tc.wantStatus, tc.wantErr)
		}
	}

	// THE CHECK CONSTRAINT accepts 'unavailable' now.
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (artist_key, title_key, artist, title, status) VALUES ('z','z','a','t','unavailable')`); err != nil {
		t.Errorf("insert with status='unavailable' rejected after migration 049: %v", err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (artist_key, title_key, artist, title, status) VALUES ('zz','zz','a','t','bogus')`); err == nil {
		t.Errorf("insert with an unrecognized status was accepted; CHECK constraint did not survive the rebuild")
	}
}

// TestMigration049PreservesSchemaShape asserts the rebuild carries every
// column, index, and trigger through unchanged.
func TestMigration049PreservesSchemaShape(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 48)

	wantCols := columnNames(t, ctx, dbh, "work_queue")

	if _, err := provider.UpTo(ctx, 49); err != nil {
		t.Fatalf("migrate to 49: %v", err)
	}

	gotCols := columnNames(t, ctx, dbh, "work_queue")
	if len(gotCols) != len(wantCols) {
		t.Fatalf("column count after migration 049 = %d; want %d (before: %v, after: %v)",
			len(gotCols), len(wantCols), wantCols, gotCols)
	}
	for i, name := range wantCols {
		if gotCols[i] != name {
			t.Errorf("column %d = %q; want %q (order/identity must be preserved)", i, gotCols[i], name)
		}
	}

	// The known pre-rebuild index set (migrations 001/026/031 and the v12 rebuild).
	wantIdx := []string{
		"idx_work_queue_artist_title_key",
		"idx_work_queue_batch_seq",
		"idx_work_queue_dequeue",
		"idx_work_queue_scan_result",
		"idx_work_queue_source_path",
	}
	gotIdx := objectNames(t, ctx, dbh, "index", "work_queue")
	if len(gotIdx) != len(wantIdx) {
		t.Fatalf("index count after migration 049 = %d; want %d (got: %v, want: %v)",
			len(gotIdx), len(wantIdx), gotIdx, wantIdx)
	}
	for i, name := range wantIdx {
		if gotIdx[i] != name {
			t.Errorf("index %d = %q; want %q", i, gotIdx[i], name)
		}
	}

	gotTrg := objectNames(t, ctx, dbh, "trigger", "work_queue")
	if len(gotTrg) != 1 || gotTrg[0] != "update_work_queue_updated_at" {
		t.Errorf("triggers after migration 049 = %v; want [update_work_queue_updated_at]", gotTrg)
	}
}

// TestMigration049DownReversesStatusAndCheck asserts the Down migration
// reclassifies 'unavailable' back to 'done' (last_error is left as-is -- it
// already carries the pre-migration sentinel shape) and restores the
// original CHECK constraint (rejecting 'unavailable' again).
func TestMigration049DownReversesStatusAndCheck(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 49)

	const sentinel = "miss limit reached"
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (id, artist_key, title_key, artist, title, status, last_error)
		 VALUES (1001, 'k', 'k', 'a', 't', 'unavailable', ?)`, sentinel); err != nil {
		t.Fatalf("insert unavailable row: %v", err)
	}
	// A row untouched by this migration must also survive the down-migration
	// untouched.
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (id, artist_key, title_key, artist, title, status, last_error)
		 VALUES (1002, 'k2', 'k2', 'a', 't', 'done', '')`); err != nil {
		t.Fatalf("insert done row: %v", err)
	}

	if _, err := provider.DownTo(ctx, 48); err != nil {
		t.Fatalf("DownTo(48): %v", err)
	}

	var status1, lastError1 string
	if err := dbh.QueryRowContext(ctx, `SELECT status, last_error FROM work_queue WHERE id = 1001`).Scan(&status1, &lastError1); err != nil {
		t.Fatalf("select 1001: %v", err)
	}
	if status1 != "done" {
		t.Errorf("row 1001 status after Down = %q; want %q", status1, "done")
	}
	if lastError1 != sentinel {
		t.Errorf("row 1001 last_error after Down = %q; want unchanged %q", lastError1, sentinel)
	}

	var status2 string
	if err := dbh.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = 1002`).Scan(&status2); err != nil {
		t.Fatalf("select 1002: %v", err)
	}
	if status2 != "done" {
		t.Errorf("row 1002 status after Down = %q; want unchanged %q", status2, "done")
	}

	// The CHECK constraint no longer accepts 'unavailable'.
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (artist_key, title_key, artist, title, status) VALUES ('z','z','a','t','unavailable')`); err == nil {
		t.Errorf("insert with status='unavailable' was accepted after Down; CHECK constraint was not restored")
	}
}

// columnNames returns table's column names in declared order.
func columnNames(t *testing.T, ctx context.Context, dbh *sql.DB, table string) []string {
	return queryNames(t, ctx, dbh, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
}

// objectNames returns the sorted names of objType (index/trigger) on table.
func objectNames(t *testing.T, ctx context.Context, dbh *sql.DB, objType, table string) []string {
	return queryNames(t, ctx, dbh,
		`SELECT name FROM sqlite_master WHERE type = ? AND tbl_name = ? ORDER BY name`, objType, table)
}

func queryNames(t *testing.T, ctx context.Context, dbh *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := dbh.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return names
}

// openAtVersionAppPragmas is openAtVersion with db.Open's connection settings
// (one connection, foreign_keys=ON), so the rebuild's FK toggle runs as in prod.
func openAtVersionAppPragmas(t *testing.T, version int64) (*sql.DB, *goose.Provider) {
	t.Helper()
	dbh, provider := openAtVersion(t, 1)
	dbh.SetMaxOpenConns(1)
	if _, err := dbh.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return dbh, provider
}

// TestMigration049KeepsChildRowsAndAutoincrement pins two rebuild hazards in
// both directions: with foreign_keys=ON, DROP TABLE work_queue would cascade
// into work_queue_scan_results unless the migration turns FKs off; and
// INSERT...SELECT into the new table resets the AUTOINCREMENT counter to the
// max surviving id, so a deleted row's id would be reissued unless the
// migration carries the counter over.
func TestMigration049KeepsChildRowsAndAutoincrement(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 48)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbh.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string) int64 {
		t.Helper()
		var n int64
		if err := dbh.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	exec(`INSERT INTO libraries (id, path, name) VALUES (1, '/lib', 'lib')`)
	for i := 1; i <= 10; i++ {
		exec(`INSERT INTO scan_results (id, library_id, file_path) VALUES (?, 1, ?)`, i, fmt.Sprintf("/lib/%d.mp3", i))
		exec(`INSERT INTO work_queue (id, artist_key, title_key, artist, title) VALUES (?, ?, ?, 'a', 't')`, i, i, i)
		exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, i, i)
	}
	// Push the counter to 1000, then delete it so max surviving id (10) < counter.
	exec(`INSERT INTO work_queue (id, artist_key, title_key, artist, title) VALUES (1000, 'top', 'top', 'a', 't')`)
	exec(`DELETE FROM work_queue WHERE id = 1000`)

	var counter int64 = 1000
	check := func(stage string) {
		t.Helper()
		if n := count(`SELECT COUNT(*) FROM work_queue_scan_results`); n != 10 {
			t.Fatalf("%s: work_queue_scan_results rows = %d; want 10 (FK cascade on DROP TABLE)", stage, n)
		}
		if n := count(`SELECT COUNT(*) FROM sqlite_sequence WHERE name LIKE 'work_queue%'`); n != 1 {
			t.Fatalf("%s: sqlite_sequence has %d work_queue* rows; want exactly 1", stage, n)
		}
		if seq := count(`SELECT seq FROM sqlite_sequence WHERE name = 'work_queue'`); seq != counter {
			t.Fatalf("%s: work_queue sequence = %d; want carried-over %d", stage, seq, counter)
		}
		var id int64
		key := "new-" + stage
		if err := dbh.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist_key, title_key, artist, title) VALUES (?, ?, 'a', 't') RETURNING id`,
			key, key).Scan(&id); err != nil {
			t.Fatalf("%s: insert: %v", stage, err)
		}
		if id <= counter {
			t.Fatalf("%s: new id = %d; want > %d (a deleted row's id was reissued)", stage, id, counter)
		}
		counter = id
		// Delete it again so the next rebuild also starts with max id < counter.
		exec(`DELETE FROM work_queue WHERE id = ?`, id)
	}

	if _, err := provider.UpTo(ctx, 49); err != nil {
		t.Fatalf("migrate to 49: %v", err)
	}
	check("up")
	if _, err := provider.DownTo(ctx, 48); err != nil {
		t.Fatalf("DownTo(48): %v", err)
	}
	check("down")
}

// TestMigration049NoSequenceRow covers a work_queue with no sqlite_sequence
// row (a fresh v48 database has one at seq 0, so the test removes it): there
// is no counter to carry, and the rebuild must not invent one.
func TestMigration049NoSequenceRow(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 48)
	if _, err := dbh.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'work_queue'`); err != nil {
		t.Fatalf("clear sequence: %v", err)
	}
	if _, err := provider.UpTo(ctx, 49); err != nil {
		t.Fatalf("migrate to 49: %v", err)
	}
	var n int64
	if err := dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_sequence WHERE name LIKE 'work_queue%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("sqlite_sequence work_queue* rows = %d; want 0", n)
	}
	var id int64
	if err := dbh.QueryRowContext(ctx,
		`INSERT INTO work_queue (artist_key, title_key, artist, title) VALUES ('k', 'k', 'a', 't') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 1 {
		t.Fatalf("first id = %d; want 1", id)
	}
}
