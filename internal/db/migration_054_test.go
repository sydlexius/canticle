package db

import (
	"context"
	"testing"
)

// Migration 054 (#1007) round-trips. Its queue-side readers were removed (#1008);
// the columns stay until their own migration drops them. The INDEX stays too because
// it is live: the upgrade sweep's mis_synced arm uses idx_work_queue_word_generate_missynced,
// so that later migration must replace it with one on (timing_outcome, status).
func TestMigration054RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 53)
	for _, step := range []func() error{
		func() error { _, err := provider.UpTo(ctx, 54); return err },
		func() error { _, err := provider.DownTo(ctx, 53); return err },
		func() error { _, err := provider.UpTo(ctx, 54); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist, title, artist_key, title_key, status, word_generate_version, word_generate_at)
	      VALUES ('A', 'T', 'a', 't', 'done', 1, '2026-09-29T00:00:00Z')`); err != nil {
		t.Fatalf("columns after re-up: %v", err)
	}
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_work_queue_word_generate_missynced'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index count = %d, %v; want 1", n, err)
	}
}
