package db

import (
	"context"
	"testing"
)

// Migration 052 (#1075): a pre-existing row reads NULL (not yet classified),
// its data survives the round trip, and the column round-trips up, down, up.
// No index assertion here, unlike 051's mirror: this migration ships NO
// index at all (a partial index was removed before merge -- EXPLAIN QUERY
// PLAN showed the planner never chose it over idx_work_queue_dequeue, #1075
// hostile-review finding 7; see the migration's own comment).
func TestMigration052SyncTierRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 51)
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (artist, title, artist_key, title_key, status) VALUES ('A', 'T', 'a', 't', 'done')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := dbh.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	const col = `SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'sync_tier'`

	if _, err := provider.UpTo(ctx, 52); err != nil {
		t.Fatalf("up to 52: %v", err)
	}
	if c := count(col); c != 1 {
		t.Fatalf("after up: sync_tier column=%d; want 1", c)
	}
	if n := count(`SELECT COUNT(*) FROM work_queue WHERE sync_tier IS NULL`); n != 1 {
		t.Fatalf("pre-existing row not NULL (not yet classified): %d", n)
	}
	var artist, title, status string
	if err := dbh.QueryRowContext(ctx,
		`SELECT artist, title, status FROM work_queue`).Scan(&artist, &title, &status); err != nil {
		t.Fatalf("read seeded row after up: %v", err)
	}
	if artist != "A" || title != "T" || status != "done" {
		t.Fatalf("seeded row mutated by the up migration: artist=%q title=%q status=%q", artist, title, status)
	}

	if _, err := provider.DownTo(ctx, 51); err != nil {
		t.Fatalf("down to 51: %v", err)
	}
	if c := count(col); c != 0 {
		t.Fatalf("after down: sync_tier column=%d; want 0", c)
	}
	if n := count(`SELECT COUNT(*) FROM work_queue WHERE artist = 'A' AND title = 'T' AND status = 'done'`); n != 1 {
		t.Fatalf("seeded row lost across down: %d", n)
	}

	if _, err := provider.UpTo(ctx, 52); err != nil || count(col) != 1 {
		t.Fatalf("re-up to 52: %v, sync_tier column=%d", err, count(col))
	}
}
