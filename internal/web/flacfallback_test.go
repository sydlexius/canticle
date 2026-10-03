package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
)

var fakeFlac = []byte("fLaC-fake-converted-bytes")

// countingConverter writes fakeFlac and counts its calls.
type countingConverter struct {
	calls atomic.Int32
	gate  chan struct{} // when non-nil, each call blocks until it is closed
	err   error         // returned after writing to out, so a failure leaves a file to clean
	in    atomic.Pointer[os.File]
}

func (c *countingConverter) convert(ctx context.Context, in *os.File, _ string, out string) error {
	c.calls.Add(1)
	c.in.Store(in)
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := os.WriteFile(out, fakeFlac, 0o600); err != nil || c.err != nil {
		return errors.Join(err, c.err)
	}
	return nil
}

func newTestCache(t *testing.T, max int64, conv flacConverter) *flacCache {
	t.Helper()
	c, err := newFlacCache(t.TempDir(), max, conv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// srcNamed writes a source file name into dir and returns its path, opener and info.
func srcNamed(t *testing.T, dir, name string) (string, func() (*os.File, error), os.FileInfo) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, func() (*os.File, error) { return os.Open(p) }, fi
}

// getAsync runs Get in a goroutine and reports the body it served.
func getAsync(c *flacCache, ctx context.Context, p string, open func() (*os.File, error), fi os.FileInfo) chan error {
	ch := make(chan error, 1)
	go func() {
		f, err := c.Get(ctx, p, fi, open)
		if err == nil { // read the served handle: once opened, the name may be evicted
			var b []byte
			b, err = io.ReadAll(f)
			_ = f.Close()
			if err == nil && !bytes.Equal(b, fakeFlac) {
				err = errors.New("served body is not the conversion: " + string(b))
			}
		}
		ch <- err
	}()
	return ch
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (c *flacCache) waiters(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.inflight[key]; j != nil {
		return j.waiters
	}
	return 0
}

func TestFlacKeyChangesWithPathMtimeSize(t *testing.T) {
	dir := t.TempDir()
	p, _, fi := srcNamed(t, dir, "song.m4a")
	base := flacKey(p, fi)
	if flacKey(p, fi) != base || flacKey(p+"x", fi) == base {
		t.Fatal("key is not stable, or ignores the path")
	}
	_ = os.Chtimes(p, time.Now(), time.Now().Add(time.Hour))
	fi2, _ := os.Stat(p)
	_ = os.WriteFile(p, append(previewBytes, 'x'), 0o600)
	_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
	fi3, _ := os.Stat(p)
	if fi3.ModTime() != fi.ModTime() || flacKey(p, fi2) == base || flacKey(p, fi3) == base {
		t.Error("key ignores the mtime or the size")
	}
}

func TestFlacCacheReusesAndReconvertsOnChange(t *testing.T) {
	conv := &countingConverter{}
	c := newTestCache(t, 1<<20, conv.convert)
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	for i := 0; i < 2; i++ {
		if err := <-getAsync(c, context.Background(), p, open, fi); err != nil {
			t.Fatal(err)
		}
	}
	if conv.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (second Get should hit the cache)", conv.calls.Load())
	}
	if _, err := conv.in.Load().Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("source handle after the conversion: stat err = %v, want it closed", err)
	}
	// A changed file (new mtime) converts again.
	_ = os.Chtimes(p, time.Now(), time.Now().Add(time.Hour))
	fi2, _ := os.Stat(p)
	if err := <-getAsync(c, context.Background(), p, open, fi2); err != nil || conv.calls.Load() != 2 {
		t.Fatalf("err = %v, calls = %d, want 2 after the file changed", err, conv.calls.Load())
	}
}

