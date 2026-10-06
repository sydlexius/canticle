package instrumentalbackfill

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanfail"
)

// failingDetector counts Detect calls and returns a fixed error.
type failingDetector struct {
	err   error
	calls int
}

func (d *failingDetector) Detect(context.Context, string) (detector.Result, error) {
	d.calls++
	return detector.Result{}, d.err
}

func realFailureStore(t *testing.T) *scanfail.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return scanfail.NewDetector(sqlDB)
}

// sampleFailure is a decode failure: a real process that ran and exited 1, the
// shape ffmpeg produces on an undecodable file.
func sampleFailure() error {
	err := exec.Command("sh", "-c", "exit 1").Run()
	return ffmpeg.SampleError("detector", err, "Invalid data found")
}

// A sampler that could not START (ffmpeg/nice/ionice missing or not
// executable) is a host failure: retried every cycle, never remembered.
func TestRun_SamplerStartFailureIsNotRemembered(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ok.flac")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	startErr := exec.Command("/nonexistent/ffmpeg").Run()
	det := &failingDetector{err: ffmpeg.SampleError("detector", startErr, "")}
	bf := New(newFakeStore(t, item(5, src)), det, &fakeWriter{}).WithFailureStore(realFailureStore(t))
	for range 2 {
		if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true}); err != nil {
			t.Fatal(err)
		}
	}
	if det.calls != 2 {
		t.Fatalf("calls = %d; a start failure must be retried every cycle (2)", det.calls)
	}
}

// pathDetector records the paths it was asked about and reports an outage, so
// nothing is mutated and nothing is remembered.
type pathDetector struct{ paths []string }

func (d *pathDetector) Detect(_ context.Context, p string) (detector.Result, error) {
	d.paths = append(d.paths, p)
	return detector.Result{}, detector.ErrClassifierUnavailable
}

// backlog seeds real deferred, never-scored rows (in creation order) over one
// SQLite database and returns the queue and the detector failure store on it.
func backlog(t *testing.T, srcs ...string) (*queue.DBQueue, *scanfail.Store) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	seedDeferred(t, sqlDB, 0, srcs...)
	return queue.NewDBQueue(sqlDB), scanfail.NewDetector(sqlDB)
}

