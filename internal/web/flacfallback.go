package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/ffmpeg"
)

// The preview FLAC fallback's conversion cache (#1243), consumed by the
// /preview/{id}/audio.flac endpoint, #1243. Conversions live in a size-capped
// temp directory, never in a library; FLAC because every browser plays it and
// it is lossless, so no encoder delay shifts the timing the offset editor
// corrects.

const (
	// flacConvertTimeout bounds one ffmpeg run.
	flacConvertTimeout = 10 * time.Minute
	// flacMaxConversions bounds concurrent ffmpeg runs: the CPU they take and
	// the in-flight .part bytes, which the cap counts but cannot evict.
	flacMaxConversions = 2
	// flacStderrMax bounds the ffmpeg stderr held in memory (a corrupt file can
	// print one line per bad frame, #731).
	flacStderrMax = 64 << 10
)

// flacConverter writes a FLAC conversion of in to outPath. inName is in's path
// (used only where the platform cannot read the open handle).
type flacConverter func(ctx context.Context, in *os.File, inName, outPath string) error

// flacJob is one conversion shared by every request for its key. It stays in
// inflight, which pins its result against eviction, until every waiter has
// opened the result or left.
type flacJob struct {
	done    chan struct{}
	err     error
	waiters int
	cancel  context.CancelFunc
}

// flacCache converts files to FLAC into dir, at most once per (path, mtime,
// size) while the result stays cached.
type flacCache struct {
	dir     string
	max     int64
	convert flacConverter
	sem     chan struct{}

	mu       sync.Mutex
	inflight map[string]*flacJob
}

// newFlacCache creates the cache in a fresh private directory under parent
// (os.MkdirTemp: 0700 and a random name, so two processes sharing parent never
// touch each other's files and nothing pre-planted is followed). Close removes
// it; a process that dies without closing leaves it to the OS temp cleanup.
func newFlacCache(parent string, max int64, convert flacConverter) (*flacCache, error) {
	dir, err := os.MkdirTemp(parent, "canticle-preview-flac-*")
	if err != nil {
		return nil, fmt.Errorf("create preview flac cache: %w", err)
	}
	return &flacCache{dir: dir, max: max, convert: convert,
		sem: make(chan struct{}, flacMaxConversions), inflight: map[string]*flacJob{}}, nil
}

// Close removes the cache directory and every conversion in it.
func (c *flacCache) Close() error { return os.RemoveAll(c.dir) }

// flacKey names a conversion by source path, mtime and size, so a changed file
// is converted afresh and an unchanged one is not.
func flacKey(path string, fi fs.FileInfo) string {
	sum := sha256.Sum256([]byte(path + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "\x00" + strconv.FormatInt(fi.Size(), 10)))
	return hex.EncodeToString(sum[:]) + ".flac"
}

// Get returns the converted file opened for reading, converting src first when
// no cached copy exists. Concurrent callers for one key share a single
// conversion; it is canceled when every waiter has gone, and a later request
// then starts a fresh one rather than joining the canceled job.
//
// open yields a fresh confined handle on the source for the conversion, which
// owns and closes it, so it never depends on a requester's handle staying open.
func (c *flacCache) Get(ctx context.Context, srcPath string, fi fs.FileInfo, open func() (*os.File, error)) (*os.File, error) {
	key := flacKey(srcPath, fi)
	out := filepath.Join(c.dir, key)
	if f, err := os.Open(out); err == nil { //nolint:gosec // reason: G304 -- out is dir joined with a sha256 hex name this package computed; no caller input reaches the path
		now := time.Now()
		_ = os.Chtimes(out, now, now) // recency for eviction; best effort
		return f, nil
	}

	c.mu.Lock()
	job, ok := c.inflight[key]
	if !ok {
		jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flacConvertTimeout)
		job = &flacJob{done: make(chan struct{}), cancel: cancel}
		c.inflight[key] = job
		go c.run(jctx, key, job, open, srcPath, out)
	}
	job.waiters++
	c.mu.Unlock()

	var f *os.File
	var err error
	select {
	case <-job.done:
		if err = job.err; err == nil {
			f, err = os.Open(out) //nolint:gosec // reason: G304 -- out is dir joined with a sha256 hex name this package computed; no caller input reaches the path
		}
	case <-ctx.Done():
		err = ctx.Err()
	}
	c.mu.Lock()
	job.waiters--
	if job.waiters == 0 {
		job.cancel() // no-op once the job has finished
		if c.inflight[key] == job {
			delete(c.inflight, key) // unpins the result; a later request starts afresh
		}
	}
	c.mu.Unlock()
	return f, err
}

