package scan_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/library"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"

	_ "modernc.org/sqlite"
)

func countScanResults(t *testing.T, sqlDB *sql.DB, libraryID int64) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRow("SELECT COUNT(*) FROM scan_results WHERE library_id = ?", libraryID).Scan(&n); err != nil {
		t.Fatalf("count scan_results: %v", err)
	}
	return n
}

func makeResults(n int, prefix string) []models.ScanResult {
	out := make([]models.ScanResult, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/music/%s_%05d.mp3", prefix, i)
		out = append(out, models.ScanResult{
			FilePath: p,
			Track:    models.Track{ArtistName: fmt.Sprintf("Artist %d", i), TrackName: fmt.Sprintf("Title %d", i)},
			Outdir:   "/music",
			Filename: fmt.Sprintf("%s_%05d.lrc", prefix, i),
		})
	}
	return out
}

// TestRepo_UpsertConcurrentWriterRetries reproduces the #131 contention: a second
// process holds the write lock while Upsert runs. With batched txns + busy retry,
// Upsert must persist all rows instead of aborting with SQLITE_BUSY.
func TestRepo_UpsertConcurrentWriterRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")

	// Connection A: applies migrations (busy_timeout=5000) and holds the lock.
	connA, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { _ = connA.Close() })

	lib, err := library.New(connA).Add(ctx, "/music", "Music")
	if err != nil {
		t.Fatalf("add library: %v", err)
	}

	// Connection B: a *second* connection with a short busy_timeout so contention
	// surfaces quickly as SQLITE_BUSY (forcing the retry path rather than a 5s wait).
	connB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { _ = connB.Close() })
	connB.SetMaxOpenConns(1)

	// A acquires and holds the write lock, then releases shortly after.
	locked := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tx, err := connA.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("A begin: %v", err)
			close(locked)
			return
		}
		if _, err := tx.ExecContext(ctx, "UPDATE libraries SET name = name WHERE id = ?", lib.ID); err != nil {
			t.Errorf("A acquire write lock: %v", err)
			_ = tx.Rollback()
			close(locked)
			return
		}
		close(locked)
		time.Sleep(120 * time.Millisecond)
		if err := tx.Commit(); err != nil {
			t.Errorf("A commit: %v", err)
		}
	}()

	<-locked
	results := makeResults(50, "conc")
	if err := scan.New(connB).Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert under contention = %v; want nil (should retry past SQLITE_BUSY)", err)
	}
	wg.Wait()

	if got := countScanResults(t, connA, lib.ID); got != 50 {
		t.Fatalf("persisted %d rows; want 50", got)
	}
}

// TestRepo_UpsertPartialBatchFailure verifies that one failing batch (an invalid
// status violates the CHECK constraint) does not abort the whole scan: earlier
// batches stay committed and an aggregate error is returned.
func TestRepo_UpsertPartialBatchFailure(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	lib, err := library.New(sqlDB).Add(ctx, "/music", "Music")
	if err != nil {
		t.Fatalf("add library: %v", err)
	}

	// One full valid batch, then a second batch whose single row has an invalid
	// status (violates CHECK(status IN (...))).
	results := makeResults(scan.UpsertBatchSize, "ok")
	bad := models.ScanResult{
		FilePath: "/music/bad.mp3",
		Track:    models.Track{ArtistName: "Bad", TrackName: "Row"},
		Outdir:   "/music",
		Filename: "bad.lrc",
		Status:   "bogus", // not in the CHECK set -> batch fails
	}
	results = append(results, bad)

	err = scan.New(sqlDB).Upsert(ctx, lib.ID, results, scan.UpsertOptions{})
	if err == nil {
		t.Fatal("Upsert = nil; want aggregate error for the failed batch")
	}
	if got := countScanResults(t, sqlDB, lib.ID); got != scan.UpsertBatchSize {
		t.Fatalf("persisted %d rows; want %d (first batch must survive the second batch's failure)", got, scan.UpsertBatchSize)
	}
}