// Two instances on one parent get separate private dirs: starting the second
// never touches the first's in-flight conversion, and Close removes only its own.
func TestFlacCacheInstancesDoNotShareADir(t *testing.T) {
	parent := t.TempDir()
	gate, started := make(chan struct{}), make(chan struct{})
	conv := func(_ context.Context, _ *os.File, _, out string) error {
		f, err := os.Create(out) // held open across the gate, as ffmpeg holds its output
		if err != nil {
			return err
		}
		close(started)
		<-gate
		_, err = f.Write(fakeFlac)
		return errors.Join(err, f.Close())
	}
	c1, err := newFlacCache(parent, 1<<20, conv)
	if err != nil {
		t.Fatal(err)
	}
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	done := getAsync(c1, context.Background(), p, open, fi)
	<-started
	c2, err := newFlacCache(parent, 1<<20, conv)
	if err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("first instance's Get after a second instance started: %v", err)
	}
	if st, err := os.Stat(c1.dir); err != nil || st.Mode().Perm() != 0o700 || c1.dir == c2.dir {
		t.Fatalf("cache dirs %q / %q: want distinct 0700 dirs (%v, %v)", c1.dir, c2.dir, st, err)
	}
	if err := errors.Join(c1.Close(), c2.Close()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 0 {
		t.Fatalf("Close left %d entries under the parent", len(ents))
	}
}

func TestFlacCacheEvictsOldestOverCapAndHitsCountAsRecent(t *testing.T) {
	conv := &countingConverter{}
	c := newTestCache(t, int64(len(fakeFlac))*2, conv.convert) // room for two
	src := t.TempDir()
	get := func(name string) string {
		p := filepath.Join(src, name)
		fi, err := os.Stat(p)
		if err != nil {
			_, _, fi = srcNamed(t, src, name)
		}
		if err := <-getAsync(c, context.Background(), p, func() (*os.File, error) { return os.Open(p) }, fi); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(c.dir, flacKey(p, fi))
	}
	a, b := get("a.m4a"), get("b.m4a")
	// Backdate so "oldest" is unambiguous regardless of fs timestamp resolution.
	for i, path := range []string{a, b} {
		old := time.Now().Add(time.Duration(i-20) * time.Minute)
		_ = os.Chtimes(path, old, old)
	}
	get("a.m4a") // a cache hit: a is now the most recent
	cpath := get("c.m4a")
	for path, want := range map[string]bool{a: true, b: false, cpath: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", filepath.Base(path)[:8], err == nil, want)
		}
	}
	if conv.calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (the second a.m4a is a hit)", conv.calls.Load())
	}
}

// A result is pinned until its waiters have opened it: two conversions that
// finish together in a cache with room for one must both be served (so is a
// single file over the cap).
func TestFlacCacheEvictionSparesUnservedResults(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		conv := &countingConverter{gate: make(chan struct{})}
		c := newTestCache(t, int64(len(fakeFlac)), conv.convert)
		src := t.TempDir()
		var errs []chan error
		for _, name := range []string{"a.m4a", "b.m4a"} {
			p, open, fi := srcNamed(t, src, name)
			errs = append(errs, getAsync(c, context.Background(), p, open, fi))
		}
		waitFor(t, "both conversions to start", func() bool { return conv.calls.Load() == 2 })
		close(conv.gate)
		for _, ch := range errs {
			if err := <-ch; err != nil {
				t.Fatalf("iteration %d: a finished conversion was not served: %v", iter, err)
			}
		}
	}
}

// A partial conversion is never visible under the cached name, and its bytes
// count toward the cap: with room for two entries, a cached one goes when a
// finished one plus a large partial pass the cap.
func TestFlacCacheHidesAndCountsPartials(t *testing.T) {
	big, bigWritten := make(chan struct{}), make(chan struct{})
	conv := func(ctx context.Context, in *os.File, name, out string) error {
		if strings.HasSuffix(name, "big.m4a") {
			_ = os.WriteFile(out, make([]byte, 1024), 0o600)
			close(bigWritten)
			<-big
		}
		return os.WriteFile(out, fakeFlac, 0o600)
	}
	c := newTestCache(t, int64(len(fakeFlac))*2, conv)
	defer close(big)
	src := t.TempDir()
	x, xopen, xfi := srcNamed(t, src, "x.m4a")
	if err := <-getAsync(c, context.Background(), x, xopen, xfi); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(c.dir, flacKey(x, xfi)), old, old)
	bp, bopen, bfi := srcNamed(t, src, "big.m4a")
	_ = getAsync(c, context.Background(), bp, bopen, bfi)
	<-bigWritten
	if _, err := os.Stat(filepath.Join(c.dir, flacKey(bp, bfi))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a partial conversion is visible under the cached name: %v", err)
	}
	y, yopen, yfi := srcNamed(t, src, "y.m4a")
	if err := <-getAsync(c, context.Background(), y, yopen, yfi); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.dir, flacKey(x, xfi))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest entry survived though cached + partial bytes pass the cap: %v", err)
	}
}

