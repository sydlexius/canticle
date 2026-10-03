package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
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
	"github.com/sydlexius/canticle/internal/reports"
)

// The preview FLAC fallback (#1243, opt-in via server.preview_flac_fallback).
// When the browser cannot decode a track, the player retries ONCE against
// /preview/{id}/audio.flac, which serves a whole-file FLAC conversion made with
// ffmpeg. FLAC because every browser plays it and it is lossless, so no encoder
// delay shifts the timing the offset editor corrects. Conversions live in a
// size-capped temp directory, never in a library.

const (
	// flacCacheMaxBytes caps the conversion cache; the oldest entries are
	// evicted once the total passes it.
	flacCacheMaxBytes int64 = 512 << 20
	// flacConvertTimeout bounds one ffmpeg run, from when it gets a slot.
	flacConvertTimeout = 10 * time.Minute
	// flacMaxConversions bounds concurrent ffmpeg runs: the CPU they take and
	// the in-flight .part bytes, which the cap counts but cannot evict.
	flacMaxConversions = 2
	// flacDirPrefix names every cache dir, so a new cache can find and
	// remove its dead siblings'.
	flacDirPrefix = "canticle-preview-flac-"
	// flacLockName is the liveness lock in a cache dir: its owner holds an
	// exclusive flock on it for the cache's lifetime, and the kernel drops
	// the lock when the process dies, however it dies.
	flacLockName = ".lock"
	// flacLocklessStale is how old a cache dir with no .lock must be before a
	// sweep reads it as dead: a creating sibling renames its lock into place
	// within moments, so an hour-old lockless dir was left by a crash or a
	// failed Close.
	flacLocklessStale = time.Hour
	// flacStderrMax bounds the ffmpeg stderr held in memory (a corrupt file can
	// print one line per bad frame, #731).
	flacStderrMax = 64 << 10
)

// flacConverter writes a FLAC conversion of in to outPath. inName is in's path,
// for tests and messages only: the conversion reads the handle, never the name.
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
	lock    *os.File // the dir's liveness lock; nil where there is none
	runs    sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	mu       sync.Mutex
	closed   bool
	inflight map[string]*flacJob
}

var errFlacCacheClosed = errors.New("preview flac cache is closed")

// flacHitOpened runs between a cache hit's open and its closed check; a test
// seam for the race with Close.
var flacHitOpened = func() {}

// newFlacCache creates the cache in a fresh private directory under parent
// (os.MkdirTemp: 0700 and a random name, so two processes sharing parent never
// touch each other's files and nothing pre-planted is followed), holds that
// dir's liveness lock until Close, and removes every sibling cache dir whose
// lock is free: one a process left when it died without Close (a crash, a
// kill, a container stop). A live sibling holds its lock, so it is never
// touched. Windows has no lock and no sweep (flaclock_other.go).
func newFlacCache(parent string, max int64, convert flacConverter) (*flacCache, error) {
	dir, err := os.MkdirTemp(parent, flacDirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create preview flac cache: %w", err)
	}
	lock, err := flacLockDir(dir)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("lock preview flac cache: %w", err), os.RemoveAll(dir))
	}
	sweepFlacDirs(parent, dir)
	return &flacCache{dir: dir, max: max, convert: convert, lock: lock,
		sem: make(chan struct{}, flacMaxConversions), inflight: map[string]*flacJob{}}, nil
}

// flacLockDir takes a new cache dir's liveness lock; a test seam.
var flacLockDir = lockFlacDir

// sweepFlacDirs removes the dead cache dirs under parent, never own. Only real
// directories are probed, never a symlink planted under the prefix.
func sweepFlacDirs(parent, own string) {
	ents, err := os.ReadDir(parent)
	if err != nil {
		slog.Warn("preview flac cache: cannot list for stale dirs", "error", err)
		return
	}
	for _, e := range ents {
		p := filepath.Join(parent, e.Name())
		if p == own {
			// Never probe our own dir. Native flock conflicts across two open
			// files even in one process, but NFS emulates flock with per-process
			// POSIX locks and some FUSE filesystems make it a no-op, so there the
			// probe would succeed and remove the dir this cache just created.
			continue
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), flacDirPrefix) || !flacDirDead(p) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			slog.Warn("preview flac cache: cannot remove a stale dir", "error", err)
		}
	}
}

