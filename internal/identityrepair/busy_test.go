package identityrepair

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/normalize"
)

// openBlocker opens an independent handle on the same file, standing in for a
// running `canticle serve` (a separate process, so a separate connection).
func openBlocker(t *testing.T, path string) *sql.DB {
	t.Helper()
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	other.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = other.Close() })
	return other
}

// TestApplySequence_StaleSnapshotWriteIsBusy pins the mechanism behind #978 at
// the SQL level, on the handle the CLI used before the fix (db.Open, DEFERRED
// transactions): a transaction that READS, then sees another connection COMMIT,
// then WRITES gets SQLITE_BUSY immediately. busy_timeout (5s) cannot rescue it,
// because waiting never makes the stale read snapshot valid again.
func TestApplySequence_StaleSnapshotWriteIsBusy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stale.db")
	a, err := dbpkg.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	lib := seedLibrary(t, a)
	sr := seedScan(t, a, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	b := openBlocker(t, path)

	tx, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_queue`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := b.ExecContext(ctx, `UPDATE scan_results SET status = 'done' WHERE id = ?`, sr); err != nil {
		t.Fatalf("concurrent commit: %v", err)
	}
	start := time.Now()
	_, err = tx.ExecContext(ctx, `UPDATE scan_results SET artist = 'x' WHERE id = ?`, sr)
	if !dbpkg.IsSQLiteBusy(err) {
		t.Fatalf("write after concurrent commit: err = %v; want SQLITE_BUSY", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("busy returned after %v; want immediate (busy_timeout does not apply to a stale snapshot)", waited)
	}
}

// TestRun_ConcurrentWriterRetriedRowAppliesOnceWithOneRecord is #978's
// end-to-end regression: a CLI-shaped handle runs Run while another connection
// (serve) holds the write lock for longer than the handle's busy_timeout. The
// row must still be corrected, and the retry must not duplicate its backup
// record (report runs inside the transaction, so a retried attempt that wrote
// one would leave two).
func TestRun_ConcurrentWriterRetriedRowAppliesOnceWithOneRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	repairDB, err := dbpkg.OpenImmediate(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repairDB.Close() })
	lib := seedLibrary(t, repairDB)
	sr := seedScan(t, repairDB, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, repairDB, "AlphaBravo", "", "pending", sr)

	// A short busy_timeout on the repair handle (its single pooled connection)
	// makes the lock wait outlast it, so the collision surfaces as SQLITE_BUSY
	// rather than being absorbed by the timeout.
	if _, err := repairDB.ExecContext(ctx, "PRAGMA busy_timeout=20"); err != nil {
		t.Fatalf("lower busy_timeout: %v", err)
	}

	blocker := openBlocker(t, path)
	conn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE work_queue SET priority = 1 WHERE id = ?`, wq); err != nil {
		t.Fatalf("blocker write: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, err := conn.ExecContext(ctx, "COMMIT")
		released <- err
	}()

	var records []Change
	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(repairDB, reader.read).Run(ctx, Options{
		Report: func(ch Change) error { records = append(records, ch); return nil },
	})
	if cerr := <-released; cerr != nil {
		t.Fatalf("blocker commit: %v", cerr)
	}
	if err != nil {
		t.Fatalf("Run under a concurrent writer: %v", err)
	}
	if res.Changed != 1 {
		t.Fatalf("Result = %+v; want Changed=1", res)
	}
	if len(records) != 1 {
		t.Fatalf("backup records = %d; want exactly 1 for one applied row", len(records))
	}
	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a, _, k := scanIdentity(t, repairDB, sr); a != "Alpha; Bravo" || k != wantKey {
		t.Errorf("scan_results = (%q,%q); want corrected", a, k)
	}
}
