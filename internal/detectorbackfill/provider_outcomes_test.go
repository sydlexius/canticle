package detectorbackfill

import (
	"context"
	"database/sql"
	"testing"
)

// seedSettle inserts one work_queue row shaped for the provider_outcomes
// backfill: the three columns its predicate reads. providerLane is nil for a
// row that was never attributed (the backfill's target) or a string for one
// that already was.
func seedSettle(t *testing.T, sqlDB *sql.DB, title string, instrumentalResult, outcomeType, providerLane any) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key,
             instrumental_result, outcome_type, provider_lane)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"Artist", title, "Artist", title, instrumentalResult, outcomeType, providerLane); err != nil {
		t.Fatalf("insert work_queue %q: %v", title, err)
	}
}

func detectorHits(t *testing.T, sqlDB *sql.DB) (hits, misses int64) {
	t.Helper()
	err := sqlDB.QueryRowContext(context.Background(),
		`SELECT hits, misses FROM provider_outcomes WHERE lane = ?`, LaneName).Scan(&hits, &misses)
	if err == sql.ErrNoRows {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read provider_outcomes: %v", err)
	}
	return hits, misses
}

// TestBackfillProviderOutcomesCreditsOnlyUnattributedSettles is the core
// correctness case: exactly the detector settles that were never attributed get
// credited, and nothing else does.
func TestBackfillProviderOutcomesCreditsOnlyUnattributedSettles(t *testing.T) {
	sqlDB := openDB(t)

	// Creditable: detector settled, never attributed.
	seedSettle(t, sqlDB, "Uncredited One", 1, "instrumental", nil)
	seedSettle(t, sqlDB, "Uncredited Two", 1, "instrumental", nil)
	// Already attributed -- the live writer stamped the lane AND counted it.
	// Crediting again would double-count.
	seedSettle(t, sqlDB, "Already Credited", 1, "instrumental", LaneName)
	// Detector RAN AND LOST. This is the miss subset, which is deliberately
	// never credited (see BackfillProviderOutcomes' doc comment).
	seedSettle(t, sqlDB, "Detector Lost", 0, nil, nil)
	// Detection never ran.
	seedSettle(t, sqlDB, "Not Detected", nil, nil, nil)
	// Settled by a provider, not the detector.
	seedSettle(t, sqlDB, "Provider Win", nil, "synced", "musixmatch")

	res, err := BackfillProviderOutcomes(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("BackfillProviderOutcomes: %v", err)
	}
	if res.Credited != 2 {
		t.Errorf("Credited = %d; want 2 (only the unattributed detector settles)", res.Credited)
	}
	if res.AlreadyDone {
		t.Error("AlreadyDone = true on a first run")
	}

	hits, misses := detectorHits(t, sqlDB)
	if hits != 2 {
		t.Errorf("detector hits = %d; want 2", hits)
	}
	if misses != 0 {
		t.Errorf("detector misses = %d; want 0 -- the miss subset must never be credited", misses)
	}
}

// TestBackfillProviderOutcomesIsIdempotent is the property the marker exists to
// provide. provider_outcomes is a bare counter with no per-row key, so a second
// run without the marker gate would silently double the hit count.
func TestBackfillProviderOutcomesIsIdempotent(t *testing.T) {
	sqlDB := openDB(t)
	seedSettle(t, sqlDB, "One", 1, "instrumental", nil)
	seedSettle(t, sqlDB, "Two", 1, "instrumental", nil)

	if _, err := BackfillProviderOutcomes(context.Background(), sqlDB); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := BackfillProviderOutcomes(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.AlreadyDone {
		t.Error("second run AlreadyDone = false; the marker gate did not hold")
	}
	if second.Credited != 0 {
		t.Errorf("second run Credited = %d; want 0", second.Credited)
	}
	if hits, _ := detectorHits(t, sqlDB); hits != 2 {
		t.Errorf("detector hits after two runs = %d; want 2 (double-credited)", hits)
	}
}

// TestBackfillProviderOutcomesAddsToExistingCounter covers the ON CONFLICT arm:
// a database with live detector hits already recorded must have the backfill
// ADDED to them, not replace them.
func TestBackfillProviderOutcomesAddsToExistingCounter(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO provider_outcomes(lane, hits, misses) VALUES(?, 5, 7)`, LaneName); err != nil {
		t.Fatalf("seed provider_outcomes: %v", err)
	}
	seedSettle(t, sqlDB, "One", 1, "instrumental", nil)

	if _, err := BackfillProviderOutcomes(context.Background(), sqlDB); err != nil {
		t.Fatalf("BackfillProviderOutcomes: %v", err)
	}

	hits, misses := detectorHits(t, sqlDB)
	if hits != 6 {
		t.Errorf("detector hits = %d; want 6 (5 live + 1 backfilled)", hits)
	}
	if misses != 7 {
		t.Errorf("detector misses = %d; want 7 unchanged -- the backfill must not touch misses", misses)
	}
}

// TestBackfillProviderOutcomesMarksEmptyDatabaseDone covers the zero-rows path:
// a database with nothing to backfill is DONE, and must not re-scan on every
// subsequent startup.
func TestBackfillProviderOutcomesMarksEmptyDatabaseDone(t *testing.T) {
	sqlDB := openDB(t)

	res, err := BackfillProviderOutcomes(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("BackfillProviderOutcomes: %v", err)
	}
	if res.Credited != 0 {
		t.Errorf("Credited = %d; want 0", res.Credited)
	}

	second, err := BackfillProviderOutcomes(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.AlreadyDone {
		t.Error("an empty database was not marked done; it would re-scan every startup")
	}

	// No counter row should have been created for a zero credit.
	var n int
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM provider_outcomes WHERE lane = ?`, LaneName).Scan(&n); err != nil {
		t.Fatalf("count provider_outcomes: %v", err)
	}
	if n != 0 {
		t.Errorf("provider_outcomes rows for %q = %d; want 0 (nothing to credit)", LaneName, n)
	}
}

