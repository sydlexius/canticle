package wordsyncrepair

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/queue"
)

// seed inserts n completed synced rows with a NULL word_timing_state, completed
// on consecutive days, and returns the queue.
func seed(t *testing.T, n int) (*queue.DBQueue, *sql.DB) {
	t.Helper()
	sqlDB := dbtest.Open(t)
	for i := 1; i <= n; i++ {
		key := fmt.Sprintf("t%d", i)
		if _, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, timing_outcome, completed_at, priority)
			 VALUES ('A', ?, 'a', ?, ?, 'done', 'synced', 'ok', ?, 5)`,
			key, key, key+".flac", fmt.Sprintf("2026-01-%02dT00:00:00Z", i)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return queue.NewDBQueue(sqlDB), sqlDB
}

func queuedCount(t *testing.T, sqlDB *sql.DB) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM work_queue WHERE word_timing_state = 'queued'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type memBackup struct {
	buf      bytes.Buffer
	records  int
	failAt   int // 1-based record that fails, 0 = never
	syncErr  error
	syncs    int
	recorded []int64
}

func (m *memBackup) Record(p queue.WordRecheckPrior) error {
	if m.records++; m.records == m.failAt {
		return errors.New("disk full")
	}
	m.recorded = append(m.recorded, p.ID)
	return AppendRecord(&m.buf, p)
}

func (m *memBackup) Sync() error { m.syncs++; return m.syncErr }

func TestRun_DryRunCountsAndWritesNothing(t *testing.T) {
	q, sqlDB := seed(t, 4)
	res, err := Run(context.Background(), q, Options{Query: queue.WordRecheckOptions{Limit: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 4 || res.Selected != 3 || res.Queued != 0 || res.Written != 0 {
		t.Errorf("res = %+v; want candidates=4 selected=3 queued=0 written=0", res)
	}
	if got := queuedCount(t, sqlDB); got != 0 {
		t.Errorf("dry run queued %d rows", got)
	}
}

func TestRun_ApplyQueuesLimitedRowsAndBacksThemUp(t *testing.T) {
	q, sqlDB := seed(t, 4)
	b := &memBackup{}
	var progress []string
	res, err := Run(context.Background(), q, Options{
		Apply: true, Backup: b, Query: queue.WordRecheckOptions{Limit: 3},
		Progress: func(done, total int) { progress = append(progress, fmt.Sprintf("%d/%d", done, total)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 3 || res.Written != 3 || len(b.recorded) != 3 || b.syncs != 1 {
		t.Errorf("res = %+v records=%d syncs=%d; want 3 queued, 3 records, one batch fsync", res, len(b.recorded), b.syncs)
	}
	if got := queuedCount(t, sqlDB); got != 3 {
		t.Errorf("queued rows = %d; want 3", got)
	}
	if strings.Join(progress, ",") != "3/3" {
		t.Errorf("progress = %v; want [3/3]", progress)
	}
	// A second run sees the rows as already queued.
	res, err = Run(context.Background(), q, Options{})
	if err != nil || res.AlreadyQueued != 3 || res.Candidates != 1 {
		t.Errorf("second run = %+v, %v; want candidates=1 already=3", res, err)
	}
}

func TestRun_CutoffIsStrict(t *testing.T) {
	q, _ := seed(t, 4)
	res, err := Run(context.Background(), q, Options{Query: queue.WordRecheckOptions{CompletedBefore: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)}})
	if err != nil || res.Candidates != 2 {
		t.Errorf("res = %+v, %v; want 2 candidates before the cutoff", res, err)
	}
}

func TestRun_ApplyWithoutBackupRefused(t *testing.T) {
	q, sqlDB := seed(t, 2)
	if _, err := Run(context.Background(), q, Options{Apply: true}); !errors.Is(err, ErrNoBackup) {
		t.Errorf("err = %v; want ErrNoBackup", err)
	}
	if got := queuedCount(t, sqlDB); got != 0 {
		t.Errorf("queued %d rows without a backup", got)
	}
}

// A backup failing on a later row rolls the batch back; the result still says
// how many records were written so the caller can warn about the stale ones.
func TestRun_BackupFailureRollsBackAndIsReported(t *testing.T) {
	q, sqlDB := seed(t, 3)
	b := &memBackup{failAt: 2}
	res, err := Run(context.Background(), q, Options{Apply: true, Backup: b})
	var se *StageError
	if !errors.As(err, &se) || se.Stage != "flip" {
		t.Fatalf("err = %v; want a flip StageError", err)
	}
	if res.Queued != 0 || res.Written != 1 {
		t.Errorf("res = %+v; want queued=0 written=1", res)
	}
	if got := queuedCount(t, sqlDB); got != 0 {
		t.Errorf("a failed backup left %d rows queued", got)
	}
}

func TestRun_BatchFsyncFailureRollsBack(t *testing.T) {
	q, sqlDB := seed(t, 2)
	b := &memBackup{syncErr: errors.New("fsync failed")}
	res, err := Run(context.Background(), q, Options{Apply: true, Backup: b})
	if err == nil || res.Queued != 0 || res.Written != 2 {
		t.Errorf("res = %+v, err = %v; want a rollback with 2 written records", res, err)
	}
	if got := queuedCount(t, sqlDB); got != 0 {
		t.Errorf("a failed fsync left %d rows queued", got)
	}
}

// fakeQueue serves a fixed id list and flips each batch successfully.
type fakeQueue struct {
	ids       []int64
	batches   int
	failBatch int // 1-based batch whose flip fails
}

func (f *fakeQueue) CountWordRecheckCandidates(context.Context, queue.WordRecheckOptions) (int, error) {
	return len(f.ids), nil
}
func (f *fakeQueue) CountWordRecheckQueued(context.Context, []int64) (int, error) { return 0, nil }
func (f *fakeQueue) ListWordRecheckCandidates(context.Context, queue.WordRecheckOptions) ([]int64, error) {
	return f.ids, nil
}
func (f *fakeQueue) MarkWordRecheckQueued(_ context.Context, ids []int64, _ queue.WordRecheckOptions, report func(queue.WordRecheckPrior) error) ([]queue.WordRecheckPrior, error) {
	f.batches++
	prior := make([]queue.WordRecheckPrior, len(ids))
	for i, id := range ids {
		prior[i] = queue.WordRecheckPrior{ID: id}
		if err := report(prior[i]); err != nil {
			return nil, err
		}
	}
	if f.batches == f.failBatch {
		return nil, errors.New("commit failed")
	}
	return prior, nil
}

func ids(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

func TestRun_BatchesAndKeepsCommittedBatchesOnLaterFailure(t *testing.T) {
	f := &fakeQueue{ids: ids(FlipBatch*2 + 1), failBatch: 2}
	res, err := Run(context.Background(), f, Options{Apply: true, Backup: &memBackup{}})
	var se *StageError
	if !errors.As(err, &se) || se.Stage != "flip" {
		t.Fatalf("err = %v; want a flip StageError", err)
	}
	if res.Queued != FlipBatch || res.Written != 2*FlipBatch {
		t.Errorf("res = %+v; want queued=%d (batch 1) written=%d (incl. rolled-back batch 2)", res, FlipBatch, 2*FlipBatch)
	}
}

func TestRun_CancelStopsAtBatchBoundaryWithPartialResult(t *testing.T) {
	f := &fakeQueue{ids: ids(FlipBatch + 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := Run(ctx, f, Options{Apply: true, Backup: &memBackup{}, Progress: func(int, int) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want it to wrap context.Canceled", err)
	}
	if res.Queued != FlipBatch || f.batches != 1 {
		t.Errorf("res = %+v batches=%d; want only the first batch committed", res, f.batches)
	}
}

func TestFileBackup_LazyOpenAndRecordShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.jsonl")
	b := &FileBackup{Path: path}
	if err := b.Sync(); err != nil || b.Opened() {
		t.Fatalf("Sync before any record: err=%v opened=%v; want a no-op", err, b.Opened())
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("file exists before the first record")
	}
	state := "absent"
	if err := b.Record(queue.WordRecheckPrior{ID: 7, Status: "done", WordTimingState: &state}); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `"id":7`) || !strings.Contains(string(got), `"word_timing_state":"absent"`) || !strings.HasSuffix(string(got), "\n") {
		t.Errorf("record = %q", got)
	}
}

func TestFileBackup_OpenFailureNamesTheOperatorPathOnly(t *testing.T) {
	b := &FileBackup{Path: filepath.Join(t.TempDir(), "missing", "b.jsonl")}
	err := b.Record(queue.WordRecheckPrior{ID: 1})
	if err == nil || b.Opened() {
		t.Fatalf("err=%v opened=%v; want an open failure", err, b.Opened())
	}
	if !strings.Contains(err.Error(), b.Path) {
		t.Errorf("error %q does not name the backup path", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v does not wrap fs.ErrNotExist", err)
	}
}

func TestFileBackup_NilReceiverErrorsNotPanics(t *testing.T) {
	var b *FileBackup
	if err := b.Record(queue.WordRecheckPrior{ID: 1}); err == nil {
		t.Error("Record on a nil FileBackup returned nil")
	}
	if err := b.Sync(); err == nil {
		t.Error("Sync on a nil FileBackup returned nil")
	}
	if b.Opened() {
		t.Error("nil FileBackup reports Opened")
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close on a nil FileBackup: %v", err)
	}
}
