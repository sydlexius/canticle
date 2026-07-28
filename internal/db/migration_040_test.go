package db

import (
	"context"
	"testing"
)

// Migration 040 fills lane_attempts for detector verdicts the offline backfill
// paths recorded without ever writing the table (#282). Asserted against the
// REAL migration via openAtVersion -- see the comment on that helper in
// migration_038_test.go for why a hand-executed copy of the UPDATE proves
// nothing.
//
// The two cases that matter are the ones a careless fill gets wrong: a row whose
// attempt was already recorded must keep the RECORDED value (reconstruction must
// never overwrite observation, including the recalibration correction the code
// fix introduces), and a not-instrumental verdict must be filled as a MISS rather
// than skipped -- the report renders a hit RATE, so a hits-only fill inflates it.
func TestMigration040FillsGapsWithoutOverwritingRecordedAttempts(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 39) // the state a real deployment upgrades FROM

	insert := `INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, instrumental_result, updated_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, '2026-07-27T00:00:00Z')`
	rows := []struct {
		id      int64
		key     string
		verdict any
		// preRecorded, when non-nil, is an attempt already in the table before the
		// migration runs.
		preRecorded *int
		wantHit     int // -1 = no attempt row expected
	}{
		// The defect's population: a verdict the backfill produced, never recorded.
		{901, "c1", 1, nil, 1},
		// A miss must be filled as hit=0, not skipped: the tile is a RATE.
		{902, "c2", 0, nil, 0},
		// Already recorded live, and corrected by a recalibration flip: the attempt
		// says hit while the row says 0. The RECORDED value must survive, or the
		// migration silently reverts that correction. This is the AMBIGUOUS
		// direction the repair below deliberately does not touch -- a first draft
		// used a symmetric "make the attempt match the row" UPDATE and broke exactly
		// this case, which is why the statement is one-directional.
		{903, "c3", 0, intPtr(1), 1},
		// Never detected -- no verdict, so nothing to attribute.
		{904, "c4", nil, nil, -1},
		// The UNAMBIGUOUS direction: the attempt says miss, the row says
		// instrumental. Settling requires instrumental_result=1 AND a completion, so
		// the row is the later, stronger evidence and the attempt is stale. Both
		// real rows measured on a live install are this shape. The gap-fill cannot
		// reach it -- the row already has an attempt, so DO NOTHING skips it -- which
		// is why the repair statement exists at all.
		{905, "c5", 1, intPtr(0), 1},
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert, r.id, r.key, r.key, r.key, r.key, "/m/"+r.key, "deferred", r.verdict); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
		if r.preRecorded != nil {
			if _, err := dbh.ExecContext(ctx,
				`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, 'detector', ?, '2026-07-26T00:00:00Z')`,
				r.id, *r.preRecorded); err != nil {
				t.Fatalf("seed attempt %s: %v", r.key, err)
			}
		}
	}

	// Run the migration under test -- the actual .sql file, not a copy of it.
	if _, err := provider.UpTo(ctx, 40); err != nil {
		t.Fatalf("migrate to 40: %v", err)
	}

	for _, r := range rows {
		var hit *int
		var n int
		if err := dbh.QueryRowContext(ctx,
			`SELECT MAX(hit), COUNT(*) FROM lane_attempts WHERE queue_id = ? AND lane = 'detector'`,
			r.id).Scan(&hit, &n); err != nil {
			t.Fatalf("select %s: %v", r.key, err)
		}
		if r.wantHit == -1 {
			if n != 0 {
				t.Errorf("%s: got %d attempt rows; a row that was never detected must not claim one", r.key, n)
			}
			continue
		}
		if n != 1 {
			t.Errorf("%s: got %d attempt rows; want exactly 1 (UNIQUE(queue_id, lane) must collapse, never duplicate)", r.key, n)
			continue
		}
		if hit == nil || *hit != r.wantHit {
			t.Errorf("%s: hit = %v; want %d", r.key, deref2(hit), r.wantHit)
		}
	}

	// The repair OVERWRITES a recorded hit value, which Down cannot recover, so the
	// pre-mutation value must be restorable and the backup must cover exactly the
	// overwritten rows -- c5 only. c3 is deliberately absent: it is never touched,
	// so backing it up would misrepresent what this migration changed.
	var backedUp, restoredRow int
	var oldValue string
	if err := dbh.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(row_id), -1), COALESCE(MAX(old_value), '')
		   FROM provenance_repair_backup
		  WHERE migration = '040' AND table_name = 'lane_attempts' AND column_name = 'hit'`,
	).Scan(&backedUp, &restoredRow, &oldValue); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if backedUp != 1 || restoredRow != 905 {
		t.Errorf("backup = %d row(s), row_id %d; want exactly 1 for queue_id 905 (only c5 is overwritten)", backedUp, restoredRow)
	}
	if oldValue != "0" {
		t.Errorf("backed-up old_value = %q; want %q -- the PRE-mutation hit, or the record cannot restore", oldValue, "0")
	}
}

func intPtr(i int) *int { return &i }

func deref2(i *int) string {
	if i == nil {
		return "<NULL>"
	}
	return string(rune('0' + *i))
}
