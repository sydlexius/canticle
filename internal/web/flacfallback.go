package web

import (
	"bytes"
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

// flacConvertTimeout bounds one ffmpeg run.
const flacConvertTimeout = 10 * time.Minute

// flacConverter writes a FLAC conversion of in to outPath. inName is in's path
// (used only where the platform cannot read the open handle).
type flacConverter func(ctx context.Context, in *os.File, inName, outPath string) error

// flacJob is one in-flight conversion shared by every request for its key.
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

	mu       sync.Mutex
	inflight map[string]*flacJob
}

// newFlacCache clears dir (a previous run's leftovers) and recreates it 0700.
func newFlacCache(dir string, max int64, convert flacConverter) (*flacCache, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clear preview flac cache: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create preview flac cache: %w", err)
	}
	return &flacCache{dir: dir, max: max, convert: convert, inflight: map[string]*flacJob{}}, nil
}

// flacKey names a conversion by source path, mtime and size, so a changed file
// is converted afresh and an unchanged one is not.
func flacKey(path string, fi fs.FileInfo) string {
	sum := sha256.Sum256([]byte(path + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "\x00" + strconv.FormatInt(fi.Size(), 10)))
	return hex.EncodeToString(sum[:]) + ".flac"
}

// Get returns the converted file opened for reading, converting src first when
// no cached copy exists. Concurrent callers for one key share a single
// conversion; it is canceled when every waiter has gone.
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

	select {
	case <-job.done:
	case <-ctx.Done():
		c.mu.Lock()
		job.waiters--
		if job.waiters == 0 {
			job.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
	if job.err != nil {
		return nil, job.err
	}
	return os.Open(out) //nolint:gosec // reason: G304 -- out is dir joined with a sha256 hex name this package computed; no caller input reaches the path
}

// run performs one conversion into a temp name then renames it into place, so
// a reader never sees a partial file.
func (c *flacCache) run(ctx context.Context, key string, job *flacJob, open func() (*os.File, error), srcPath, out string) {
	defer job.cancel()
	tmp := out + ".part"
	var err error
	if src, oerr := open(); oerr != nil {
		err = fmt.Errorf("open source: %w", oerr)
	} else {
		err = c.convert(ctx, src, srcPath, tmp)
		_ = src.Close()
	}
	if err == nil {
		err = os.Rename(tmp, out)
	}
	if err != nil {
		_ = os.Remove(tmp)
		job.err = err
	} else {
		c.evict(out)
	}
	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	close(job.done)
}

// evict removes the oldest cached conversions until the total is within the
// cap. keep (the entry just made) is never removed, so a single file larger
// than the cap is still served; it goes with the next eviction.
func (c *flacCache) evict(keep string) {
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
		if !strings.HasSuffix(e.Name(), ".flac") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(c.dir, e.Name())
		all = append(all, entry{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, e := range all {
		if total <= c.max {
			return
		}
		if e.path == keep {
			continue
		}
		if err := os.Remove(e.path); err != nil {
			slog.Warn("preview flac cache: eviction failed", "error", err)
			continue
		}
		total -= e.size
	}
}

// ffmpegFlacConverter returns a converter that runs the ffmpeg at bin with an
// argument list (no shell). Where the platform has /dev/fd the already-open,
// already-confined handle is passed as fd 3 (a seekable regular file, so an
// m4a with its index at the end works) and the library path is never re-opened.
func ffmpegFlacConverter(bin string) flacConverter {
	return func(ctx context.Context, in *os.File, inName, outPath string) error {
		input := "/dev/fd/3"
		if runtime.GOOS == "windows" {
			input = inName
		}
		cmd := exec.CommandContext(ctx, bin, //nolint:gosec // reason: G204 -- bin comes from ffmpeg.Resolve and the argv is fixed; no value passes through a shell
			"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-i", input, "-map", "0:a:0", "-vn", "-c:a", "flac", "-f", "flac", outPath)
		cmd.ExtraFiles = []*os.File{in}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg flac conversion: %w: %s", err, ffmpeg.BoundOutput(stderr.String()))
		}
		return nil
	}
}
