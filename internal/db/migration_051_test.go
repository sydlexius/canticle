package db

import (
	"context"
	"testing"
)

// Migration 051 (#982): a pre-existing row reads NULL (not examined), and the
// columns + partial index round-trip up, down, up.
func TestMigration051WordTimingRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 50)
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
	const cols = `SELECT COUNT(*) FROM pragma_table_info('work_queue')
        WHERE name IN ('word_timing_state', 'word_timing_generation', 'word_timing_checked_at')`
	const idx = `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_work_queue_word_timing'`

	if _, err := provider.UpTo(ctx, 51); err != nil {
		t.Fatalf("up to 51: %v", err)
	}
	if c, i := count(cols), count(idx); c != 3 || i != 1 {
		t.Fatalf("after up: columns=%d index=%d; want 3 and 1", c, i)
	}
	if n := count(`SELECT COUNT(*) FROM work_queue WHERE word_timing_state IS NULL AND word_timing_generation IS NULL`); n != 1 {
		t.Fatalf("pre-existing row not NULL (not examined): %d", n)
	}
	if _, err := provider.DownTo(ctx, 50); err != nil {
		t.Fatalf("down to 50: %v", err)
	}
	if c, i := count(cols), count(idx); c != 0 || i != 0 {
		t.Fatalf("after down: columns=%d index=%d; want 0 and 0", c, i)
	}
	if _, err := provider.UpTo(ctx, 51); err != nil || count(cols) != 3 {
		t.Fatalf("re-up to 51: %v, columns=%d", err, count(cols))
	}
}