func TestFlacCacheBoundsConcurrentConversions(t *testing.T) {
	conv := &countingConverter{gate: make(chan struct{})}
	c := newTestCache(t, 1<<20, conv.convert)
	src := t.TempDir()
	var errs []chan error
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		p, open, fi := srcNamed(t, src, name+".m4a")
		errs = append(errs, getAsync(c, context.Background(), p, open, fi))
	}
	waitFor(t, "the first conversions", func() bool { return conv.calls.Load() >= flacMaxConversions })
	time.Sleep(50 * time.Millisecond) // room for an unbounded cache to start the rest
	if n := conv.calls.Load(); n != flacMaxConversions {
		t.Fatalf("%d conversions running at once, want %d", n, flacMaxConversions)
	}
	close(conv.gate)
	for _, ch := range errs {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlacCacheSingleConversionForConcurrentRequests(t *testing.T) {
	conv := &countingConverter{gate: make(chan struct{})}
	c := newTestCache(t, 1<<20, conv.convert)
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	var errs []chan error
	for i := 0; i < 8; i++ {
		errs = append(errs, getAsync(c, context.Background(), p, open, fi))
	}
	waitFor(t, "every request to join the job", func() bool { return c.waiters(flacKey(p, fi)) == len(errs) })
	close(conv.gate)
	for _, ch := range errs {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if conv.calls.Load() != 1 {
		t.Fatalf("conversions = %d, want exactly 1", conv.calls.Load())
	}
}

// One waiter leaving (the creator, here) does not cancel a conversion another
// still wants; the last one leaving does.
func TestFlacCacheCancelsOnlyWhenEveryWaiterLeaves(t *testing.T) {
	var jctx atomic.Value
	conv := &countingConverter{gate: make(chan struct{})}
	c := newTestCache(t, 1<<20, func(ctx context.Context, in *os.File, name, out string) error {
		jctx.Store(ctx)
		return conv.convert(ctx, in, name, out)
	})
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	first := getAsync(c, ctx1, p, open, fi) // the creator
	waitFor(t, "the conversion to start", func() bool { return conv.calls.Load() == 1 })
	second := getAsync(c, ctx2, p, open, fi)
	waitFor(t, "both requests to join", func() bool { return c.waiters(flacKey(p, fi)) == 2 })
	cancel1()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaver err = %v, want context canceled", err)
	}
	// Get decides on the cancel before it returns, and cancel is synchronous.
	if err := jctx.Load().(context.Context).Err(); err != nil {
		t.Fatalf("conversion canceled while a waiter remained: %v", err)
	}
	cancel2()
	<-second
	if jctx.Load().(context.Context).Err() == nil {
		t.Fatal("conversion was not canceled after its last waiter left")
	}
}

// A request arriving after every waiter left (while the canceled conversion is
// still being torn down) starts a fresh one instead of inheriting the cancel,
// and the dying run's cleanup does not delete the fresh run's partial file.
func TestFlacCacheLateJoinerAfterCancelStartsFresh(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	var dying *flacJob
	c := newTestCache(t, 1<<20, func(ctx context.Context, _ *os.File, _, out string) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			<-release // ffmpeg kill + reap latency
			return ctx.Err()
		}
		err := os.WriteFile(out, fakeFlac, 0o600)
		once.Do(func() { close(release) })
		<-dying.done // the dying run has finished its cleanup
		return err
	})
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	ctx, cancel := context.WithCancel(context.Background())
	first := getAsync(c, ctx, p, open, fi)
	waitFor(t, "the conversion to start", func() bool { return calls.Load() == 1 })
	c.mu.Lock()
	dying = c.inflight[flacKey(p, fi)]
	c.mu.Unlock()
	cancel()
	<-first
	second := getAsync(c, context.Background(), p, open, fi)
	// Release the dying run once the live request has joined it or started anew.
	waitFor(t, "the live request", func() bool { return c.waiters(flacKey(p, fi)) == 1 || calls.Load() == 2 })
	once.Do(func() { close(release) })
	if err := <-second; err != nil {
		t.Fatalf("live request after a cancel: %v", err)
	}
}

