package commands

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
)

func openBackfillTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := dbpkg.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func seedUncreditedSettle(t *testing.T, sqlDB *sql.DB, title string) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key,
             instrumental_result, outcome_type, provider_lane)
         VALUES (?, ?, ?, ?, 1, 'instrumental', NULL)`,
		"Artist", title, "Artist", title); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
}

// TestRunProviderOutcomesBackfillCredits covers the success path and, on a
// second call, the AlreadyDone early return.
func TestRunProviderOutcomesBackfillCredits(t *testing.T) {
	sqlDB := openBackfillTestDB(t)
	seedUncreditedSettle(t, sqlDB, "One")

	runProviderOutcomesBackfill(context.Background(), sqlDB)

	var hits int64
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT hits FROM provider_outcomes WHERE lane = ?`,
		detectorbackfill.LaneName).Scan(&hits); err != nil {
		t.Fatalf("read provider_outcomes: %v", err)
	}
	if hits != 1 {
		t.Errorf("detector hits = %d; want 1", hits)
	}

	// Second call takes the AlreadyDone path and must not credit again.
	runProviderOutcomesBackfill(context.Background(), sqlDB)
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT hits FROM provider_outcomes WHERE lane = ?`,
		detectorbackfill.LaneName).Scan(&hits); err != nil {
		t.Fatalf("read provider_outcomes: %v", err)
	}
	if hits != 1 {
		t.Errorf("detector hits after a second run = %d; want 1 (double-credited)", hits)
	}
}

// TestRunProviderOutcomesBackfillSurvivesFailure covers the error path. The
// runner is best-effort by contract: a failure is logged, never propagated, and
// must not panic -- serve startup continues regardless.
func TestRunProviderOutcomesBackfillSurvivesFailure(t *testing.T) {
	sqlDB := openBackfillTestDB(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	runProviderOutcomesBackfill(context.Background(), sqlDB) // must not panic
}

// TestRunProviderOutcomesBackfillHandlesCanceledContext covers the
// context.Canceled branch, which logs at Info ("will retry") rather than Error:
// a shutdown mid-startup is not a failure worth alarming on.
func TestRunProviderOutcomesBackfillHandlesCanceledContext(t *testing.T) {
	sqlDB := openBackfillTestDB(t)
	seedUncreditedSettle(t, sqlDB, "One")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runProviderOutcomesBackfill(ctx, sqlDB) // must not panic

	// A canceled run must credit nothing, so the next startup can retry.
	var n int
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM maintenance_markers WHERE name = ?`,
		detectorbackfill.ProviderOutcomesMarker).Scan(&n); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if n != 0 {
		t.Errorf("marker rows = %d after a canceled run; want 0 so the next startup retries", n)
	}
}
