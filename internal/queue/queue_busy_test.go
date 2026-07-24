package queue

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"

	_ "modernc.org/sqlite"
)

// lockHoldDuration is how long holdWriteLock keeps the write lock. It comfortably
// exceeds the 50ms busy_timeout (so the first contended attempt is guaranteed to
// see SQLITE_BUSY) yet stays well under the retry budget (~1.5s of backoff over 5
// attempts), so a wrapped op reliably retries into success and the tests are not
// flaky. (#625)
const lockHoldDuration = 120 * time.Millisecond

// holdWriteLock opens a SECOND connection to path, holds an open write
// transaction against it for lockHoldDuration, then commits. It closes
// lockAcquired once the lock is held so the caller can run its contended
// operation against a different connection and reliably hit SQLITE_BUSY. Errors
// are reported on t.
func holdWriteLock(t *testing.T, ctx context.Context, path string, lockAcquired chan<- struct{}) *sync.WaitGroup {
	t.Helper()
	// A dedicated connection with a short busy_timeout so IT never blocks waiting
	// on the lock it is trying to hold (it is the holder, not a waiter).
	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	locker.SetMaxOpenConns(1)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = locker.Close() }()
		tx, err := locker.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("locker begin: %v", err)
			close(lockAcquired)
			return
		}
		// A write statement acquires the WAL write lock. UPDATE ... WHERE 1=0
		// touches no rows but still takes the lock.
		if _, err := tx.ExecContext(ctx, "UPDATE work_queue SET status = status WHERE 1=0"); err != nil {
			t.Errorf("locker acquire write lock: %v", err)
			_ = tx.Rollback()
			close(lockAcquired)
			return
		}
		close(lockAcquired)
		time.Sleep(lockHoldDuration)
		if err := tx.Commit(); err != nil {
			t.Errorf("locker commit: %v", err)
		}
	}()
	return wg
}

// newBusyProneQueue opens the queue on a SHORT busy_timeout so a concurrent
// write surfaces as SQLITE_BUSY quickly (forcing the retry path) rather than
// blocking for the default 5s. Migrations are applied via db.Open on a separate
// connection first (the short-timeout handle is only used by the queue).
func newBusyProneQueue(t *testing.T, path string) (*DBQueue, *sql.DB) {
	t.Helper()
	// Apply migrations on a full connection.
	migrator, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	t.Cleanup(func() { _ = migrator.Close() })

	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open queue conn: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	return NewDBQueue(conn), migrator
}

// TestDequeueConcurrentWriterRetries reproduces the #625 contention: a second
// connection holds the write lock while the worker dequeues. With busy retry,
// Dequeue must return the row instead of dropping the poll on SQLITE_BUSY.
func TestDequeueConcurrentWriterRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	q, _ := newBusyProneQueue(t, path)
	// Deterministic single-statement dequeue path (also covers the wrap).
	q.SetRandomized(false)

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}, PriorityScan); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	lockAcquired := make(chan struct{})
	wg := holdWriteLock(t, ctx, path, lockAcquired)
	<-lockAcquired

	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue under contention = %v; want nil (must retry past SQLITE_BUSY)", err)
	}
	if item.Inputs.Track.ArtistName != "A" {
		t.Fatalf("dequeued %q; want A", item.Inputs.Track.ArtistName)
	}
	wg.Wait()
}

// TestDequeueBatchedConcurrentWriterRetries covers the PRODUCTION-default dequeue
// path (randomized + batch_size>0 -> dequeueBatched), whose retry safety is
// non-trivial: it is a multi-statement BeginTx/refillBuffer/claim/Commit
// transaction, not a single autocommit statement. A BUSY anywhere before commit
// must roll the whole transaction back so a retry re-runs from a clean slate
// (no partial batch_seq mutation, no double-claim). (#625)
func TestDequeueBatchedConcurrentWriterRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	q, _ := newBusyProneQueue(t, path)
	// Production default: randomized shuffled lookahead buffer.
	q.SetRandomized(true)
	q.SetBatchSize(10)

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}, PriorityScan); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	lockAcquired := make(chan struct{})
	wg := holdWriteLock(t, ctx, path, lockAcquired)
	<-lockAcquired

	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("batched Dequeue under contention = %v; want nil (must retry past SQLITE_BUSY)", err)
	}
	if item.Inputs.Track.ArtistName != "A" {
		t.Fatalf("dequeued %q; want A", item.Inputs.Track.ArtistName)
	}
	wg.Wait()
}

// TestCompleteConcurrentWriterRetries verifies Complete retries past a transient
// lock. A dropped Complete would leave a finished item to be re-processed (#625
// scope audit: higher blast radius than a dropped poll).
func TestCompleteConcurrentWriterRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	q, verify := newBusyProneQueue(t, path)
	q.SetRandomized(false)

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}, PriorityScan); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	lockAcquired := make(chan struct{})
	wg := holdWriteLock(t, ctx, path, lockAcquired)
	<-lockAcquired

	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete under contention = %v; want nil (must retry past SQLITE_BUSY)", err)
	}
	wg.Wait()

	var status string
	if err := verify.QueryRowContext(ctx, "SELECT status FROM work_queue WHERE id = ?", item.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "done" {
		t.Fatalf("status = %q; want done", status)
	}
}

// TestFailConcurrentWriterRetries verifies Fail retries past a transient lock. A
// dropped Fail would leave a row stuck in 'processing' (#625 scope audit).
func TestFailConcurrentWriterRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	q, verify := newBusyProneQueue(t, path)
	q.SetRandomized(false)

	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}, PriorityScan); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	lockAcquired := make(chan struct{})
	wg := holdWriteLock(t, ctx, path, lockAcquired)
	<-lockAcquired

	if _, err := q.Fail(ctx, item.ID, context.DeadlineExceeded); err != nil {
		t.Fatalf("Fail under contention = %v; want nil (must retry past SQLITE_BUSY)", err)
	}
	wg.Wait()

	var status string
	if err := verify.QueryRowContext(ctx, "SELECT status FROM work_queue WHERE id = ?", item.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "processing" {
		t.Fatalf("status = processing; want a terminal/failed state (Fail dropped)")
	}
}