// A failure is returned as itself, leaves no partial file and is not cached.
func TestFlacCacheDoesNotCacheFailures(t *testing.T) {
	boom := errors.New("boom")
	conv := &countingConverter{err: boom}
	c := newTestCache(t, 1<<20, conv.convert)
	p, open, fi := srcNamed(t, t.TempDir(), "song.m4a")
	if _, err := c.Get(context.Background(), p, fi, open); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the conversion error", err)
	}
	if ents, _ := os.ReadDir(c.dir); len(ents) != 0 && (len(ents) != 1 || ents[0].Name() != flacLockName) {
		t.Fatalf("a failed conversion left %d files in the cache", len(ents))
	}
	conv.err = nil
	if err := <-getAsync(c, context.Background(), p, open, fi); err != nil || conv.calls.Load() != 2 {
		t.Fatalf("retry: err = %v, calls = %d, want 2 (a failure must not be cached)", err, conv.calls.Load())
	}
}

func TestFlacInputAndCappedStderr(t *testing.T) {
	in, _ := os.Open(os.DevNull)
	defer func() { _ = in.Close() }()
	if name, extra, _, err := flacInput(context.Background(), false, in, "/m/o.part"); err != nil || name != "/dev/fd/3" || len(extra) != 1 || extra[0] != in {
		t.Errorf("handle input = %q, %v, %v: want the handle as fd 3", name, extra, err)
	}
	b := &cappedBuffer{max: 16}
	for _, s := range []string{"HEAD-", strings.Repeat("x", 1000), "-TAIL"} {
		_, _ = b.Write([]byte(s))
	}
	if s := b.String(); len(s) > 64 || !strings.HasPrefix(s, "HEAD-") || !strings.HasSuffix(s, "-TAIL") {
		t.Errorf("capped stderr = %q, want head and tail within the cap", s)
	}
	for i := 0; i < 100; i++ { // small writes after the overflow: held bytes stay bounded
		_, _ = b.Write([]byte("abc"))
		if len(b.tail) > b.max {
			t.Fatalf("tail holds %d bytes, want at most %d", len(b.tail), b.max)
		}
	}
	if s, want := b.String(), "HEAD-xxx\n... [1294 bytes dropped] ...\nbcabcabc"; s != want {
		t.Errorf("capped stderr = %q, want %q", s, want)
	}
}

// flacMux builds a UI over the fixture DB with the fallback attached (fake
// converter) when conv is non-nil.
func flacMux(t *testing.T, f *previewFixture, conv *countingConverter) (*http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(f.db)))
	cacheDir := ""
	if conv != nil {
		ui.attachPreviewFlac(t.TempDir(), 1<<20, conv.convert)
		cacheDir = ui.flac.dir
		t.Cleanup(ui.ClosePreviewFlac)
	}
	ui.Register(mux)
	return mux, cacheDir
}

func flacGet(mux http.Handler, id string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio.flac", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPreviewFlacOffIs404(t *testing.T) {
	f := newPreviewFixture(t)
	mux, _ := flacMux(t, f, nil)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.m4a")), 10)
	rec := flacGet(mux, id)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with the key off", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), previewBytes) || bytes.Contains(rec.Body.Bytes(), fakeFlac) {
		t.Error("404 body carries file content")
	}
}

