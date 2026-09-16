package db

import (
	"context"
	"testing"
)

// Migration 050 (#950) adds work_queue.refused_waits. A pre-existing row must
// read 0 (no row was ever parked by DeferRefused before this column existed),
// and the migration must round-trip up, down, up with the column's presence
// tracking each direction.
func TestMigration050RefusedWaitsRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 49)

	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (artist, title, artist_key, title_key, status)
         VALUES ('A', 'T', 'a', 't', 'deferred')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := dbh.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('work_queue') WHERE name = 'refused_waits'`).Scan(&n); err != nil {
			t.Fatalf("table_info: %v", err)
		}
		return n == 1
	}

	if _, err := provider.UpTo(ctx, 50); err != nil {
		t.Fatalf("up to 50: %v", err)
	}
	var waits int
	if err := dbh.QueryRowContext(ctx, `SELECT refused_waits FROM work_queue WHERE artist_key = 'a'`).Scan(&waits); err != nil {
		t.Fatalf("read refused_waits: %v", err)
	}
	if waits != 0 {
		t.Errorf("pre-existing row refused_waits = %d; want 0", waits)
	}

	if _, err := provider.DownTo(ctx, 49); err != nil {
		t.Fatalf("down to 49: %v", err)
	}
	if hasColumn() {
		t.Fatal("refused_waits still present after down-migration")
	}
	var status string
	if err := dbh.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE artist_key = 'a'`).Scan(&status); err != nil || status != "deferred" {
		t.Fatalf("row after down = (%q, %v); want deferred row intact", status, err)
	}

	if _, err := provider.UpTo(ctx, 50); err != nil {
		t.Fatalf("re-up to 50: %v", err)
	}
	if !hasColumn() {
		t.Fatal("refused_waits missing after re-up")
	}
}
