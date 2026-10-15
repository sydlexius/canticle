package tierrepair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/queue"
)

const lrcLines = "[00:01.00]alpha\n[00:02.00]beta\n"

// seedRows inserts n completed synced rows, each with a line-synced sidecar,
// and returns the queue, the ids and the backup path.
func seedRows(t *testing.T, n int) (*queue.DBQueue, string) {
	t.Helper()
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%d", i)
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type)
             VALUES ('A', ?, 'a', ?, ?, 'done', 'synced')`, name, name, filepath.Join(dir, name+".flac")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".lrc"), []byte(lrcLines), 0o600); err != nil {
			t.Fatalf("lrc: %v", err)
		}
	}
	return queue.NewDBQueue(sqlDB), filepath.Join(dir, "backup.jsonl")
}

func backupLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func openSink(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := OpenBackup(path)
	if err != nil {
		t.Fatalf("OpenBackup: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// cancelAfterFirst returns a context and a Progress that cancels it once the
// first row is done, so the run stops at the next row boundary.
func cancelAfterFirst() (context.Context, Progress) {
	ctx, cancel := context.WithCancel(context.Background())
	return ctx, func(done, _ int64) {
		if done == 1 {
			cancel()
		}
	}
}

func TestRunSyncTier_CancelBetweenRowsKeepsBackupAndRowsConsistent(t *testing.T) {
	q, backup := seedRows(t, 3)
	cands, err := ListSyncTier(context.Background(), q)
	if err != nil || len(cands) != 3 {
		t.Fatalf("list: %v, %d rows", err, len(cands))
	}
	ctx, progress := cancelAfterFirst()
	res, err := RunSyncTier(ctx, q, cands, Options{Apply: true, Backup: openSink(t, backup), Progress: progress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Stamped != 1 {
		t.Fatalf("stamped %d, want 1", res.Stamped)
	}
	lines := backupLines(t, backup)
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(`"id":%d`, cands[0].ID)) {
		t.Errorf("backup = %v, want exactly the first row's record", lines)
	}
	left, err := ListSyncTier(context.Background(), q)
	if err != nil || len(left) != 2 {
		t.Fatalf("after cancel %d rows still pending (err %v), want 2", len(left), err)
	}
}

// countingQ counts guarded writes so a refused run can prove it made none.
type countingQ struct{ writes int }

func (f *countingQ) SetSyncTierIfPending(context.Context, int64, string, func() error) (bool, error) {
	f.writes++
	return true, nil
}

func TestApplyWithoutBackupIsRefusedAndChangesNothing(t *testing.T) {
	q, _ := seedRows(t, 2)
	ctx := context.Background()
	sc, _ := ListSyncTier(ctx, q)
	f := &countingQ{}
	if _, err := RunSyncTier(ctx, f, sc, Options{Apply: true}); !errors.Is(err, ErrNoBackup) || f.writes != 0 {
		t.Errorf("sync-tier: err=%v writes=%d, want ErrNoBackup and 0 writes", err, f.writes)
	}
	if left, _ := ListSyncTier(ctx, q); len(left) != 2 {
		t.Errorf("%d rows pending, want both untouched", len(left))
	}
}

// scriptedQ answers SetSyncTierIfPending per row id; an applied row runs the
// backup closure first, as the real queue does inside its transaction.
type scriptedQ struct {
	raced  map[int64]bool
	failed map[int64]bool
}

func (f *scriptedQ) SetSyncTierIfPending(_ context.Context, id int64, _ string, backup func() error) (bool, error) {
	switch {
	case f.failed[id]:
		return false, errors.New("write failed")
	case f.raced[id]:
		return false, nil
	}
	if err := backup(); err != nil {
		return false, err
	}
	return true, nil
}

// recSink records Write and Sync calls.
type recSink struct {
	buf   strings.Builder
	syncs int
}

func (r *recSink) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recSink) Sync() error                 { r.syncs++; return nil }

func TestRunSyncTier_RacedAndWriteFailedAreCountedAndNeverBackedUp(t *testing.T) {
	q, _ := seedRows(t, 4)
	ctx := context.Background()
	cands, _ := ListSyncTier(ctx, q)
	f := &scriptedQ{raced: map[int64]bool{cands[1].ID: true}, failed: map[int64]bool{cands[2].ID: true}}
	sink := &recSink{}
	res, err := RunSyncTier(ctx, f, cands, Options{Apply: true, Backup: sink})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Stamped != 2 || res.WriteFailed != 1 || res.Counts["raced"] != 1 || res.Counts["line"] != 2 {
		t.Errorf("result = %+v, want stamped=2 write_failed=1 raced=1 line=2", res)
	}
	lines := strings.Split(strings.TrimSuffix(sink.buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("backup = %q, want exactly the 2 stamped rows", lines)
	}
	for _, l := range lines {
		for _, id := range []int64{cands[1].ID, cands[2].ID} {
			if strings.Contains(l, fmt.Sprintf(`"id":%d,`, id)) {
				t.Errorf("backup record %q for a row that did not change", l)
			}
		}
	}
}

func TestRunSyncTier_SyncsEveryBackupRecord(t *testing.T) {
	q, _ := seedRows(t, 3)
	ctx := context.Background()
	cands, _ := ListSyncTier(ctx, q)
	sink := &recSink{}
	res, err := RunSyncTier(ctx, &scriptedQ{}, cands, Options{Apply: true, Backup: sink})
	if err != nil || res.Stamped != 3 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if sink.syncs != 3 {
		t.Errorf("Sync called %d times for 3 records, want 3", sink.syncs)
	}
}

func TestDryRunWritesNothingAndReportsProgress(t *testing.T) {
	q, backup := seedRows(t, 2)
	ctx := context.Background()
	cands, _ := ListSyncTier(ctx, q)
	var last, total int64
	res, err := RunSyncTier(ctx, q, cands, Options{Progress: func(d, n int64) { last, total = d, n }})
	if err != nil || res.Stamped != 0 || res.Counts["line"] != 2 {
		t.Fatalf("dry run: %+v err %v", res, err)
	}
	if last != 2 || total != 2 {
		t.Errorf("progress ended at %d/%d, want 2/2", last, total)
	}
	if _, err := os.Stat(backup); err == nil {
		t.Error("dry run created a backup")
	}
	if left, _ := ListSyncTier(ctx, q); len(left) != 2 {
		t.Errorf("dry run changed rows: %d pending", len(left))
	}
}

func TestOpenBackupBadPathFailsBeforeAnyWrite(t *testing.T) {
	if _, err := OpenBackup(filepath.Join(t.TempDir(), "missing-dir", "b.jsonl")); err == nil {
		t.Error("want an error for an unopenable backup path")
	}
}
