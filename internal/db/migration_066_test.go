package db

import (
	"context"
	"testing"
)

// Migration 066 (#1301) adds source_event_daily. It applies, accepts the
// documented events only, enforces the (day, lane, event) key, and rolls back.
func TestMigration066RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 65)
	hasTable := func() (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'source_event_daily'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := provider.UpTo(ctx, 66); err != nil {
		t.Fatal(err)
	}
	if hasTable() != 1 {
		t.Fatal("source_event_daily missing after Up")
	}
	ins := func(event string) error {
		_, err := dbh.ExecContext(ctx, `INSERT INTO source_event_daily(day, lane, event, count) VALUES('2026-10-06', 'musixmatch', ?, 1)`, event)
		return err
	}
	if err := ins("line"); err != nil {
		t.Fatalf("valid event refused: %v", err)
	}
	if ins("line") == nil {
		t.Fatal("duplicate (day, lane, event) accepted")
	}
	if ins("bogus") == nil {
		t.Fatal("unknown event accepted")
	}
	if _, err := provider.DownTo(ctx, 65); err != nil {
		t.Fatal(err)
	}
	if hasTable() != 0 {
		t.Fatal("source_event_daily still present after Down")
	}
}