// seedDeferred enqueues srcs as deferred, never-scored rows whose created_at
// seconds start at first, so rows order as seeded.
func seedDeferred(t *testing.T, sqlDB *sql.DB, first int, srcs ...string) {
	t.Helper()
	ctx := context.Background()
	q := queue.NewDBQueue(sqlDB)
	for i, src := range srcs {
		if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		it, err := q.Enqueue(ctx, models.Inputs{
			Track:      models.Track{ArtistName: "A", TrackName: filepath.Base(src)},
			Outdir:     "out",
			Filename:   filepath.Base(src) + ".lrc",
			SourcePath: src,
		}, 1)
		if err != nil {
			t.Fatal(err)
		}
		// Same priority; created_at then id order the rows as seeded.
		stamp := time.Date(2026, 1, 1, 0, 0, first+i, 0, time.UTC).Format(time.RFC3339)
		if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status='deferred', created_at=? WHERE id=?`, stamp, it.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func remember(t *testing.T, fs *scanfail.Store, src string) {
	t.Helper()
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.RecordFailure(context.Background(), src, fi.ModTime().UnixNano(), fi.Size(), sampleFailure()); err != nil {
		t.Fatal(err)
	}
}

// The first Limit rows being remembered-unsampleable must not stall the sweep:
// the healthy row behind them is attempted (#1149).
func TestRun_RememberedRowsDoNotStallHealthyRows(t *testing.T) {
	dir := t.TempDir()
	bad1, bad2, good := filepath.Join(dir, "b1.flac"), filepath.Join(dir, "b2.flac"), filepath.Join(dir, "g.flac")
	q, fs := backlog(t, bad1, bad2, good)
	remember(t, fs, bad1)
	remember(t, fs, bad2)
	det := &pathDetector{}
	bf := New(q, det, &fakeWriter{}).WithFailureStore(fs)
	if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true, Limit: 2}); err != nil {
		t.Fatal(err)
	}
	if len(det.paths) != 1 || det.paths[0] != good {
		t.Fatalf("detector saw %v; want only the healthy row %s", det.paths, good)
	}
}

// Within the remembered tail, a file changed since its failure is reached even
// when Limit unchanged remembered rows sort ahead of it: gather pages past them.
func TestRun_PagesPastUnchangedRememberedRows(t *testing.T) {
	dir := t.TempDir()
	stale, changed := filepath.Join(dir, "s.flac"), filepath.Join(dir, "c.flac")
	q, fs := backlog(t, stale, changed)
	remember(t, fs, stale)
	remember(t, fs, changed)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(changed, later, later); err != nil {
		t.Fatal(err)
	}
	det := &pathDetector{}
	bf := New(q, det, &fakeWriter{}).WithFailureStore(fs)
	res, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(det.paths) != 1 || det.paths[0] != changed {
		t.Fatalf("detector saw %v; want only the changed file %s (res %+v)", det.paths, changed, res)
	}
}

// An unsampleable file is attempted once per file version; a changed mtime
// retries it (#1149 AC1).
func TestRun_UnsampleableAttemptedOncePerFileVersion(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bad.flac")
	if err := os.WriteFile(src, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	det := &failingDetector{err: sampleFailure()}
	bf := New(newFakeStore(t, item(7, src)), det, &fakeWriter{}).WithFailureStore(realFailureStore(t))
	opts := Options{GlobalDetectDefault: true}

	first, err := bf.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if det.calls != 1 || first.Errors != 1 {
		t.Fatalf("first run: calls=%d res=%+v; want 1 attempt, 1 error", det.calls, first)
	}
	second, err := bf.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// AC4: nothing to report on a converged cycle.
	if det.calls != 1 || second.Errors != 0 || second.SkippedUnsampleable != 1 {
		t.Fatalf("second run: calls=%d res=%+v; want no new attempt, 0 errors, 1 skipped", det.calls, second)
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := bf.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if det.calls != 2 {
		t.Fatalf("calls after mtime change = %d; want the file retried (2)", det.calls)
	}
}

// A missing audio file is stale-path territory for prune, not a row error (AC2).
func TestRun_MissingAudioIsNotARowError(t *testing.T) {
	det := &failingDetector{err: ffmpeg.MissingAudioError("detector", "/gone.flac")}
	bf := New(newFakeStore(t, item(9, "/gone.flac")), det, &fakeWriter{}).WithFailureStore(realFailureStore(t))

	res, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 0 || res.SkippedMissing != 1 {
		t.Fatalf("res = %+v; want Errors=0 SkippedMissing=1", res)
	}
}

// A non-sampling failure (classifier outage) is retried, never remembered.
func TestRun_OutageIsNotRemembered(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ok.flac")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	det := &failingDetector{err: detector.ErrClassifierUnavailable}
	bf := New(newFakeStore(t, item(3, src)), det, &fakeWriter{}).WithFailureStore(realFailureStore(t))
	for range 2 {
		if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true}); err != nil {
			t.Fatal(err)
		}
	}
	if det.calls != 2 {
		t.Fatalf("calls = %d; an outage must be retried every cycle (2)", det.calls)
	}
}

// The row's Warn carries the queue id and no path (#1149 AC3).
func TestRun_UnsampleableWarnNamesQueueID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	src := filepath.Join(t.TempDir(), "bad.flac")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bf := New(newFakeStore(t, item(42, src)), &failingDetector{err: sampleFailure()}, &fakeWriter{}).WithFailureStore(realFailureStore(t))
	if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "queue_id=42") {
		t.Errorf("warn lacks queue id: %q", out)
	}
	if strings.Contains(out, src) {
		t.Errorf("warn leaks the audio path: %q", out)
	}
}

// rememberedTail seeds n unchanged remembered rows, then one remembered row
// whose file changed since its failure, all in the remembered tail.
func rememberedTail(t *testing.T, n int) (*sql.DB, *scanfail.Store, string) {
	t.Helper()
	dir := t.TempDir()
	srcs := make([]string, 0, n+1)
	for i := range n {
		srcs = append(srcs, filepath.Join(dir, fmt.Sprintf("s%03d.flac", i)))
	}
	changed := filepath.Join(dir, "changed.flac")
	srcs = append(srcs, changed)
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	seedDeferred(t, sqlDB, 0, srcs...)
	fs := scanfail.NewDetector(sqlDB)
	for _, src := range srcs {
		remember(t, fs, src)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(changed, later, later); err != nil {
		t.Fatal(err)
	}
	return sqlDB, fs, changed
}

func countPath(paths []string, want string) int {
	n := 0
	for _, p := range paths {
		if p == want {
			n++
		}
	}
	return n
}

// A changed file behind more than maxGatherPages*Limit unchanged remembered rows
// is reached by later cycles of one Backfiller, and the tail wraps so it is
// reached again after the backlog ends (#1149).
func TestRun_CursorRotatesPastRememberedPrefix(t *testing.T) {
	sqlDB, fs, changed := rememberedTail(t, 3*maxGatherPages)
	det := &pathDetector{}
	bf := New(queue.NewDBQueue(sqlDB), det, &fakeWriter{}).WithFailureStore(fs)
	opts := Options{GlobalDetectDefault: true, Limit: 1}
	firstHit := 0
	for run := 1; run <= 8; run++ {
		res, err := bf.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Candidates > maxGatherPages*opts.Limit {
			t.Fatalf("run %d examined %d rows; the per-run page cap is %d", run, res.Candidates, maxGatherPages*opts.Limit)
		}
		if firstHit == 0 && countPath(det.paths, changed) > 0 {
			firstHit = run
		}
	}
	if firstHit == 0 || firstHit > 4 {
		t.Fatalf("changed file first attempted on run %d; want within 4 runs (detector saw %v)", firstHit, det.paths)
	}
	if got := countPath(det.paths, changed); got < 2 {
		t.Fatalf("changed file attempted %d times in 8 runs; the tail must wrap and reach it again", got)
	}
}

// A fresh row arriving at the head while the cursor sits deep in the tail is
// still attempted on the very next run: page 0 always reads from offset 0.
func TestRun_CursorKeepsHeadPriority(t *testing.T) {
	sqlDB, fs, _ := rememberedTail(t, 3*maxGatherPages)
	det := &pathDetector{}
	bf := New(queue.NewDBQueue(sqlDB), det, &fakeWriter{}).WithFailureStore(fs)
	opts := Options{GlobalDetectDefault: true, Limit: 1}
	if _, err := bf.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh.flac")
	seedDeferred(t, sqlDB, 100, fresh)
	det.paths = nil
	if _, err := bf.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(det.paths) != 1 || det.paths[0] != fresh {
		t.Fatalf("detector saw %v; want the fresh head row %s", det.paths, fresh)
	}
}

// Opted-out rows never produce a candidate, so a capped gather over a long run
// of them must still walk the whole backlog across cycles, wrap at its end, and
// settle the cursor back to the start (#1149).
func TestRun_CursorWalksWholeBacklogAndWraps(t *testing.T) {
	off := false
	items := make([]queue.WorkItem, 0, 3*maxGatherPages)
	for i := range 3 * maxGatherPages {
		items = append(items, queue.WorkItem{ID: int64(i + 1), DetectInstrumental: &off})
	}
	examined := map[int]bool{}
	bf := New(&offsetProbe{fakeStore: newFakeStore(t, items...), seen: examined}, &fakeDetector{}, &fakeWriter{})
	opts := Options{GlobalDetectDefault: true, Limit: 1}
	for run := 0; run < 12; run++ {
		res, err := bf.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Candidates > maxGatherPages*opts.Limit {
			t.Fatalf("run %d examined %d rows; cap is %d", run, res.Candidates, maxGatherPages*opts.Limit)
		}
	}
	for off := range items {
		if !examined[off] {
			t.Fatalf("offset %d never read across 12 capped runs; the cursor skipped it", off)
		}
	}
}

// A tail read that starts mid-backlog and runs off the end wraps to the start
// of the tail, stops where it began, and resets the cursor.
func TestRun_CursorWrapStopsWhereTailReadBegan(t *testing.T) {
	off := false
	items := make([]queue.WorkItem, 0, 6)
	for i := range 6 {
		items = append(items, queue.WorkItem{ID: int64(i + 1), DetectInstrumental: &off})
	}
	probe := &offsetProbe{fakeStore: newFakeStore(t, items...), seen: map[int]bool{}}
	bf := New(probe, &fakeDetector{}, &fakeWriter{})
	bf.setCursor(3)
	if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	for o := range items {
		if !probe.seen[o] {
			t.Fatalf("offset %d not read; the wrap must cover the skipped middle (seen %v)", o, probe.seen)
		}
	}
	bf.cursorMu.Lock()
	defer bf.cursorMu.Unlock()
	if bf.cursor != 0 {
		t.Fatalf("cursor = %d after a completed wrap; want 0", bf.cursor)
	}
}

// offsetProbe records every offset a gather reads.
type offsetProbe struct {
	*fakeStore
	seen map[int]bool
}

func (p *offsetProbe) ListUnclassified(ctx context.Context, opts queue.ListUnclassifiedOptions) ([]queue.WorkItem, error) {
	p.seen[opts.Offset] = true
	return p.fakeStore.ListUnclassified(ctx, opts)
}

// A row repeated across pages (a concurrent writer reordered the set) is
// examined once, not twice.
func TestRun_GatherDeduplicatesRowsByID(t *testing.T) {
	off := false
	fs := newFakeStore(t,
		queue.WorkItem{ID: 1, DetectInstrumental: &off},
		queue.WorkItem{ID: 1, DetectInstrumental: &off},
	)
	bf := New(fs, &fakeDetector{}, &fakeWriter{})
	res, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 1 {
		t.Fatalf("Candidates = %d; want 1 (duplicate id examined once)", res.Candidates)
	}
}

// A listing failure on a capped gather is returned wrapped, not swallowed.
func TestRun_GatherListErrorIsReturned(t *testing.T) {
	fs := newFakeStore(t)
	fs.listErr = fmt.Errorf("boom")
	bf := New(fs, &fakeDetector{}, &fakeWriter{})
	if _, err := bf.Run(context.Background(), Options{GlobalDetectDefault: true, Limit: 2}); err == nil || !strings.Contains(err.Error(), "list unclassified") {
		t.Fatalf("err = %v; want a wrapped list error", err)
	}
}

// A failing backup-trail callback degrades the trail only: the row still lands.
func TestRun_OutcomeCallbackErrorDoesNotUndoSettle(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Outcome:             func(Outcome) error { return fmt.Errorf("trail full") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.settleCalls != 1 || res.MarkersWritten != 1 {
		t.Fatalf("settleCalls=%d markers=%d; want the row settled despite the callback error", store.settleCalls, res.MarkersWritten)
	}
}
