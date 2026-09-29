package db

import (
	"context"
	"testing"
)

// Migration 053 (#553) round-trips; the trigger's disarm is covered over the
// real queue (TestMarkUpgradeQueued_FlipStampAndHold).
func TestMigration053RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 52)
	for _, step := range []func() error{
		func() error { _, err := provider.UpTo(ctx, 53); return err },
		func() error { _, err := provider.DownTo(ctx, 52); return err },
		func() error { _, err := provider.UpTo(ctx, 53); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbh.ExecContext(ctx, `INSERT INTO work_queue (artist, title, artist_key, title_key, status, upgrade_queued)
	      VALUES ('A', 'T', 'a', 't', 'pending', 1)`); err != nil {
		t.Fatalf("columns after re-up: %v", err)
	}
}
