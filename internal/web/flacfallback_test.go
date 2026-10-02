package web

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fakeFlac = []byte("fLaC-fake-converted-bytes")

// countingConverter writes fakeFlac and counts its calls.
type countingConverter struct {
	calls atomic.Int32
	gate  chan struct{} // when non-nil, each call blocks until it is closed
	err   error
}

func (c *countingConverter) convert(ctx context.Context, _ *os.File, _ string, out string) error {
	c.calls.Add(1)
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.err != nil {
		return c.err
	}
	return os.WriteFile(out, fakeFlac, 0o600)
}

func openSrc(t *testing.T, dir string) (string, func() (*os.File, error), os.FileInfo) {
	t.Helper()
	p := filepath.Join(dir, "song.m4a")
	if err := os.WriteFile(p, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, func() (*os.File, error) { return os.Open(p) }, fi
}

func readAll(t *testing.T, f *os.File) []byte {
	t.Helper()
	defer func() { _ = f.Close() }()
	b := make([]byte, 1<<10)
	n, _ := f.Read(b)
	return b[:n]
}

func TestFlacKeyChangesWithPathMtimeSize(t *testing.T) {
	dir := t.TempDir()
	p, _, fi := openSrc(t, dir)
	base := flacKey(p, fi)
	if flacKey(p, fi) != base {
		t.Fatal("key is not stable")
	}
	if flacKey(p+"x", fi) == base {
		t.Error("key ignores the path")
	}
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(p)
	if flacKey(p, fi2) == base {
		t.Error("key ignores the mtime")
	}
	if err := os.WriteFile(p, append(previewBytes, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	fi3, _ := os.Stat(p)
	if flacKey(p, fi3) == base {
		t.Error("key ignores the size")
	}
}

func TestFlacCacheReusesAndReconvertsOnChange(t *testing.T) {
	conv := &countingConverter{}
	c, err := newFlacCache(filepath.Join(t.TempDir(), "c"), 1<<20, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := openSrc(t, t.TempDir())
	for i := 0; i < 2; i++ {
		f, err := c.Get(context.Background(), p, fi, open)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, f); !bytes.Equal(got, fakeFlac) {
			t.Fatalf("body = %q", got)
		}
	}
	if conv.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (second Get should hit the cache)", conv.calls.Load())
	}
	// A changed file (new mtime) converts again.
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(p)
	f, err := c.Get(context.Background(), p, fi2, open)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if conv.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 after the file changed", conv.calls.Load())
	}
}

func TestFlacCacheClearedOnStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "old.flac")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newFlacCache(dir, 1<<20, (&countingConverter{}).convert); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale cache entry survived startup: %v", err)
	}
}

func TestFlacCacheEvictsOldestOverCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	conv := &countingConverter{}
	// Each fake conversion is len(fakeFlac) bytes; room for two.
	c, err := newFlacCache(dir, int64(len(fakeFlac))*2, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	var keys []string
	for i, name := range []string{"a.m4a", "b.m4a", "c.m4a"} {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, previewBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		f, err := c.Get(context.Background(), p, fi, func() (*os.File, error) { return os.Open(p) })
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		k := flacKey(p, fi)
		keys = append(keys, k)
		// Backdate so "oldest" is unambiguous regardless of fs timestamp resolution.
		old := time.Now().Add(time.Duration(i-10) * time.Minute)
		if i < 2 {
			_ = os.Chtimes(filepath.Join(dir, k), old, old)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, keys[0])); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("oldest entry was not evicted: %v", err)
	}
	for _, k := range keys[1:] {
		if _, err := os.Stat(filepath.Join(dir, k)); err != nil {
			t.Errorf("entry %s evicted though within cap: %v", k[:8], err)
		}
	}
}

func TestFlacCacheKeepsNewestEvenWhenOverCap(t *testing.T) {
	conv := &countingConverter{}
	c, err := newFlacCache(filepath.Join(t.TempDir(), "c"), 1, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := openSrc(t, t.TempDir())
	f, err := c.Get(context.Background(), p, fi, open)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f); !bytes.Equal(got, fakeFlac) {
		t.Fatalf("a single file over the cap must still be served, got %q", got)
	}
}

