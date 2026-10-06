package instrumentalbackfill

import (
	"bytes"
	"context"
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
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
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
		stamp := fmt.Sprintf("2026-01-01T00:00:%02dZ", i)
		if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status='deferred', created_at=? WHERE id=?`, stamp, it.ID); err != nil {
			t.Fatal(err)
		}
	}
	return q, scanfail.NewDetector(sqlDB)
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
