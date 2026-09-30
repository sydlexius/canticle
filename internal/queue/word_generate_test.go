package queue

import (
	"context"
	"database/sql"
	"slices"
	"testing"
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
		seedGenerateRow(t, dbh, "old-version", "word_generate_version = 2, word_generate_at = '2026-09-01T00:00:00Z'"),
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

// TestWordGenerateMarker: a stamped row is skipped under its version and a
// version bump re-admits it; a row that left 'done' is not stamped.
func TestWordGenerateMarker(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	a := seedGenerateRow(t, dbh, "a", "")
	b := seedGenerateRow(t, dbh, "b", "timing_outcome = 'mis_synced', word_timing_state = NULL")
	taken := seedGenerateRow(t, dbh, "taken", "")
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, taken)

	stamped, err := q.StampWordGenerateAttempt(ctx, []int64{a, b, taken}, genTestVersion)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !slices.Equal(stamped, []int64{a, b}) {
		t.Fatalf("stamped = %v, want [%d %d] (not the processing row)", stamped, a, b)
	}
	var at sql.NullString
	if err := dbh.QueryRow(`SELECT word_generate_at FROM work_queue WHERE id = ?`, a).Scan(&at); err != nil || at.String != formatTime(upgradeNow) {
		t.Fatalf("word_generate_at = %v, %v; want %s", at, err, formatTime(upgradeNow))
	}
	if got := listGenerate(t, q, genTestVersion, 0); len(got) != 0 {
		t.Fatalf("after stamp at v%d: %v, want none", genTestVersion, got)
	}
	got := listGenerate(t, q, genTestVersion+1, 0)
	slices.Sort(got)
	if !slices.Equal(got, []int64{a, b}) {
		t.Fatalf("after version bump: %v, want [%d %d]", got, a, b)
	}
}
