package queue

import (
	"context"
	"database/sql"
	"testing"
)

// insertSyncTierRow inserts a minimal work_queue row and returns its id.
func insertSyncTierRow(t *testing.T, dbh *sql.DB, key string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(
		`INSERT INTO work_queue (artist, title, artist_key, title_key, status, outcome_type)
         VALUES ('A', ?, 'a', ?, 'done', 'synced') RETURNING id`, key, key).Scan(&id); err != nil {
		t.Fatalf("insert row %s: %v", key, err)
	}
	return id
}

func readSyncTier(t *testing.T, dbh *sql.DB, id int64) sql.NullString {
	t.Helper()
	var tier sql.NullString
	if err := dbh.QueryRow(`SELECT sync_tier FROM work_queue WHERE id = ?`, id).Scan(&tier); err != nil {
		t.Fatalf("read sync_tier: %v", err)
	}
	return tier
}

// TestSetSyncTier covers writing each tier value, clearing to NULL via an
// empty string, and a missing id being a benign no-op (matching
// SetOutcomeType's unconditional-UPDATE contract).
func TestSetSyncTier(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)

	for _, tier := range []string{SyncTierWord, SyncTierLine, SyncTierUnsynced} {
		id := insertSyncTierRow(t, dbh, tier)
		if err := q.SetSyncTier(ctx, id, tier); err != nil {
			t.Fatalf("SetSyncTier(%q): %v", tier, err)
		}
		if got := readSyncTier(t, dbh, id); !got.Valid || got.String != tier {
			t.Errorf("sync_tier = %+v, want %q", got, tier)
		}
	}

	id := insertSyncTierRow(t, dbh, "clear")
	if err := q.SetSyncTier(ctx, id, SyncTierLine); err != nil {
		t.Fatalf("SetSyncTier: %v", err)
	}
	if err := q.SetSyncTier(ctx, id, ""); err != nil {
		t.Fatalf("SetSyncTier(clear): %v", err)
	}
	if got := readSyncTier(t, dbh, id); got.Valid {
		t.Errorf("sync_tier = %q, want NULL after clearing", got.String)
	}

	if err := q.SetSyncTier(ctx, 999999, SyncTierWord); err != nil {
		t.Fatalf("SetSyncTier on missing id: %v", err)
	}
}

// TestSetSyncTier_InvalidTier (#1075 hostile-review, Copilot 4097431615):
// an invalid tier must be refused before the UPDATE runs, leaving the column
// unchanged, matching SetWordTimingState's enum guard.
func TestSetSyncTier_InvalidTier(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)

	id := insertSyncTierRow(t, dbh, "invalid")
	if err := q.SetSyncTier(ctx, id, SyncTierLine); err != nil {
		t.Fatalf("seed SetSyncTier: %v", err)
	}

	if err := q.SetSyncTier(ctx, id, "bogus"); err == nil {
		t.Fatal("SetSyncTier(bogus): want an error for an unrecognized tier, got nil")
	}
	if got := readSyncTier(t, dbh, id); !got.Valid || got.String != SyncTierLine {
		t.Errorf("sync_tier = %+v, want unchanged %q after a rejected write", got, SyncTierLine)
	}
}

// TestSetSyncTier_DoesNotTouchWordTimingState is the design-constraint proof
// (#1075 AC): stamping the on-disk sync tier must NEVER write
// word_timing_state, which means something entirely different (a provider
// lane's verdict) and is consumed by the #1048 recheck sweep.
func TestSetSyncTier_DoesNotTouchWordTimingState(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)

	id := insertSyncTierRow(t, dbh, "t")
	if _, err := dbh.Exec(`UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = 3 WHERE id = ?`, id); err != nil {
		t.Fatalf("seed word_timing_state: %v", err)
	}
	if err := q.SetSyncTier(ctx, id, SyncTierLine); err != nil {
		t.Fatalf("SetSyncTier: %v", err)
	}

	var state sql.NullString
	var gen sql.NullInt64
	if err := dbh.QueryRow(`SELECT word_timing_state, word_timing_generation FROM work_queue WHERE id = ?`, id).Scan(&state, &gen); err != nil {
		t.Fatalf("read word_timing_state: %v", err)
	}
	if !state.Valid || state.String != "absent" || !gen.Valid || gen.Int64 != 3 {
		t.Errorf("word_timing_state/generation = %+v/%+v, want unchanged 'absent'/3", state, gen)
	}
}