func TestPreviewFlacServesConversionWithRange(t *testing.T) {
	f := newPreviewFixture(t)
	conv := &countingConverter{}
	mux, cacheDir := flacMux(t, f, conv)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.m4a")), 10)

	rec := flacGet(mux, id)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), fakeFlac) {
		t.Fatalf("status = %d body = %q, want 200 with the conversion", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/flac" {
		t.Errorf("Content-Type = %q, want audio/flac", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	rec = flacGet(mux, id, "Range", "bytes=2-5")
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), fakeFlac[2:6]) {
		t.Fatalf("range: status = %d body = %q, want 206 %q", rec.Code, rec.Body.Bytes(), fakeFlac[2:6])
	}
	if conv.calls.Load() != 1 {
		t.Errorf("conversions = %d, want 1", conv.calls.Load())
	}
	// Nothing lands in the library: the root holds only the source file.
	ents, _ := os.ReadDir(f.root)
	if len(ents) != 1 {
		t.Errorf("library root has %d entries, want only the source", len(ents))
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache dir missing: %v", err)
	}
}

func TestPreviewFlacRefusesWhatTheAudioRouteRefuses(t *testing.T) {
	f := newPreviewFixture(t)
	conv := &countingConverter{}
	mux, _ := flacMux(t, f, conv)
	secret := f.writeFile(t, f.outside, "secret.m4a")
	link := filepath.Join(f.root, "link.m4a")
	symlinkOK := os.Symlink(secret, link) == nil
	cases := map[string]string{
		"missing file":       filepath.Join(f.root, "gone.m4a"),
		"outside every root": secret,
		"dotdot out of root": filepath.Join(f.root, "..", filepath.Base(f.outside), "secret.m4a"),
		"relative path":      "song.m4a",
		"directory":          f.root,
	}
	if symlinkOK {
		cases["symlink escaping root"] = link
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			rec := flacGet(mux, strconv.FormatInt(f.row(t, path), 10))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
	for _, bad := range []string{"0", "-3", "abc", "99999"} {
		if rec := flacGet(mux, bad); rec.Code != http.StatusNotFound {
			t.Errorf("id %q: status = %d, want 404", bad, rec.Code)
		}
	}
	if conv.calls.Load() != 0 {
		t.Errorf("converter ran %d times for refused paths, want 0", conv.calls.Load())
	}
}

func TestPreviewFlacRequiresSession(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	f := newPreviewFixture(t)
	conv := &countingConverter{}
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(f.db)))
	ui.attachPreviewFlac(t.TempDir(), 1<<20, conv.convert)
	ui.Register(mux)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.m4a")), 10)

	req := httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio.flac", nil)
	req.RemoteAddr = "198.51.100.31:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("no session: %d -> %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if conv.calls.Load() != 0 {
		t.Fatal("an unauthenticated request started a conversion")
	}

	req = httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio.flac", nil)
	req.RemoteAddr = "198.51.100.31:1"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), fakeFlac) {
		t.Fatalf("with session: status = %d, want 200 with the conversion", rec.Code)
	}
}

func TestPreviewFlacConversionFailureIs502(t *testing.T) {
	f := newPreviewFixture(t)
	mux, _ := flacMux(t, f, &countingConverter{err: errors.New("boom")})
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.m4a")), 10)
	rec := flacGet(mux, id)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("response leaks the converter error")
	}
}

func TestPreviewFlacSrc(t *testing.T) {
	u := NewUI(config.Config{}, "v")
	if got := u.previewFlacSrc(7); got != "" {
		t.Errorf("off: src = %q, want empty", got)
	}
	u.attachPreviewFlac(t.TempDir(), 1, (&countingConverter{}).convert)
	if got := u.previewFlacSrc(7); got != "/preview/7/audio.flac" {
		t.Errorf("on: src = %q", got)
	}
	u.ClosePreviewFlac()
	if _, err := os.Stat(u.flac.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ClosePreviewFlac left the cache dir: %v", err)
	}
}

func TestAttachPreviewFlacLeavesItOffWhenCacheUncreatable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := NewUI(config.Config{}, "v")
	u.attachPreviewFlac(blocker, 1, (&countingConverter{}).convert)
	if u.flac != nil {
		t.Error("an uncreatable cache must leave the fallback off")
	}
	u.ClosePreviewFlac() // a no-op when off
}