// Close cancels every conversion, waits for each to finish (an ffmpeg kill
// and reap), then removes the cache directory and releases its lock. Get fails
// once Close has begun. Calling Close again returns the first call's result.
func (c *flacCache) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		for _, job := range c.inflight {
			job.cancel()
		}
		c.mu.Unlock()
		c.runs.Wait()
		c.closeErr = os.RemoveAll(c.dir)
		if c.lock != nil {
			c.closeErr = errors.Join(c.closeErr, c.lock.Close())
		}
	})
	return c.closeErr
}

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
		flacHitOpened()
		// The closed check under c.mu is the hit's linearization point: a Close
		// that began before it gets no handle back, one that begins after it
		// finds the handle already served.
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			_ = f.Close()
			return nil, errFlacCacheClosed
		}
		now := time.Now()
		_ = os.Chtimes(out, now, now) // recency for eviction; best effort
		return f, nil
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errFlacCacheClosed
	}
	job, ok := c.inflight[key]
	if !ok {
		jctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		job = &flacJob{done: make(chan struct{}), cancel: cancel}
		c.inflight[key] = job
		c.runs.Add(1) // under c.mu with closed false, so never after Close's Wait began
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
		// Only this branch unmaps a job, and a job is joined only while mapped,
		// so 0 is reached once, with job still under key: never a newer job.
		job.cancel()            // no-op once the job has finished
		delete(c.inflight, key) // unpins the result; a later request starts afresh
	}
	c.mu.Unlock()
	return f, err
}

// run performs one conversion into its own unique temp name then renames it
// into place, so a reader never sees a partial file and a canceled run's
// cleanup never touches a newer run's file.
func (c *flacCache) run(ctx context.Context, key string, job *flacJob, open func() (*os.File, error), srcPath, out string) {
	defer c.runs.Done()
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
	// The timeout starts with the slot, so time queued behind other
	// conversions never cuts this one short.
	ctx, cancel := context.WithTimeout(ctx, flacConvertTimeout)
	defer cancel()
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
// .part files (and any source snapshot, see flacInput) fit the cap. An entry whose job still has waiters to open it is
// pinned, so a result is never deleted before it is served; a single file
// larger than the cap is therefore still served and goes with a later eviction.
//
// Windows (not exercised by any test here): removing, or renaming a fresh
// conversion onto, a file another request is still serving fails unless the
// reader opened it with share-delete. Eviction logs that and moves on; the
// rename fails that one request.
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
	return func(ctx context.Context, in *os.File, _, outPath string) error {
		input, extra, cleanup, err := flacInput(ctx, flacSnapshotInput, in, outPath)
		if err != nil {
			return err
		}
		defer cleanup()
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

// flacSnapshotInput selects the snapshot input (see flacInput); a test seam, so
// the Windows path runs on every platform's tests.
var flacSnapshotInput = runtime.GOOS == "windows"

// flacInput picks how ffmpeg reads the source, always through the handle in,
// which was opened and confined under the library root; the library path is
// never re-opened, so a path swapped after that check cannot redirect the read.
//
// Where the platform has /dev/fd the handle is passed as fd 3 (a seekable
// regular file, so an m4a with its index at the end works). Windows cannot
// pass extra handles (os/exec fails every start that sets ExtraFiles there),
// so with snapshot set the handle's bytes are first copied into a private
// file beside outPath, in the cache dir, and ffmpeg reads that copy: still
// seekable, which stdin would not be. The copy counts toward the cache cap
// like a partial conversion while it exists, and cleanup removes it. The file:
// prefix keeps a name with a colon from being read as a protocol.
func flacInput(ctx context.Context, snapshot bool, in *os.File, outPath string) (input string, extra []*os.File, cleanup func(), err error) {
	if !snapshot {
		return "/dev/fd/3", []*os.File{in}, func() {}, nil
	}
	snap, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".src-*")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create source snapshot: %w", err)
	}
	remove := func() { _ = os.Remove(snap.Name()) }
	_, err = io.Copy(snap, ctxReader{ctx, io.NewSectionReader(in, 0, 1<<62)})
	if cerr := snap.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		remove()
		return "", nil, nil, fmt.Errorf("snapshot source: %w", err)
	}
	return "file:" + snap.Name(), nil, remove, nil
}

// ctxReader stops a copy once ctx is done, so a cancel or Close is not held up
// by a long snapshot.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// cappedBuffer keeps the first and the last max/2 bytes of output, which carry
// the failing stream and the cause that ended the run. The tail is trimmed only
// once it doubles, so a run of small writes copies it rarely (it holds up to
// 1.5*max between trims).
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
	if over := len(b.tail) - b.max/2; len(b.tail) > b.max && over > 0 {
		b.dropped += over
		b.tail = append(b.tail[:0], b.tail[over:]...)
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	tail, dropped := b.tail, b.dropped
	if over := len(tail) - b.max/2; over > 0 {
		tail, dropped = tail[over:], dropped+over
	}
	if dropped == 0 {
		return string(b.head) + string(tail)
	}
	return fmt.Sprintf("%s\n... [%d bytes dropped] ...\n%s", b.head, dropped, tail)
}