func TestFlacCacheSingleConversionForConcurrentRequests(t *testing.T) {
	conv := &countingConverter{gate: make(chan struct{})}
	c, err := newFlacCache(filepath.Join(t.TempDir(), "c"), 1<<20, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := openSrc(t, t.TempDir())
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := c.Get(context.Background(), p, fi, open)
			if err == nil {
				_ = f.Close()
			}
			errs <- err
		}()
	}
	// Wait until every request is parked on the one in-flight job.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		w := 0
		for _, j := range c.inflight {
			w = j.waiters
		}
		c.mu.Unlock()
		if w == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests joined the job", w, n)
		}
		time.Sleep(time.Millisecond)
	}
	close(conv.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if conv.calls.Load() != 1 {
		t.Fatalf("conversions = %d, want exactly 1", conv.calls.Load())
	}
}

func TestFlacCacheCancelsWhenEveryWaiterLeaves(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	c, err := newFlacCache(filepath.Join(t.TempDir(), "c"), 1<<20,
		func(ctx context.Context, _ *os.File, _, _ string) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		})
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := openSrc(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, p, fi, open)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v, want context canceled", err)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("conversion was not canceled after its only waiter left")
	}
}

func TestFlacCacheDoesNotCacheFailures(t *testing.T) {
	conv := &countingConverter{err: errors.New("boom")}
	dir := filepath.Join(t.TempDir(), "c")
	c, err := newFlacCache(dir, 1<<20, conv.convert)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := openSrc(t, t.TempDir())
	if _, err := c.Get(context.Background(), p, fi, open); err == nil {
		t.Fatal("want the conversion error")
	}
	conv.err = nil
	f, err := c.Get(context.Background(), p, fi, open)
	if err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	_ = f.Close()
	if conv.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (a failure must not be cached)", conv.calls.Load())
	}
}

// ---- real ffmpeg (skipped when none is on PATH) ----

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; conversion tests need it")
	}
	return bin
}

// makeFixture encodes a 2.3 s tone (an awkward, non-round sample count) with codec into dir/name.
func makeFixture(t *testing.T, bin, dir, name string, codecArgs ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args := append([]string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2.3"}, codecArgs...)
	args = append(args, out)
	if b, err := exec.Command(bin, args...).CombinedOutput(); err != nil { //nolint:gosec // reason: G204 -- test fixture generation with a fixed argv
		t.Skipf("this ffmpeg cannot build the %s fixture: %v: %s", name, err, b)
	}
	return out
}

// pcm decodes path to signed 16-bit little-endian PCM, so the sample count is
// counted from decoded data, never read from a container duration.
func pcm(t *testing.T, bin, path string) []byte {
	t.Helper()
	b, err := exec.Command(bin, "-nostdin", "-loglevel", "error", "-i", path, "-map", "0:a:0", "-f", "s16le", "-ac", "1", "-ar", "44100", "-").Output() //nolint:gosec // reason: G204 -- fixed argv in a test
	if err != nil {
		t.Fatalf("decode %s: %v", filepath.Base(path), err)
	}
	return b
}

func TestFFmpegFlacConversionIsSampleExact(t *testing.T) {
	bin := requireFFmpeg(t)
	dir := t.TempDir()
	fixtures := map[string]string{
		"alac": makeFixture(t, bin, dir, "t.m4a", "-c:a", "alac"),
		"wma":  makeFixture(t, bin, dir, "t.wma", "-c:a", "wmav2"),
	}
	conv := ffmpegFlacConverter(bin)
	for name, src := range fixtures {
		t.Run(name, func(t *testing.T) {
			in, err := os.Open(src)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close() }()
			out := filepath.Join(t.TempDir(), "o.flac")
			if err := conv(context.Background(), in, src, out); err != nil {
				t.Fatalf("convert: %v", err)
			}
			want, got := pcm(t, bin, src), pcm(t, bin, out)
			if len(want) == 0 {
				t.Fatal("source decoded to no samples")
			}
			if len(got) != len(want) {
				t.Fatalf("decoded samples: flac %d, source %d (bytes of s16 mono)", len(got)/2, len(want)/2)
			}
		})
	}
}

func TestFFmpegFlacConversionFailureNamesFFmpeg(t *testing.T) {
	bin := requireFFmpeg(t)
	src := filepath.Join(t.TempDir(), "not-audio.m4a")
	if err := os.WriteFile(src, []byte("this is not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := os.Open(src)
	defer func() { _ = in.Close() }()
	err := ffmpegFlacConverter(bin)(context.Background(), in, src, filepath.Join(t.TempDir(), "o.flac"))
	if err == nil || !strings.Contains(err.Error(), "ffmpeg flac conversion") {
		t.Fatalf("err = %v, want an ffmpeg conversion error", err)
	}
}
