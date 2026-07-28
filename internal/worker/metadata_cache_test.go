package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/scanner"
)

// fakeMetaCache records what it was asked for and returns a canned answer.
type fakeMetaCache struct {
	facts scanner.AudioFacts
	found bool
	err   error

	gotPath  string
	gotMtime int64
	gotSize  int64
	calls    int
}

func (f *fakeMetaCache) Facts(_ context.Context, path string, mtimeNano, size int64) (scanner.AudioFacts, bool, error) {
	f.calls++
	f.gotPath, f.gotMtime, f.gotSize = path, mtimeNano, size
	return f.facts, f.found, f.err
}

// newCacheTestWorker builds a Worker wired for the metadata-cache path with a
// file reader that FAILS THE TEST if it is ever called. That is the whole point
// of #712: a cache hit must not open the audio file, and the only way to prove
// it is to make the open observable rather than to reason about it.
func newCacheTestWorker(t *testing.T, cache MetadataCache, mtimeNano, size int64) (*Worker, *int) {
	t.Helper()
	fileReads := 0
	w := &Worker{
		enrichRecordingDefault: true,
		metaCache:              cache,
		statFile: func(string) (int64, int64, error) {
			return mtimeNano, size, nil
		},
		readMetadata: func(string) (scanner.AudioMetadata, error) {
			fileReads++
			return scanner.AudioMetadata{TrackLength: 999, ISRC: "FROM-FILE", AlbumName: "FromFile"}, nil
		},
	}
	return w, &fileReads
}

// THE #712 ACCEPTANCE CRITERION. A current cached row must serve the fetch-time
// read WITHOUT the audio file being opened. On the reference library 96.8% of
// deferred rows have such a row, so this is the path nearly every queued item
// takes; if it silently fell through to the reader the change would be a no-op
// that still looked correct.
func TestResolveMetadataCacheHitDoesNotReadFile(t *testing.T) {
	cache := &fakeMetaCache{
		found: true,
		facts: scanner.AudioFacts{TrackLength: 210, ISRC: "USRC17607839", Album: "Cached Album"},
	}
	w, fileReads := newCacheTestWorker(t, cache, 100, 200)

	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}

	if *fileReads != 0 {
		t.Errorf("audio file was opened %d time(s) on a cache HIT; #712 exists to prevent exactly this", *fileReads)
	}
	if meta.TrackLength != 210 || meta.ISRC != "USRC17607839" || meta.AlbumName != "Cached Album" {
		t.Errorf("got %+v, want the CACHED values", meta)
	}
	// The identity must be echoed back: recordDuration banks against it, so a
	// zero stamp would silently disable the duration cache write on every hit.
	if meta.MTimeNano != 100 || meta.SizeBytes != 200 {
		t.Errorf("identity = (%d, %d), want (100, 200): recordDuration banks against this stamp",
			meta.MTimeNano, meta.SizeBytes)
	}
	// And the cache must be asked about the file as it is NOW, not some other key.
	if cache.gotPath != "/music/song.mp3" || cache.gotMtime != 100 || cache.gotSize != 200 {
		t.Errorf("cache queried with (%q, %d, %d), want the current stat identity",
			cache.gotPath, cache.gotMtime, cache.gotSize)
	}
}

// A miss must fall back to the file reader and return ITS values. This is the
// coverage-gap path: rows enqueued before the index existed, files the scanner
// skipped, and -- after a reader-identity bump (#713) -- the entire legacy
// population until it is re-indexed.
func TestResolveMetadataCacheMissReadsFile(t *testing.T) {
	w, fileReads := newCacheTestWorker(t, &fakeMetaCache{found: false}, 100, 200)

	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if *fileReads != 1 {
		t.Errorf("file read %d times on a cache MISS, want exactly 1", *fileReads)
	}
	if meta.ISRC != "FROM-FILE" {
		t.Errorf("got ISRC %q, want the FILE's value on a miss", meta.ISRC)
	}
}

// A cache ERROR must degrade to the file reader, never fail the item. The cache
// is an optimization over a read that must still happen correctly.
func TestResolveMetadataCacheErrorReadsFile(t *testing.T) {
	w, fileReads := newCacheTestWorker(t, &fakeMetaCache{err: errors.New("database is locked")}, 100, 200)

	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("a cache error must not surface as an error: %v", err)
	}
	if *fileReads != 1 {
		t.Errorf("file read %d times after a cache error, want exactly 1", *fileReads)
	}
	if meta.ISRC != "FROM-FILE" {
		t.Errorf("got ISRC %q, want the FILE's value after a cache error", meta.ISRC)
	}
}