// TestBackfillProviderOutcomesFailsOnClosedDB covers the BeginTx error path: a
// closed database must surface an error rather than panicking or silently
// reporting success (which would leave the marker unset but the caller assuming
// the pass ran).
func TestBackfillProviderOutcomesFailsOnClosedDB(t *testing.T) {
	sqlDB := openDB(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	res, err := BackfillProviderOutcomes(context.Background(), sqlDB)
	if err == nil {
		t.Fatal("BackfillProviderOutcomes on a closed DB returned nil error; want failure")
	}
	if res.AlreadyDone {
		t.Error("AlreadyDone = true on a failed run; the caller would skip the retry")
	}
}

// TestBackfillProviderOutcomesFailsWhenQueueMissing covers the count-query error
// path, distinct from the marker-check and marker-insert paths.
func TestBackfillProviderOutcomesFailsWhenQueueMissing(t *testing.T) {
	sqlDB := openDB(t)
	if _, err := sqlDB.ExecContext(context.Background(), `DROP TABLE work_queue`); err != nil {
		t.Fatalf("drop work_queue: %v", err)
	}

	if _, err := BackfillProviderOutcomes(context.Background(), sqlDB); err == nil {
		t.Fatal("BackfillProviderOutcomes with no work_queue returned nil error; want failure")
	}

	// The marker must NOT have been stamped by a failed run, or the pass would
	// never retry and the counter would stay undercounted forever.
	var n int
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM maintenance_markers WHERE name = ?`, ProviderOutcomesMarker).Scan(&n); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if n != 0 {
		t.Errorf("marker rows = %d after a failed run; want 0 so the next startup retries", n)
	}
}

// TestBackfillProviderOutcomesRollsBackOnMarkerFailure pins the atomicity the
// design depends on. If the counter could commit without the marker, the next
// startup would credit the same rows again.
func TestBackfillProviderOutcomesRollsBackOnMarkerFailure(t *testing.T) {
	sqlDB := openDB(t)
	seedSettle(t, sqlDB, "One", 1, "instrumental", nil)

	// Fail the marker INSERT specifically, leaving the preceding marker-existence
	// SELECT and the counter UPDATE to run normally. A trigger is required here:
	// DROPPING the table instead makes the FIRST query (the existence check) fail
	// and the function returns before the counter is ever touched, so the test
	// would pass even with no rollback protection at all. That vacuous version
	// shipped in the first draft of this file and was caught in review.
	if _, err := sqlDB.ExecContext(context.Background(),
		`CREATE TRIGGER fail_marker_insert BEFORE INSERT ON maintenance_markers
         BEGIN SELECT RAISE(ABORT, 'marker insert blocked by test'); END`); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}

	if _, err := BackfillProviderOutcomes(context.Background(), sqlDB); err == nil {
		t.Fatal("BackfillProviderOutcomes returned nil error with no marker table; want failure")
	}

	if hits, _ := detectorHits(t, sqlDB); hits != 0 {
		t.Errorf("detector hits = %d after a failed run; want 0 -- the counter update must roll "+
			"back with the marker, or the next startup double-credits", hits)
	}
}