// AttachPreviewFlacFallback enables the FLAC fallback with the ffmpeg at bin
// (server.preview_flac_fallback), caching in a private directory under the OS
// temp dir that ClosePreviewFlac removes. A cache that cannot be created
// leaves it off, logged: the player then shows its ordinary error.
func (u *UI) AttachPreviewFlacFallback(bin string) {
	u.attachPreviewFlac(os.TempDir(), flacCacheMaxBytes, ffmpegFlacConverter(bin))
}

// ClosePreviewFlac cancels the FLAC fallback's conversions, waits for them to
// stop, and removes its cache directory, if any. Call it once the HTTP server
// has stopped serving; a request still waiting on a conversion then fails.
func (u *UI) ClosePreviewFlac() {
	if u.flac == nil {
		return
	}
	if err := u.flac.Close(); err != nil {
		slog.Warn("preview flac cache: cannot remove on shutdown", "error", err)
	}
}

func (u *UI) attachPreviewFlac(parent string, max int64, convert flacConverter) {
	c, err := newFlacCache(parent, max, convert)
	if err != nil {
		slog.Error("preview flac fallback unavailable", "error", err)
		return
	}
	u.flac = c
}

// previewFlacSrc is the fallback URL the player retries against, or "" when the
// fallback is off, so the page learns of it from the server and never probes.
func (u *UI) previewFlacSrc(id int64) string {
	if u.flac == nil {
		return ""
	}
	return "/preview/" + strconv.FormatInt(id, 10) + "/audio.flac"
}

// handlePreviewFlac serves the FLAC conversion of one row's audio, with Range
// support. It resolves and confines the file exactly as handlePreviewAudio does
// (the same session guard, DB lookup and os.Root open) and answers 404 for every
// refusal and whenever the fallback is not enabled.
func (u *UI) handlePreviewFlac(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if u.flac == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve preview flac", "id", id)
		http.Error(w, "preview data source unavailable", http.StatusServiceUnavailable)
		return
	}
	audioPath, err := u.reports.PreviewAudioPath(r.Context(), id)
	if errors.Is(err, reports.ErrPreviewNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("preview source lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	roots, err := u.reports.LibraryRoots(r.Context())
	if err != nil {
		slog.Error("preview library roots lookup failed", "id", id, "error", err)
		http.Error(w, "preview lookup failed", http.StatusInternalServerError)
		return
	}
	f, fi, ok := openPreviewAudio(roots, audioPath)
	if !ok {
		slog.Warn("preview flac refused: not a regular file under a library root", "id", id)
		http.NotFound(w, r)
		return
	}
	_ = f.Close() // the stat keys the cache; a conversion re-opens through the same confinement
	open := func() (*os.File, error) {
		g, gfi, ok := openPreviewAudio(roots, audioPath)
		if !ok {
			return nil, errors.New("not a regular file under a library root")
		}
		// The conversion is cached under fi's key, so it must convert the
		// file fi described: a file replaced in between (another inode, or
		// rewritten in place) fails this conversion instead of being cached
		// under the old key. The next request keys the new file afresh.
		if !sameFlacSource(fi, gfi) {
			_ = g.Close()
			return nil, errors.New("source changed since the request opened it")
		}
		return g, nil
	}
	// A conversion can outlast the server-wide write timeout, and so can the
	// stream that follows. The conversion's own timeout starts only once it
	// gets a slot, so the deadline is re-armed after Get, before any write:
	// the stream's, or the error's after a long queue.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(flacConvertTimeout + previewWriteBound)); err != nil {
		slog.Error("preview flac: cannot extend the write deadline; long responses will be cut", "id", id, "error", err)
	}
	out, err := u.flac.Get(r.Context(), filepath.Clean(audioPath), fi, open)
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(previewWriteBound)); err != nil {
		slog.Error("preview flac: cannot extend the write deadline; long responses will be cut", "id", id, "error", err)
	}
	if err != nil {
		if r.Context().Err() == nil {
			slog.Error("preview flac conversion failed", "id", id, "error", err)
			http.Error(w, "audio conversion failed", http.StatusBadGateway)
		}
		return
	}
	defer func() { _ = out.Close() }()
	ofi, err := out.Stat()
	if err != nil {
		slog.Error("preview flac: stat of converted file failed", "id", id, "error", err)
		http.Error(w, "audio conversion failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	http.ServeContent(w, r, "", ofi.ModTime(), out)
}

// sameFlacSource reports whether b is the same file as a, unchanged: the same
// file identity and the same mtime and size, the fields the cache key holds.
func sameFlacSource(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}