// run performs one conversion into its own unique temp name then renames it
// into place, so a reader never sees a partial file and a canceled run's
// cleanup never touches a newer run's file.
func (c *flacCache) run(ctx context.Context, key string, job *flacJob, open func() (*os.File, error), srcPath, out string) {
	defer close(job.done)
	defer job.cancel()
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		job.err = ctx.Err()
		return
	}
	defer func() { <-c.sem }()
	if job.err = ctx.Err(); job.err != nil {
		return
	}
	src, err := open()
	if err != nil {
		job.err = fmt.Errorf("open source: %w", err)
		return
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(c.dir, key+".*.part")
	if err != nil {
		job.err = fmt.Errorf("create conversion temp file: %w", err)
		return
	}
	_ = tmp.Close()
	if err = c.convert(ctx, src, srcPath, tmp.Name()); err == nil {
		err = os.Rename(tmp.Name(), out)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		job.err = err
		return
	}
	c.evict()
}

// evict removes the oldest cached conversions until they and the in-flight
// .part files fit the cap. An entry whose job still has waiters to open it is
// pinned, so a result is never deleted before it is served; a single file
// larger than the cap is therefore still served and goes with a later eviction.
func (c *flacCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		slog.Warn("preview flac cache: cannot list for eviction", "error", err)
		return
	}
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var all []entry
	var total int64
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
		if _, pinned := c.inflight[e.Name()]; pinned || !strings.HasSuffix(e.Name(), ".flac") {
			continue
		}
		all = append(all, entry{filepath.Join(c.dir, e.Name()), fi.Size(), fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, e := range all {
		if total <= c.max {
			return
		}
		if err := os.Remove(e.path); err != nil {
			slog.Warn("preview flac cache: eviction failed", "error", err)
			continue
		}
		total -= e.size
	}
}

// ffmpegFlacConverter returns a converter that runs the ffmpeg at bin with an
// argument list (no shell), keeping only the first audio stream and no tags.
func ffmpegFlacConverter(bin string) flacConverter {
	return func(ctx context.Context, in *os.File, inName, outPath string) error {
		input, extra := flacInput(runtime.GOOS, in, inName)
		cmd := exec.CommandContext(ctx, bin, //nolint:gosec // reason: G204 -- bin comes from ffmpeg.Resolve and the argv is fixed; no value passes through a shell
			"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", input,
			"-map", "0:a:0", "-map_metadata", "-1", "-vn", "-c:a", "flac", "-f", "flac", outPath)
		cmd.ExtraFiles = extra
		stderr := &cappedBuffer{max: flacStderrMax}
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg flac conversion: %w: %s", err, ffmpeg.BoundOutput(stderr.String()))
		}
		return nil
	}
}

// flacInput picks how ffmpeg reads the source. Where the platform has /dev/fd
// the already-open, already-confined handle is passed as fd 3 (a seekable
// regular file, so an m4a with its index at the end works) and the library
// path is never re-opened. Windows cannot pass extra handles (os/exec fails
// every start that sets ExtraFiles there), so ffmpeg opens inName itself: that
// re-open is NOT confined, so a path swapped after the handle was opened is
// not caught on Windows. The file: prefix keeps a name with a colon from being
// read as a protocol.
func flacInput(goos string, in *os.File, inName string) (string, []*os.File) {
	if goos == "windows" {
		return "file:" + inName, nil
	}
	return "/dev/fd/3", []*os.File{in}
}

// cappedBuffer holds at most max bytes of output: the first and the last
// max/2, which carry the failing stream and the cause that ended the run.
type cappedBuffer struct {
	head, tail []byte
	max        int
	dropped    int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	k := min(len(p), max(b.max/2-len(b.head), 0))
	b.head, p = append(b.head, p[:k]...), p[k:]
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - b.max/2; over > 0 {
		b.dropped += over
		b.tail = append(b.tail[:0:0], b.tail[over:]...)
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	if b.dropped == 0 {
		return string(b.head) + string(b.tail)
	}
	return fmt.Sprintf("%s\n... [%d bytes dropped] ...\n%s", b.head, b.dropped, b.tail)
}