// A STAT failure must also degrade to the file reader. A stat can fail where an
// open succeeds, and if the open genuinely fails too, readMetadata reports the
// real error -- this is not the place to decide the item's fate.
func TestResolveMetadataStatFailureReadsFile(t *testing.T) {
	cache := &fakeMetaCache{found: true, facts: scanner.AudioFacts{TrackLength: 210, ISRC: "CACHED"}}
	w, fileReads := newCacheTestWorker(t, cache, 0, 0)
	w.statFile = func(string) (int64, int64, error) { return 0, 0, errors.New("no such file") }

	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if *fileReads != 1 {
		t.Errorf("file read %d times after a stat failure, want exactly 1", *fileReads)
	}
	if cache.calls != 0 {
		t.Error("the cache must not be consulted when the identity to validate against is unknown")
	}
	if meta.ISRC != "FROM-FILE" {
		t.Errorf("got ISRC %q, want the FILE's value", meta.ISRC)
	}
}

// With no cache wired, behavior is exactly pre-#712: every item reads the file.
// Nil must disable the feature rather than panic or silently skip the refresh.
func TestResolveMetadataNilCacheReadsFile(t *testing.T) {
	w, fileReads := newCacheTestWorker(t, nil, 100, 200)
	w.metaCache = nil

	if _, err := w.resolveMetadata(context.Background(), "/music/song.mp3"); err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if *fileReads != 1 {
		t.Errorf("file read %d times with no cache wired, want exactly 1", *fileReads)
	}
}

// THE CACHE KEY MUST BE CANONICAL (#643, #712). audio_metadata rows are written
// under an absolute, symlink-resolved key, while a scan-enqueued item's
// SourcePath carries the CONFIGURED root's spelling. Querying the raw path
// misses every row on a symlinked library root -- the deployment shape #643 was
// filed against -- turning this cache into a silent no-op that still costs a
// stat and a query per item. Review caught this: recordDuration, twenty lines
// below, re-derives its key for exactly this reason and resolveMetadata did not.
func TestResolveMetadataCanonicalizesTheCacheKey(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "array", "music")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "music")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	song := filepath.Join(real, "song.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The row is written under the RESOLVED path, as index_metadata.go does.
	wantKey, err := filepath.EvalSymlinks(song)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}

	cache := &fakeMetaCache{found: true, facts: scanner.AudioFacts{TrackLength: 210, ISRC: "CACHED"}}
	w, fileReads := newCacheTestWorker(t, cache, 100, 200)

	// The worker is handed the SYMLINKED spelling, as a scan-enqueued item is.
	viaLink := filepath.Join(link, "song.mp3")
	if _, err := w.resolveMetadata(context.Background(), viaLink); err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}

	if cache.gotPath != wantKey {
		t.Errorf("cache queried with %q, want the canonical %q; a raw key misses every row on a symlinked root",
			cache.gotPath, wantKey)
	}
	if *fileReads != 0 {
		t.Errorf("file opened %d time(s) on a hit", *fileReads)
	}
}

// New() must wire statFile, or resolveMetadata's nil guard silently disables the
// cache for every production worker. Review deleted that one line and the whole
// suite stayed green.
func TestNewWiresStatFile(t *testing.T) {
	w := New(nil, nil, nil, nil)
	if w.statFile == nil {
		t.Fatal("New must wire statFile; without it resolveMetadata's guard disables the cache for every production worker (#712)")
	}
	// And it must report a real file's identity, not a zero stamp.
	f := filepath.Join(t.TempDir(), "x.mp3")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mtime, size, err := w.statFile(f)
	if err != nil {
		t.Fatalf("statFile: %v", err)
	}
	if mtime == 0 || size != 3 {
		t.Errorf("statFile = (%d, %d), want a real mtime and size 3", mtime, size)
	}
}

// SetMetadataCache must actually take effect, so a wiring regression in runServe
// cannot pass as a no-op.
func TestSetMetadataCacheTakesEffect(t *testing.T) {
	w := New(nil, nil, nil, nil)
	if w.metaCache != nil {
		t.Fatal("a fresh Worker must have no cache until one is wired")
	}
	cache := &fakeMetaCache{}
	w.SetMetadataCache(cache)
	if w.metaCache == nil {
		t.Fatal("SetMetadataCache did not take effect")
	}
}
