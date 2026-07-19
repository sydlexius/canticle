package detectorbackfill

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	dbpkg "github.com/sydlexius/canticle/internal/db"
)

var errReport = errors.New("report boom")

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := dbpkg.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// seedQueue inserts one work_queue row. instrumentalResult is an int (0/1) or
// nil. updatedAt seeds the column the backfill reads for attempted_at.
func seedQueue(t *testing.T, sqlDB *sql.DB, title string, instrumentalResult any, updatedAt string) int64 {
	t.Helper()
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, instrumental_result, updated_at)
         VALUES (?, ?, ?, ?, ?, ?)`,
		"Artist", title, "Artist", title, instrumentalResult, updatedAt)
	if err != nil {
		t.Fatalf("insert work_queue %q: %v", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// seedAttempt inserts a pre-existing lane_attempts row, standing in for one
// recorded live by queue.RecordLaneAttempts.
func seedAttempt(t *testing.T, sqlDB *sql.DB, queueID int64, lane string, hit int, at string) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, ?, ?, ?)`,
		queueID, lane, hit, at); err != nil {
		t.Fatalf("insert lane_attempts: %v", err)
	}
}

// readAttempt returns the stored hit and attempted_at for one queue row's
// detector attempt. Assertions only ever concern the detector lane; a
// pre-existing row on another lane is seeded, never read back.
func readAttempt(t *testing.T, sqlDB *sql.DB, queueID int64) (hit int, at string, found bool) {
	t.Helper()
	err := sqlDB.QueryRowContext(context.Background(),
		`SELECT hit, attempted_at FROM lane_attempts WHERE queue_id = ? AND lane = ?`,
		queueID, LaneName).Scan(&hit, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false
	}
	if err != nil {
		t.Fatalf("read lane_attempts: %v", err)
	}
	return hit, at, true
}

