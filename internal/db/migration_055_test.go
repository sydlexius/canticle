package db

import (
	"context"
	"database/sql"
	"testing"
)

// Migration 055 (#1120) round-trips: both columns exist after up, are gone
// after down (existing rows survive), and return after re-up. Selection over
// them is covered over the real queue (internal/queue/upgrade_sweep_test.go).
func TestMigration055RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 54)
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist, title, artist_key, title_key, status, timing_outcome)
	      VALUES ('A', 'T', 'a', 't', 'done', 'mis_synced')`); err != nil {
		t.Fatal(err)
	}
	cols := func() (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('work_queue')
		      WHERE name IN ('timing_stamp_source', 'missync_recheck_generation')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, step := range []struct {
		run  func() error
		want int
	}{
		{func() error { _, err := provider.UpTo(ctx, 55); return err }, 2},
		{func() error { _, err := provider.DownTo(ctx, 54); return err }, 0},
		{func() error { _, err := provider.UpTo(ctx, 55); return err }, 2},
	} {
		if err := step.run(); err != nil {
			t.Fatal(err)
		}
		if got := cols(); got != step.want {
			t.Fatalf("new columns = %d, want %d", got, step.want)
		}
	}
	var src sql.NullString
	var gen sql.NullInt64
	var outcome string
	if err := dbh.QueryRowContext(ctx, `SELECT timing_outcome, timing_stamp_source, missync_recheck_generation FROM work_queue`).
		Scan(&outcome, &src, &gen); err != nil || outcome != "mis_synced" || src.Valid || gen.Valid {
		t.Fatalf("row after round trip = %q, %+v, %+v, %v; want mis_synced, NULL, NULL", outcome, src, gen, err)
	}
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET timing_stamp_source = 'sweep', missync_recheck_generation = 7`); err != nil {
		t.Fatalf("columns not writable after re-up: %v", err)
	}
}