// deadlineRecorder records each write deadline the handler sets, and when.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	set, at []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.set, d.at = append(d.set, t), append(d.at, time.Now())
	return nil
}

// The deadline armed before the conversion is re-armed once it ends, for the
// stream and for the error write alike: a long queue must not cut either.
func TestPreviewFlacRearmsWriteDeadlineAfterConversion(t *testing.T) {
	for name, convErr := range map[string]error{"stream": nil, "error": errors.New("boom")} {
		t.Run(name, func(t *testing.T) {
			f := newPreviewFixture(t)
			var finished time.Time
			conv := &countingConverter{err: convErr}
			mux := http.NewServeMux()
			ui := NewUI(config.Config{}, "v", WithReports(reports.New(f.db)))
			ui.attachPreviewFlac(t.TempDir(), 1<<20, func(ctx context.Context, in *os.File, n, out string) error {
				defer func() { finished = time.Now() }()
				return conv.convert(ctx, in, n, out)
			})
			t.Cleanup(ui.ClosePreviewFlac)
			ui.Register(mux)
			id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.m4a")), 10)
			rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio.flac", nil))
			if n := len(rec.set); n == 0 || rec.at[n-1].Before(finished) {
				t.Fatalf("status %d: no write deadline set after the conversion ended (%d set)", rec.Code, n)
			}
			if got := rec.set[len(rec.set)-1].Sub(finished); got < previewWriteBound || got > previewWriteBound+time.Minute {
				t.Fatalf("re-armed deadline = conversion end+%v, want +%v", got, previewWriteBound)
			}
		})
	}
}

// fakeFFmpeg writes a stand-in ffmpeg (POSIX sh) that records the -i argument
// in argLog and copies the bytes it reads there to the output, the last arg.
func fakeFFmpeg(t *testing.T) (bin, argLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ffmpeg is a POSIX shell script")
	}
	dir := t.TempDir()
	bin, argLog = filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "args")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = -i ]; then shift; in=\"$1\"; fi\n  last=\"$1\"; shift\ndone\n" +
		"printf '%s' \"$in\" > '" + argLog + "'\ncat \"${in#file:}\" > \"$last\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // reason: G306 -- an executable test stand-in in a private temp dir
		t.Fatal(err)
	}
	return bin, argLog
}

