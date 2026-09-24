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

// TestSetSyncTierIfPending: the guarded backfill write (#1075 finding 3)
// applies only if the row raced (reopened/refetched/resettled/missing) since
// ListSyncTierPending listed it; SetSyncTier has no such guard. Also shares
// SetSyncTier's invalid-tier guard (Copilot 4097431615).
func TestSetSyncTierIfPending(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	word, line, none := sql.NullString{String: SyncTierWord, Valid: true}, sql.NullString{String: SyncTierLine, Valid: true}, sql.NullString{}
	pending := insertSyncTierRow(t, dbh, "pending")
	alreadyWord := insertSyncTierRow(t, dbh, "already-word")
	_ = q.SetSyncTier(ctx, alreadyWord, SyncTierWord)
	reopened := insertSyncTierRow(t, dbh, "reopened")
	_, _ = dbh.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, reopened)
	for _, tc := range []struct {
		name        string
		id          int64
		wantApplied bool
		wantTier    sql.NullString
	}{
		{"pending row applies", pending, true, line},
		{"already-classified: skipped, kept", alreadyWord, false, word},
		{"reopened (status != done): skipped", reopened, false, none},
		{"missing id: no-op", 999999, false, none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied, err := q.SetSyncTierIfPending(ctx, tc.id, SyncTierLine)
			if err != nil || applied != tc.wantApplied {
				t.Fatalf("applied=%v err=%v, want %v/nil", applied, err, tc.wantApplied)
			}
			if tc.id != 999999 {
				if got := readSyncTier(t, dbh, tc.id); got != tc.wantTier {
					t.Errorf("sync_tier = %+v, want %+v", got, tc.wantTier)
				}
			}
		})
	}
}

// TestListSyncTierPending mirrors migration 052's partial index predicate
// exactly (outcome_type='synced' AND status='done' AND sync_tier IS NULL):
// only the row satisfying all three is returned, with its source_path.
func TestListSyncTierPending(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)

	seed := func(key, status, outcome string, tier any) int64 {
		var id int64
		if err := dbh.QueryRow(
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, sync_tier)
             VALUES ('A', ?, 'a', ?, ?, ?, ?, ?) RETURNING id`,
			key, key, "/m/"+key+".flac", status, outcome, tier).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		return id
	}
	pendingID := seed("pending", "done", "synced", nil)
	seed("wrong-status", "processing", "synced", nil)
	seed("wrong-outcome", "done", "unsynced", nil)
	seed("already-tiered", "done", "synced", "line")

	got, err := q.ListSyncTierPending(ctx)
	if err != nil {
		t.Fatalf("ListSyncTierPending: %v", err)
	}
	if len(got) != 1 || got[0].ID != pendingID || got[0].AudioPath != "/m/pending.flac" {
		t.Errorf("ListSyncTierPending = %+v, want exactly the one true candidate", got)
	}
}

// TestListSyncTierPending_Empty: no candidates is an empty slice, not an error.
func TestListSyncTierPending_Empty(t *testing.T) {
	ctx := context.Background()
	got, err := NewDBQueue(openQueueTestDB(t)).ListSyncTierPending(ctx)
	if err != nil {
		t.Fatalf("ListSyncTierPending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSyncTierPending on empty db = %+v, want empty", got)
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
