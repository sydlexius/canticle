package instrumentalbackfill

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/ffmpeg"
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

func sampleFailure() error {
	return ffmpeg.SampleError("detector", os.ErrInvalid, "Invalid data found")
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
