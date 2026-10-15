package upstreamrepair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tierrepair"
)

const tagged = "[source:innertube]\n[upstream:lyricfind]\n[re:canticle]\n[00:01.00]la\n"

// seed inserts n done innertube rows with a tagged sidecar each (when files)
// and returns the queue and the backup path.
func seed(t *testing.T, n int, files bool) (*queue.DBQueue, string) { //nolint:unparam // reason: every call here passes files=true; the parameter is kept for the deferred adapter PR (#1550 adapter, blocked by #1614), whose test seeds rows with no sidecar
	t.Helper()
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%d", i)
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, provider_lane)
             VALUES ('A', ?, 'a', ?, ?, 'done', 'synced', 'innertube')`, name, name, filepath.Join(dir, name+".flac")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if files {
			if err := os.WriteFile(filepath.Join(dir, name+".lrc"), []byte(tagged), 0o600); err != nil {
				t.Fatalf("lrc: %v", err)
			}
		}
	}
	return queue.NewDBQueue(sqlDB), filepath.Join(dir, "backup.jsonl")
}

func openSink(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := tierrepair.OpenBackup(path)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestPlanOutcomes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+".flac")
	}
	cases := []struct {
		name    string
		c       queue.UpstreamCandidate
		wantUp  string
		wantOut string
	}{
		{"fill", queue.UpstreamCandidate{AudioPath: write("a.lrc", tagged), Lane: "innertube", Status: "done"}, "lyricfind", OutcomeFill},
		{"processing", queue.UpstreamCandidate{AudioPath: write("b.lrc", tagged), Lane: "innertube", Status: "processing"}, "", OutcomeProcessing},
		{"blank", queue.UpstreamCandidate{Lane: "innertube", Status: "done"}, "", OutcomeNoSidecar},
		{"mismatch", queue.UpstreamCandidate{AudioPath: write("c.lrc", tagged), Lane: "musixmatch", Status: "done"}, "", OutcomeSourceMismatch},
		{"notags", queue.UpstreamCandidate{AudioPath: write("d.lrc", "[00:01.00]la\n"), Lane: "innertube", Status: "done"}, "", OutcomeNoTags},
		{"noupstream", queue.UpstreamCandidate{AudioPath: write("e.lrc", "[source:innertube]\n[00:01.00]la\n"), Lane: "innertube", Status: "done"}, "", OutcomeNoUpstream},
		{"unknown", queue.UpstreamCandidate{AudioPath: write("f.lrc", "[source:innertube]\n[upstream:nosuchlicensor]\n[00:01.00]la\n"), Lane: "innertube", Status: "done"}, "", OutcomeUnknownUp},
		{"nosidecar", queue.UpstreamCandidate{AudioPath: filepath.Join(dir, "gone.flac"), Lane: "innertube", Status: "done"}, "", OutcomeNoSidecar},
	}
	for _, tc := range cases {
		up, out := Plan(tc.c)
		if up != tc.wantUp || out != tc.wantOut {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, up, out, tc.wantUp, tc.wantOut)
		}
	}
}

func TestRunApplyFillsRowsAndBacksUpEach(t *testing.T) {
	q, backup := seed(t, 2, true)
	ctx := context.Background()
	cands, _ := List(ctx, q)
	var last, total int64
	res, err := Run(ctx, q, cands, Options{Apply: true, Backup: openSink(t, backup), Progress: func(d, n int64) { last, total = d, n }})
	if err != nil || res.Applied != 2 || res.Counts[OutcomeFill] != 2 || res.Scanned != 2 {
		t.Fatalf("apply: %+v err %v", res, err)
	}
	if last != 2 || total != 2 {
		t.Errorf("progress ended at %d/%d, want 2/2", last, total)
	}
	b, _ := os.ReadFile(backup)
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("%d backup lines, want 2", n)
	}
	if left, _ := List(ctx, q); len(left) != 0 {
		t.Errorf("%d rows still pending after apply", len(left))
	}
}

func TestRunDryRunChangesNothing(t *testing.T) {
	q, backup := seed(t, 2, true)
	ctx := context.Background()
	cands, _ := List(ctx, q)
	res, err := Run(ctx, q, cands, Options{})
	if err != nil || res.Applied != 0 || res.Counts[OutcomeFill] != 2 {
		t.Fatalf("dry run: %+v err %v", res, err)
	}
	if _, serr := os.Stat(backup); serr == nil {
		t.Error("dry run created a backup")
	}
	if left, _ := List(ctx, q); len(left) != 2 {
		t.Errorf("dry run changed rows: %d pending", len(left))
	}
}

// scriptedQ lists fixed candidates and answers the write per id.
type scriptedQ struct {
	cands         []queue.UpstreamCandidate
	raced, failed map[int64]bool
	writes        int
}

func (f *scriptedQ) ListUpstreamCandidates(context.Context, []string) ([]queue.UpstreamCandidate, error) {
	return f.cands, nil
}

func (f *scriptedQ) SetUpstreamIfPending(_ context.Context, c queue.UpstreamCandidate, _ string, backup func() error) (bool, error) {
	f.writes++
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

func fillCands(t *testing.T, n int) []queue.UpstreamCandidate {
	t.Helper()
	dir := t.TempDir()
	var out []queue.UpstreamCandidate
	for i := 1; i <= n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("r%d.lrc", i)), []byte(tagged), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, queue.UpstreamCandidate{ID: int64(i), AudioPath: filepath.Join(dir, fmt.Sprintf("r%d.flac", i)), Lane: "innertube", Status: "done"})
	}
	return out
}

func TestRunApplyWithoutBackupIsRefusedAndWritesNothing(t *testing.T) {
	f := &scriptedQ{}
	if _, err := Run(context.Background(), f, fillCands(t, 2), Options{Apply: true}); !errors.Is(err, ErrNoBackup) || f.writes != 0 {
		t.Errorf("err=%v writes=%d, want ErrNoBackup and 0 writes", err, f.writes)
	}
}

func TestRunRacedAndWriteFailedAreCountedAndRacedIsNeverBackedUp(t *testing.T) {
	f := &scriptedQ{raced: map[int64]bool{1: true}, failed: map[int64]bool{2: true}}
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	res, err := Run(context.Background(), f, fillCands(t, 3), Options{Apply: true, Backup: openSink(t, backup)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[OutcomeRaced] != 1 || res.WriteFailed != 1 || res.Applied != 1 || res.Counts[OutcomeFill] != 1 {
		t.Errorf("counts %+v", res)
	}
	b, _ := os.ReadFile(backup)
	if got := strings.Count(string(b), "\n"); got != 1 || !strings.Contains(string(b), `"id":3`) {
		t.Errorf("backup %q, want only row 3", b)
	}
}

type failSink struct{}

func (failSink) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failSink) Sync() error               { return nil }

func TestRunBackupWriteFailureLeavesRowUnchangedAndWrapsErrBackupWrite(t *testing.T) {
	err := appendBackup(failSink{}, queue.UpstreamCandidate{ID: 1}, "lyricfind")
	if !errors.Is(err, ErrBackupWrite) {
		t.Fatalf("err %v, want ErrBackupWrite", err)
	}
	q, _ := seed(t, 1, true)
	ctx := context.Background()
	cands, _ := List(ctx, q)
	res, rerr := Run(ctx, q, cands, Options{Apply: true, Backup: failSink{}})
	if rerr != nil || res.WriteFailed != 1 || res.Applied != 0 {
		t.Fatalf("%+v err %v", res, rerr)
	}
	if left, _ := List(ctx, q); len(left) != 1 {
		t.Errorf("row changed despite failed backup: %d pending", len(left))
	}
}

func TestRunCancelStopsAtRowBoundaryWithPartialResult(t *testing.T) {
	q, backup := seed(t, 3, true)
	ctx, cancel := context.WithCancel(context.Background())
	cands, _ := List(ctx, q)
	res, err := Run(ctx, q, cands, Options{Apply: true, Backup: openSink(t, backup), Progress: func(d, _ int64) {
		if d == 1 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) || res.Applied != 1 || res.Scanned != 3 {
		t.Fatalf("res %+v err %v, want 1 applied and context.Canceled", res, err)
	}
	if left, _ := List(context.Background(), q); len(left) != 2 {
		t.Errorf("%d rows pending, want 2 untouched", len(left))
	}
}

// cancelOnLastWrite cancels the run's context inside the final row's write.
type cancelOnLastWrite struct {
	*queue.DBQueue
	cancel context.CancelFunc
}

func (c cancelOnLastWrite) SetUpstreamIfPending(ctx context.Context, cand queue.UpstreamCandidate, up string, b func() error) (bool, error) {
	c.cancel()
	return c.DBQueue.SetUpstreamIfPending(ctx, cand, up, b)
}

func TestRunCountsCancelCausedWriteInWriteFailedAndCancelFailed(t *testing.T) {
	q, backup := seed(t, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	cands, _ := List(ctx, q)
	res, err := Run(ctx, cancelOnLastWrite{DBQueue: q, cancel: cancel}, cands, Options{Apply: true, Backup: openSink(t, backup)})
	if err != nil {
		t.Fatal(err)
	}
	if res.WriteFailed != 1 || res.CancelFailed != 1 {
		t.Errorf("WriteFailed %d CancelFailed %d, want 1 and 1 (CLI stdout counts it)", res.WriteFailed, res.CancelFailed)
	}
}

func TestRunLogsFixedReasonNotBackupPathOnBackupWriteFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	q, _ := seed(t, 1, true)
	ctx := context.Background()
	cands, _ := List(ctx, q)
	sink := pathErrSink{err: &fs.PathError{Op: "write", Path: "/sentinel/backup.jsonl", Err: errors.New("disk full")}}
	res, err := Run(ctx, q, cands, Options{Apply: true, Backup: sink})
	if err != nil || res.WriteFailed != 1 {
		t.Fatalf("%+v err %v", res, err)
	}
	logged := buf.String()
	if strings.Contains(logged, "sentinel") {
		t.Errorf("log leaks the backup path: %s", logged)
	}
	if !strings.Contains(logged, "reason=") {
		t.Errorf("log has no fixed reason: %s", logged)
	}
}

type pathErrSink struct{ err error }

func (s pathErrSink) Write([]byte) (int, error) { return 0, s.err }
func (pathErrSink) Sync() error                 { return nil }
