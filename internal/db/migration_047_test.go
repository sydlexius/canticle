package db

import (
	"context"
	"testing"
)

// Migration 047 stamps the historical cohort of work_queue rows whose
// deferral cause was destroyed by the pre-#569 Release path (#624). Asserted
// against the REAL migration via openAtVersion -- see the comment on that
// helper in migration_038_test.go for why a hand-executed copy of the UPDATE
// proves nothing.
//
// The predicate is narrow on purpose (status='deferred', last_error=”,
// prev_status=”, priority=-100, miss_count>0), so this test's job is to prove
// it selects EXACTLY that cohort: the true positive gets stamped, and each
// near-miss -- differing from the cohort by exactly one clause -- is left
// untouched.
func TestMigration047StampsOnlyTheReleaseClearedCohort(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 46) // the state a real deployment upgrades FROM

	insert := `INSERT INTO work_queue
	           (id, artist_key, title_key, artist, title, source_path, status, last_error, prev_status, priority, miss_count, updated_at)
	           VALUES (?, ?, ?, 'a', 't', '/x.mp3', ?, ?, ?, ?, ?, '2026-08-04T00:00:00Z')`

	type row struct {
		id                                 int64
		key, status, lastError, prevStatus string
		priority                           int
		missCount                          int
		wantStamped                        bool
	}
	rows := []row{
		// THE COHORT: exactly the shape #624 measured -- deferred, empty cause,
		// no recorded pre-claim status, PriorityMiss tier, at least one miss.
		{901, "k1", "deferred", "", "", -100, 1, true},

		// NEAR-MISSES: each differs from the cohort by exactly one clause, and
		// none of them may be touched.
		{902, "k2", "deferred", "genuine failure text", "", -100, 1, false}, // non-empty last_error: a real, surviving cause
		{903, "k3", "pending", "", "", -100, 1, false},                      // wrong status: post-#569 release, correct as-is
		{904, "k4", "deferred", "", "deferred", -100, 1, false},             // prev_status populated: a post-#569 release
		{905, "k5", "deferred", "", "", 0, 1, false},                        // wrong priority tier: not a miss-deprioritized row
		{906, "k6", "deferred", "", "", -100, 0, false},                     // miss_count=0: never actually deferred by a miss
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert,
			r.id, r.key, r.key, r.status, r.lastError, r.prevStatus, r.priority, r.missCount,
		); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	if _, err := provider.UpTo(ctx, 47); err != nil {
		t.Fatalf("migrate to 47: %v", err)
	}

	const sentinel = "cause cleared by release"
	for _, r := range rows {
		var got string
		if err := dbh.QueryRowContext(ctx,
			`SELECT last_error FROM work_queue WHERE id = ?`, r.id,
		).Scan(&got); err != nil {
			t.Fatalf("select %d: %v", r.id, err)
		}
		if r.wantStamped {
			if got != sentinel {
				t.Errorf("id %d (cohort row): last_error = %q, want sentinel %q", r.id, got, sentinel)
			}
			continue
		}
		if got != r.lastError {
			t.Errorf("id %d (near-miss, differs from cohort by one clause): last_error = %q, want unchanged %q", r.id, got, r.lastError)
		}
	}

	// IDEMPOTENT: re-running the Up statement directly must be a no-op, because
	// the write itself empties the predicate (last_error is no longer '' on the
	// stamped row).
	if _, err := dbh.ExecContext(ctx,
		`UPDATE work_queue
		 SET last_error = 'cause cleared by release'
		 WHERE status = 'deferred'
		   AND last_error = ''
		   AND prev_status = ''
		   AND priority = -100
		   AND miss_count > 0`,
	); err != nil {
		t.Fatalf("re-apply migration predicate: %v", err)
	}
	var recount int
	if err := dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE last_error = ? AND id != 901`, sentinel,
	).Scan(&recount); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if recount != 0 {
		t.Errorf("re-applying the predicate stamped %d additional rows; want 0 (self-emptying)", recount)
	}
}
