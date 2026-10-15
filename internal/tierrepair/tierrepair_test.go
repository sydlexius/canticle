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

func TestRunRemediated_CancelBetweenRowsKeepsBackupAndRowsConsistent(t *testing.T) {
	q, backup := seedRows(t, 3)
	cands, err := ListRemediated(context.Background(), q)
	if err != nil || len(cands) != 3 {
		t.Fatalf("list: %v, %d rows", err, len(cands))
	}
	ctx, progress := cancelAfterFirst()
	res, err := RunRemediated(ctx, q, cands, Options{Apply: true, Backup: openSink(t, backup), Progress: progress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Applied != 1 || res.Scanned != 3 {
		t.Fatalf("applied %d scanned %d, want 1 and 3", res.Applied, res.Scanned)
	}
	if lines := backupLines(t, backup); len(lines) != 1 {
		t.Errorf("backup has %d records, want 1: %v", len(lines), lines)
	}
	left, err := ListRemediated(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	// The recorded row left the candidate set; the two never reached stay.
	again, err := RunRemediated(context.Background(), q, left, Options{})
	if err != nil || len(left) != 2 || again.Counts[queue.RemediatedTier] != 2 {
		t.Errorf("after cancel: %d candidates, counts %v, err %v, want 2 candidates both tier", len(left), again.Counts, err)
	}
}

// countingQ counts guarded writes so a refused run can prove it made none.
type countingQ struct{ writes int }

func (f *countingQ) SetSyncTierIfPending(context.Context, int64, string, func() error) (bool, error) {
	f.writes++
	return true, nil
}

func (f *countingQ) ListRemediatedCandidates(context.Context, string) ([]queue.RemediatedCandidate, error) {
	return nil, nil
}

func (f *countingQ) ApplyRemediated(context.Context, queue.RemediatedCandidate, string, string, func() error) (bool, error) {
	f.writes++
	return true, nil
}

func TestApplyWithoutBackupIsRefusedAndChangesNothing(t *testing.T) {
	q, _ := seedRows(t, 2)
	ctx := context.Background()
	rc, _ := ListRemediated(ctx, q)
	fr := &countingQ{}
	if _, err := RunRemediated(ctx, fr, rc, Options{Apply: true}); !errors.Is(err, ErrNoBackup) || fr.writes != 0 {
		t.Errorf("remediated: err=%v writes=%d, want ErrNoBackup and 0 writes", err, fr.writes)
	}
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

func (f *scriptedQ) ListRemediatedCandidates(context.Context, string) ([]queue.RemediatedCandidate, error) {
	return nil, nil
}

func (f *scriptedQ) ApplyRemediated(_ context.Context, c queue.RemediatedCandidate, _, _ string, backup func() error) (bool, error) {
	switch {
	case f.failed[c.ID]:
		return false, errors.New("write failed")
	case f.raced[c.ID]:
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

func TestRunRemediated_RacedAndWriteFailedAreCountedAndNeverBackedUp(t *testing.T) {
	q, _ := seedRows(t, 4)
	ctx := context.Background()
	cands, _ := ListRemediated(ctx, q)
	f := &scriptedQ{raced: map[int64]bool{cands[1].ID: true}, failed: map[int64]bool{cands[2].ID: true}}
	sink := &recSink{}
	res, err := RunRemediated(ctx, f, cands, Options{Apply: true, Backup: sink})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Applied != 2 || res.WriteFailed != 1 || res.Counts["raced"] != 1 || res.Counts[queue.RemediatedTier] != 2 {
		t.Errorf("result = %+v, want applied=2 write_failed=1 raced=1 tier=2", res)
	}
	lines := strings.Split(strings.TrimSuffix(sink.buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("backup = %q, want exactly the 2 applied rows", lines)
	}
	for _, l := range lines {
		for _, id := range []int64{cands[1].ID, cands[2].ID} {
			if strings.Contains(l, fmt.Sprintf(`"id":%d,`, id)) {
				t.Errorf("backup record %q for a row that did not change", l)
			}
		}
	}
}

func TestRunRemediated_SyncsEveryBackupRecord(t *testing.T) {
	q, _ := seedRows(t, 3)
	ctx := context.Background()
	cands, _ := ListRemediated(ctx, q)
	sink := &recSink{}
	res, err := RunRemediated(ctx, &scriptedQ{}, cands, Options{Apply: true, Backup: sink})
	if err != nil || res.Applied != 3 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if sink.syncs != 3 {
		t.Errorf("Sync called %d times for 3 records, want 3", sink.syncs)
	}
}

// TestRunRemediated_BackupRecordIsExactForEachAction pins the record bytes for
// the tier, unsynced and reset actions. Every prior_* field holds a distinct
// value, so a field wired to the wrong source cannot pass.
func TestRunRemediated_BackupRecordIsExactForEachAction(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, sidecar string) string {
		audio := filepath.Join(dir, name+".flac")
		if err := os.WriteFile(audio, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if sidecar != "" {
			if err := os.WriteFile(filepath.Join(dir, name+sidecar), []byte(lrcLines), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return audio
	}
	prior := func(id int64, audio string) queue.RemediatedCandidate {
		return queue.RemediatedCandidate{ID: id, AudioPath: audio, Status: "done",
			PriorOutcome: "synced", PriorSyncTier: "word", PriorTiming: "categorical"}
	}
	cands := []queue.RemediatedCandidate{
		prior(1, mk("t", ".lrc")),
		prior(2, mk("u", ".txt")),
		prior(3, mk("r", "")),
	}
	// Row 1 keeps its remediation verdict beside a present .lrc and its
	// recorded tier differs (line vs word), so the tier action applies.
	cands[0].PriorSyncTier = "unsynced"
	sink := &recSink{}
	if _, err := RunRemediated(context.Background(), &scriptedQ{}, cands, Options{Apply: true, Backup: sink}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"id":1,"action":"tier","tier":"line","prior_outcome_type":"synced","prior_sync_tier":"unsynced","prior_timing_outcome":"categorical","prior_status":"done"}`,
		`{"id":2,"action":"unsynced","prior_outcome_type":"synced","prior_sync_tier":"word","prior_timing_outcome":"categorical","prior_status":"done"}`,
		`{"id":3,"action":"reset","prior_outcome_type":"synced","prior_sync_tier":"word","prior_timing_outcome":"categorical","prior_status":"done"}`,
	}
	got := strings.Split(strings.TrimSuffix(sink.buf.String(), "\n"), "\n")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("backup records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
