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
		// Already recorded live (or corrected by a recalibration flip). The stored
		// verdict disagrees with the recorded attempt on purpose: the RECORDED value
		// must survive, or the migration would silently revert a correction.
		{903, "c3", 0, intPtr(1), 1},
		// Never detected -- no verdict, so nothing to attribute.
		{904, "c4", nil, nil, -1},
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
}

func intPtr(i int) *int { return &i }

func deref2(i *int) string {
	if i == nil {
		return "<NULL>"
	}
	return string(rune('0' + *i))
}