// Where handles cannot be passed (Windows), ffmpeg reads a private snapshot of
// the already-confined handle, made in the cache dir: never the library path,
// so a file swapped in under that path after the open is not what converts.
// The snapshot is gone once the conversion returns.
func TestFlacSnapshotInputReadsTheHandleNotThePath(t *testing.T) {
	bin, argLog := fakeFFmpeg(t)
	prev := flacSnapshotInput
	flacSnapshotInput = true
	t.Cleanup(func() { flacSnapshotInput = prev })

	src := filepath.Join(t.TempDir(), "song.m4a")
	if err := os.WriteFile(src, []byte("confined-original"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	// Swap the path after the confined open, as an attacker racing the check would.
	swapped := filepath.Join(filepath.Dir(src), "evil")
	if err := os.WriteFile(swapped, []byte("outside-the-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped, src); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	out := filepath.Join(cacheDir, "k.flac.1.part")
	if err := ffmpegFlacConverter(bin)(context.Background(), in, src, out); err != nil {
		t.Fatalf("convert: %v", err)
	}
	arg, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	read := strings.TrimPrefix(string(arg), "file:")
	if filepath.Dir(read) != cacheDir || read == src {
		t.Fatalf("ffmpeg read %q, want a snapshot inside the cache dir %q, never the library path", arg, cacheDir)
	}
	if got, _ := os.ReadFile(out); string(got) != "confined-original" {
		t.Fatalf("ffmpeg read %q, want the confined handle's bytes", got)
	}
	if _, err := os.Stat(read); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot %q survived the conversion: %v", read, err)
	}
	if ents, _ := os.ReadDir(cacheDir); len(ents) != 1 {
		t.Fatalf("cache dir holds %d files after the conversion, want only the output", len(ents))
	}
}

// A conversion that fails still removes its snapshot.
func TestFlacSnapshotRemovedOnFailure(t *testing.T) {
	prev := flacSnapshotInput
	flacSnapshotInput = true
	t.Cleanup(func() { flacSnapshotInput = prev })
	src := filepath.Join(t.TempDir(), "song.m4a")
	if err := os.WriteFile(src, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	cacheDir := t.TempDir()
	err = ffmpegFlacConverter(filepath.Join(t.TempDir(), "no-such-ffmpeg"))(context.Background(), in, src, filepath.Join(cacheDir, "k.part"))
	if err == nil {
		t.Fatal("conversion with a missing ffmpeg succeeded")
	}
	if ents, _ := os.ReadDir(cacheDir); len(ents) != 0 {
		t.Fatalf("a failed conversion left %d files (the snapshot) in the cache dir", len(ents))
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

// tone is a 2.3 s 440 Hz lavfi input (an awkward, non-round sample count).
var tone = []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2.3"}

// makeFixture encodes the tone with codec into dir/name.
func makeFixture(t *testing.T, bin, dir, name string, codecArgs ...string) string {
	t.Helper()
	return makeFixtureFrom(t, bin, dir, name, append(append([]string{}, tone...), codecArgs...)...)
}

func makeFixtureFrom(t *testing.T, bin, dir, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args = append(append([]string{"-nostdin", "-loglevel", "error"}, args...), out)
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
		// A second, shorter audio stream marked default (so ffmpeg's own pick)
		// plus a title tag: only the first audio stream and no tags may survive.
		"two-stream": makeFixtureFrom(t, bin, dir, "two.m4a", append(append([]string{}, tone...),
			"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000:duration=1.1", "-map", "0", "-map", "1",
			"-ac:a:1", "2", "-disposition:a:0", "0", "-disposition:a:1", "default", "-metadata", "title=T", "-c:a", "alac")...),
	}
	conv := ffmpegFlacConverter(bin)
	prev := flacSnapshotInput
	t.Cleanup(func() { flacSnapshotInput = prev })
	for _, snapshot := range []bool{false, true} {
		flacSnapshotInput = snapshot
		for name, src := range fixtures {
			name += map[bool]string{false: "/handle", true: "/snapshot"}[snapshot]
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
					t.Fatalf("decoded samples: flac %d, source stream 0 %d (bytes of s16 mono)", len(got)/2, len(want)/2)
				}
				meta, _ := exec.Command(bin, "-nostdin", "-loglevel", "error", "-i", out, "-f", "ffmetadata", "-").Output() //nolint:gosec // reason: G204 -- fixed argv in a test
				if strings.Contains(strings.ToLower(string(meta)), "title=") {
					t.Errorf("source tags carried into the conversion: %q", meta)
				}
			})
		}
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

func TestPreviewFlacEndToEndALACSeeks(t *testing.T) {
	bin := requireFFmpeg(t)
	f := newPreviewFixture(t)
	src := makeFixture(t, bin, f.root, "t.m4a", "-c:a", "alac")
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v", WithReports(reports.New(f.db)))
	ui.attachPreviewFlac(t.TempDir(), 64<<20, ffmpegFlacConverter(bin))
	ui.Register(mux)
	id := strconv.FormatInt(f.row(t, src), 10)

	rec := flacGet(mux, id)
	if rec.Code != http.StatusOK || !bytes.HasPrefix(rec.Body.Bytes(), []byte("fLaC")) {
		t.Fatalf("status = %d, want 200 with a fLaC stream", rec.Code)
	}
	total := rec.Body.Len()
	rec = flacGet(mux, id, "Range", "bytes="+strconv.Itoa(total/2)+"-")
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != total-total/2 {
		t.Fatalf("seek: status = %d len = %d, want 206 with the tail", rec.Code, rec.Body.Len())
	}
	if ents, _ := os.ReadDir(f.root); len(ents) != 1 {
		t.Errorf("library root has %d entries after conversion, want 1", len(ents))
	}
}
