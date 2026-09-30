package queue

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

const (
	genTestWordGen = 7
	genTestVersion = 3
)

// seedGenerateRow inserts a full line-arm candidate (settled line-synced .lrc,
// 'absent' under the current word generation) and applies set, a SQL SET
// fragment, to vary exactly one term.
func seedGenerateRow(t *testing.T, dbh *sql.DB, key, set string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, sync_tier,
             word_timing_state, word_timing_generation, completed_at)
         VALUES ('A', ?, 'a', ?, '/m/x.flac', 'done', 'synced', 'line', 'absent', ?, '2026-08-01T00:00:00Z') RETURNING id`,
		key, key, genTestWordGen).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	if set != "" {
		mustExec(t, dbh, `UPDATE work_queue SET `+set+` WHERE id = ?`, id)
	}
	return id
}

func listGenerate(t *testing.T, q *DBQueue, version int64, limit int) []int64 {
	t.Helper()
	got, err := q.ListWordGenerateCandidates(context.Background(), WordGenerateOptions{
		GeneratorVersion: version, WordGeneration: genTestWordGen, Limit: limit,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return got
}

// TestWordGenerateCandidates_EachRule: one row per inclusion and one per
// exclusion, each differing from a full candidate in exactly one term.
func TestWordGenerateCandidates_EachRule(t *testing.T) {
	q, dbh := upgradeQueue(t)
	missynced := `outcome_type = 'unsynced', sync_tier = NULL, word_timing_state = NULL, word_timing_generation = NULL, timing_outcome = 'mis_synced'`
	want := []int64{
		seedGenerateRow(t, dbh, "line-absent", ""),
		seedGenerateRow(t, dbh, "missynced-guard-txt", missynced),
		// Matches both arms' other terms: listed once, the arms are disjoint.
		seedGenerateRow(t, dbh, "missynced-sweep-lrc", "timing_outcome = 'mis_synced'"),
		// The OLDEST completion, so only the word_generate_at key can sort it last.
		seedGenerateRow(t, dbh, "old-version", "word_generate_version = 2, word_generate_at = '2026-09-01T00:00:00Z', completed_at = '2026-07-01T00:00:00Z'"),
	}
	for _, c := range []struct{ key, set string }{
		{"not-examined", "word_timing_state = NULL"},
		{"served", "word_timing_state = 'served'"},
		{"stale-absent", "word_timing_generation = 6"},
		{"recheck-queued", "word_timing_state = 'queued'"},
		{"word-tier", "sync_tier = 'word'"},
		{"untiered", "sync_tier = NULL"},
		{"unsynced-tier", "sync_tier = 'unsynced'"},
		{"txt", "outcome_type = 'unsynced'"},
		{"categorical", "timing_outcome = 'categorical'"},
		{"degenerate", "timing_outcome = 'degenerate'"},
		{"pending", "status = 'pending'"},
		{"processing", "status = 'processing'"},
		{"no-source", "source_path = ' '"},
		{"retired-gone", "last_error = 'source file is gone'"},
		{"upgrade-armed", "upgrade_queued = 1"},
		{"handled", "word_generate_version = 3"},
		{"missynced-handled", missynced + ", word_generate_version = 3"},
		{"missynced-recheck-queued", missynced + ", word_timing_state = 'queued'"},
		{"missynced-upgrade-armed", missynced + ", upgrade_queued = 1"},
		{"missynced-pending", missynced + ", status = 'pending'"},
		{"missynced-no-source", missynced + ", source_path = ''"},
		{"missynced-retired-gone", missynced + ", last_error = 'source file is gone'"},
	} {
		seedGenerateRow(t, dbh, c.key, c.set)
	}
	got := listGenerate(t, q, genTestVersion, 100)
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if !slices.Equal(sorted, want) {
		t.Fatalf("candidates = %v, want %v", sorted, want)
	}
	// Never-handled first: the old-version row (handled once) sorts last.
	if got[len(got)-1] != want[3] {
		t.Fatalf("order = %v, want the previously handled row %d last", got, want[3])
	}
	if got := listGenerate(t, q, genTestVersion, 1); !slices.Equal(got, want[:1]) {
		t.Fatalf("limit 1 = %v, want %v", got, want[:1])
	}
}

// TestWordGenerateMarker: a stamped row is skipped under its version; a row
// that left 'done', or (review F1, F2) was re-completed, even in the same
// second, is not stamped, and a re-completion re-admits it at the same version.
func TestWordGenerateMarker(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	a := seedGenerateRow(t, dbh, "a", "")
	b := seedGenerateRow(t, dbh, "b", "timing_outcome = 'mis_synced', word_timing_state = NULL")
	taken := seedGenerateRow(t, dbh, "taken", "")
	stamp := func(ids ...int64) []int64 {
		st, err := q.StampWordGenerateAttempt(ctx, ids, genTestVersion)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if err := q.MarkWordGenerateOffered(ctx, []int64{a, b, taken}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, taken)
	if st := stamp(a, b, taken); !slices.Equal(st, []int64{a, b}) {
		t.Fatalf("stamped = %v, want [%d %d] (not the processing row)", st, a, b)
	}
	if got := listGenerate(t, q, genTestVersion, 0); len(got) != 0 {
		t.Fatalf("after stamp at v%d: %v, want none", genTestVersion, got)
	}
	if got := listGenerate(t, q, genTestVersion+1, 0); len(got) != 2 {
		t.Fatalf("after version bump: %v, want both", got)
	}
	// A scan reopen, then the worker re-fetches and settles a new file.
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, a, upgradeNow); err != nil || !ok {
		t.Fatalf("reopen = %v, %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing', outcome_type = 'synced' WHERE id = ?`, a)
	if err := q.Complete(ctx, a); err != nil { // same second as the stamp
		t.Fatalf("complete: %v", err)
	}
	if got := listGenerate(t, q, genTestVersion, 0); !slices.Equal(got, []int64{a}) {
		t.Fatalf("after re-fetch: %v, want [%d] (the marker judged the old file)", got, a)
	}
	// Offered and unhandled it stays a candidate; re-completed before the
	// generator reports, it is not stamped.
	q.now = func() time.Time { return upgradeNow.Add(time.Hour) }
	if err := q.MarkWordGenerateOffered(ctx, []int64{a}); err != nil {
		t.Fatal(err)
	}
	if got := listGenerate(t, q, genTestVersion, 0); !slices.Equal(got, []int64{a}) {
		t.Fatalf("after an unhandled offer: %v, want [%d]", got, a)
	}
	mustExec(t, dbh, `UPDATE work_queue SET completed_at = ? WHERE id = ?`, formatTime(upgradeNow.Add(time.Hour)), a)
	if st := stamp(a); len(st) != 0 {
		t.Fatalf("stale stamp = %v; want nothing stamped", st)
	}
}