func countAttempts(t *testing.T, sqlDB *sql.DB) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM lane_attempts`).Scan(&n); err != nil {
		t.Fatalf("count lane_attempts: %v", err)
	}
	return n
}

func TestRun_BackfillsBothBuckets(t *testing.T) {
	sqlDB := openDB(t)
	hitID := seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")
	missID := seedQueue(t, sqlDB, "NotInstrumental", 0, "2026-01-02T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Hits != 1 || res.Misses != 1 {
		t.Errorf("Hits/Misses = %d/%d; want 1/1 -- both buckets must move together", res.Hits, res.Misses)
	}
	if res.Scanned != 2 {
		t.Errorf("Scanned = %d; want 2", res.Scanned)
	}

	if hit, _, found := readAttempt(t, sqlDB, hitID); !found || hit != 1 {
		t.Errorf("hit row: hit=%d found=%v; want 1/true", hit, found)
	}
	if hit, _, found := readAttempt(t, sqlDB, missID); !found || hit != 0 {
		t.Errorf("miss row: hit=%d found=%v; want 0/true", hit, found)
	}
}

// A recorded live attempt is authoritative. This test discriminates ON CONFLICT
// DO NOTHING from the DO UPDATE clause used by queue.RecordLaneAttempts: under
// DO UPDATE the stored hit would be clobbered from 1 to 0 by the backfill's
// reconstructed verdict.
func TestRun_DoesNotOverwriteRecordedAttempt(t *testing.T) {
	sqlDB := openDB(t)
	id := seedQueue(t, sqlDB, "LiveRecorded", 0, "2026-01-02T00:00:00Z")
	seedAttempt(t, sqlDB, id, LaneName, 1, "2026-06-01T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.AlreadyRecorded != 1 {
		t.Errorf("AlreadyRecorded = %d; want 1", res.AlreadyRecorded)
	}
	if res.Hits != 0 || res.Misses != 0 {
		t.Errorf("Hits/Misses = %d/%d; want 0/0 -- a skipped row must not be tallied", res.Hits, res.Misses)
	}

	hit, at, found := readAttempt(t, sqlDB, id)
	if !found {
		t.Fatal("recorded attempt vanished")
	}
	if hit != 1 {
		t.Errorf("hit = %d; want 1 -- the backfill clobbered a live-recorded attempt", hit)
	}
	if at != "2026-06-01T00:00:00Z" {
		t.Errorf("attempted_at = %q; want the live value preserved", at)
	}
}

func TestRun_IsIdempotent(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")
	seedQueue(t, sqlDB, "NotInstrumental", 0, "2026-01-02T00:00:00Z")

	b := New(sqlDB)
	if _, err := b.Run(context.Background(), Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst := countAttempts(t, sqlDB)

	second, err := b.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if got := countAttempts(t, sqlDB); got != afterFirst {
		t.Errorf("lane_attempts rows = %d after second run; want %d (no double-count)", got, afterFirst)
	}
	if second.Hits != 0 || second.Misses != 0 {
		t.Errorf("second run Hits/Misses = %d/%d; want 0/0", second.Hits, second.Misses)
	}
	if second.AlreadyRecorded != 2 {
		t.Errorf("second run AlreadyRecorded = %d; want 2", second.AlreadyRecorded)
	}
}

func TestRun_NullResultIsUncoveredAndUntouched(t *testing.T) {
	sqlDB := openDB(t)
	nullID := seedQueue(t, sqlDB, "NeverDetected", nil, "2026-01-03T00:00:00Z")
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.UncoveredNull != 1 {
		t.Errorf("UncoveredNull = %d; want 1", res.UncoveredNull)
	}
	if res.Scanned != 1 {
		t.Errorf("Scanned = %d; want 1 -- a NULL row must not be scanned", res.Scanned)
	}
	if _, _, found := readAttempt(t, sqlDB, nullID); found {
		t.Error("a NULL instrumental_result row was attributed; it must be left untouched")
	}
}

func TestRun_AttemptedAtComesFromUpdatedAt(t *testing.T) {
	sqlDB := openDB(t)
	id := seedQueue(t, sqlDB, "Detected", 1, "2026-05-04T03:02:01Z")

	if _, err := New(sqlDB).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, at, found := readAttempt(t, sqlDB, id)
	if !found {
		t.Fatal("attempt not written")
	}
	if at != "2026-05-04T03:02:01Z" {
		t.Errorf("attempted_at = %q; want the row's updated_at", at)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")
	seedQueue(t, sqlDB, "NotInstrumental", 0, "2026-01-02T00:00:00Z")

	var reported []Change
	res, err := New(sqlDB).Run(context.Background(), Options{
		DryRun: true,
		Report: func(c Change) error { reported = append(reported, c); return nil },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Hits != 1 || res.Misses != 1 {
		t.Errorf("Hits/Misses = %d/%d; want 1/1", res.Hits, res.Misses)
	}
	if len(reported) != 2 {
		t.Errorf("reported %d changes; want 2", len(reported))
	}
	if got := countAttempts(t, sqlDB); got != 0 {
		t.Errorf("lane_attempts rows = %d after dry run; want 0", got)
	}
}

// A dry run must consult lane_attempts too, so its preview does not promise
// changes that an apply would skip.
func TestRun_DryRunCountsAlreadyRecorded(t *testing.T) {
	sqlDB := openDB(t)
	id := seedQueue(t, sqlDB, "LiveRecorded", 1, "2026-01-01T00:00:00Z")
	seedAttempt(t, sqlDB, id, LaneName, 1, "2026-06-01T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.AlreadyRecorded != 1 {
		t.Errorf("AlreadyRecorded = %d; want 1", res.AlreadyRecorded)
	}
	if res.Hits != 0 {
		t.Errorf("Hits = %d; want 0 -- dry run must not promise a skipped row", res.Hits)
	}
}

// A pre-existing attempt on a DIFFERENT lane must not shield the detector row:
// UNIQUE(queue_id, lane) is per-lane, so the detector attempt still lands.
func TestRun_OtherLaneAttemptDoesNotBlock(t *testing.T) {
	sqlDB := openDB(t)
	id := seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")
	seedAttempt(t, sqlDB, id, "musixmatch", 0, "2026-06-01T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Hits != 1 {
		t.Errorf("Hits = %d; want 1", res.Hits)
	}
	if hit, _, found := readAttempt(t, sqlDB, id); !found || hit != 1 {
		t.Errorf("detector attempt: hit=%d found=%v; want 1/true", hit, found)
	}
}

// Report runs inside the transaction before commit, so a report failure must
// leave the database untouched (backup-first: never apply without a record).
func TestRun_ReportFailureRollsBack(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")
	seedQueue(t, sqlDB, "NotInstrumental", 0, "2026-01-02T00:00:00Z")

	_, err := New(sqlDB).Run(context.Background(), Options{
		Report: func(Change) error { return errReport },
	})
	if err == nil {
		t.Fatal("expected a report error")
	}
	if !errors.Is(err, errReport) {
		t.Errorf("error = %v; want it to wrap errReport", err)
	}
	if got := countAttempts(t, sqlDB); got != 0 {
		t.Errorf("lane_attempts rows = %d after a failed report; want 0 (rolled back)", got)
	}
}

func TestRun_ContextCanceled(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(sqlDB).Run(ctx, Options{}); err == nil {
		t.Fatal("expected a context error")
	}
	if got := countAttempts(t, sqlDB); got != 0 {
		t.Errorf("lane_attempts rows = %d after cancellation; want 0", got)
	}
}

func TestRun_EmptyQueue(t *testing.T) {
	sqlDB := openDB(t)

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("Result = %+v; want the zero value", res)
	}
}

func TestRun_DryRunContextCanceled(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(sqlDB).Run(ctx, Options{DryRun: true}); err == nil {
		t.Fatal("expected a context error")
	}
}

// A dry run previews into the same Report sink an apply writes a backup to, so
// a sink failure must surface rather than being reported as a clean preview.
func TestRun_DryRunReportFailureAborts(t *testing.T) {
	sqlDB := openDB(t)
	seedQueue(t, sqlDB, "Detected", 1, "2026-01-01T00:00:00Z")

	_, err := New(sqlDB).Run(context.Background(), Options{
		DryRun: true,
		Report: func(Change) error { return errReport },
	})
	if err == nil {
		t.Fatal("expected a report error")
	}
	if !errors.Is(err, errReport) {
		t.Errorf("error = %v; want it to wrap errReport", err)
	}
}

// The package's core invariant is that hits and misses move TOGETHER. Every
// other both-buckets test seeds 1 hit and 1 miss, so a swap of the two counters
// in tally() leaves 1/1 as 1/1 and survives them all. This fixture is
// deliberately ASYMMETRIC so the buckets cannot be confused for one another.
func TestRun_BucketsAreNotTransposed(t *testing.T) {
	sqlDB := openDB(t)
	hitA := seedQueue(t, sqlDB, "DetectedA", 1, "2026-01-01T00:00:00Z")
	hitB := seedQueue(t, sqlDB, "DetectedB", 1, "2026-01-02T00:00:00Z")
	missID := seedQueue(t, sqlDB, "NotInstrumental", 0, "2026-01-03T00:00:00Z")

	res, err := New(sqlDB).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Hits != 2 {
		t.Errorf("Hits = %d; want 2 (buckets transposed?)", res.Hits)
	}
	if res.Misses != 1 {
		t.Errorf("Misses = %d; want 1 (buckets transposed?)", res.Misses)
	}

	// Assert the stored verdict per row, not just the totals: a tally that is
	// right in aggregate can still write the wrong hit value per row.
	for _, id := range []int64{hitA, hitB} {
		if hit, _, found := readAttempt(t, sqlDB, id); !found || hit != 1 {
			t.Errorf("queue row %d: hit=%d found=%v; want 1/true", id, hit, found)
		}
	}
	if hit, _, found := readAttempt(t, sqlDB, missID); !found || hit != 0 {
		t.Errorf("miss row: hit=%d found=%v; want 0/true", hit, found)
	}
}
