package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// recordingWorkerDurationStore captures what the worker banks, so tests assert
// on path and seconds without a real audiodur.Store.
type recordingWorkerDurationStore struct {
	paths   []string
	seconds []int
	mtimes  []int64
	sizes   []int64
}

func (r *recordingWorkerDurationStore) Record(_ context.Context, path string, mtimeNano, size int64, seconds int) error {
	r.paths = append(r.paths, path)
	r.seconds = append(r.seconds, seconds)
	r.mtimes = append(r.mtimes, mtimeNano)
	r.sizes = append(r.sizes, size)
	return nil
}

// refreshRecordingIdentity already re-reads the duration at fetch time and
// discards it after building the provider query. Banking it makes the queue path
// a second zero-cost fill site for the revalidation cache (#441).
func TestWorkerRecordsRefreshedDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, []byte("not-real-audio-but-a-real-file"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := &recordingWorkerDurationStore{}
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 210, ISRC: "TEST00000001", MTimeNano: 12345, SizeBytes: 999}}
	w := New(&fakeQueue{}, &fakeCache{}, refreshFetcher(), &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)
	w.SetDurationStore(store)

	item := queue.WorkItem{ID: 1}
	item.Inputs.SourcePath = path

	got := w.refreshRecordingIdentity(context.Background(), item, models.Track{ArtistName: "A", TrackName: "T"})

	if got.TrackLength != 210 {
		t.Fatalf("refreshed TrackLength = %d, want 210", got.TrackLength)
	}
	if len(store.paths) != 1 || store.paths[0] != path {
		t.Fatalf("recorded paths = %v, want exactly [%q]", store.paths, path)
	}
	if store.seconds[0] != 210 {
		t.Fatalf("recorded %ds, want 210s", store.seconds[0])
	}
}

// A metadata read that returns a duration but no file identity (the open
// handle's stat failed, or the caller assembled AudioMetadata without it) must
// bank nothing: storing a duration under a zero mtime/size would let it
// validate as fresh against any file, defeating the whole point of stamping
// identity from the read's own handle rather than a path re-stat.
func TestWorkerRecordsNothingWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := &recordingWorkerDurationStore{}
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 210}}
	w := New(&fakeQueue{}, &fakeCache{}, refreshFetcher(), &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)
	w.SetDurationStore(store)

	item := queue.WorkItem{ID: 1}
	item.Inputs.SourcePath = path

	w.refreshRecordingIdentity(context.Background(), item, models.Track{ArtistName: "A", TrackName: "T"})

	if len(store.paths) != 0 {
		t.Fatalf("recorded %d durations without identity, want 0", len(store.paths))
	}
}

// TestWorkerRecordsDurationFromReadIdentityNotPath pins the fix for the defect
// where recordDuration stamped the cache row with a fresh os.Stat(path) taken
// AFTER w.readMetadata had already opened, read, and closed the file, rather
// than the identity of the handle the duration was actually read from. By the
// time recordDuration ran, a path-based stat was re-resolving the path from
// scratch -- vulnerable to a tagger's write-tmp-then-rename swapping the file
// between the metadata read and the stat, banking a wrong duration under a new
// file's mtime/size, where it would validate as fresh forever.
//
// This proves the fix directly: the fake reader returns a fixed identity
// (mtimeNano/size) for the READ, while a completely different file sits at the
// path when recordDuration runs. A path-based stat would observe the on-disk
// (different) file's identity; the fix uses the reader's returned identity
// unconditionally, so it must record the reader's values, never the on-disk
// file's.
func TestWorkerRecordsDurationFromReadIdentityNotPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, []byte("original-bytes"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const readMTime, readSize = int64(1000), int64(111)
	store := &recordingWorkerDurationStore{}
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 210, MTimeNano: readMTime, SizeBytes: readSize}}
	w := New(&fakeQueue{}, &fakeCache{}, refreshFetcher(), &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)
	w.SetDurationStore(store)

	// Simulate a tagger's write-tmp-then-rename landing between the metadata
	// read (already captured above via the fake) and recordDuration running:
	// replace the on-disk file with different content, so a path-based stat
	// would see a different size than what the read reported.
	if err := os.WriteFile(path, []byte("replacement-bytes-longer-than-original"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	onDisk, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replacement: %v", err)
	}
	if onDisk.Size() == readSize {
		t.Fatalf("replacement file is the same size (%d) as the read identity; swap does not change on-disk size, test cannot discriminate", onDisk.Size())
	}

	item := queue.WorkItem{ID: 1}
	item.Inputs.SourcePath = path

	w.refreshRecordingIdentity(context.Background(), item, models.Track{ArtistName: "A", TrackName: "T"})

	if len(store.paths) != 1 {
		t.Fatalf("recorded %d durations, want 1", len(store.paths))
	}
	if store.mtimes[0] != readMTime || store.sizes[0] != readSize {
		t.Fatalf("recorded (mtime=%d, size=%d), want the read identity (mtime=%d, size=%d) not the on-disk file's (size=%d)",
			store.mtimes[0], store.sizes[0], readMTime, readSize, onDisk.Size())
	}
}

// A metadata read that yields no duration must bank nothing: absence is how the
// table represents "unknown".
func TestWorkerRecordsNothingWithoutDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := &recordingWorkerDurationStore{}
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 0}}
	w := New(&fakeQueue{}, &fakeCache{}, refreshFetcher(), &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)
	w.SetDurationStore(store)

	item := queue.WorkItem{ID: 1}
	item.Inputs.SourcePath = path

	w.refreshRecordingIdentity(context.Background(), item, models.Track{ArtistName: "A", TrackName: "T"})

	if len(store.paths) != 0 {
		t.Fatalf("recorded %d durations for an unknown duration, want 0", len(store.paths))
	}
}

// A nil duration store must leave the existing refresh behavior untouched.
func TestWorkerRefreshWithoutDurationStoreIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 210}}
	w := New(&fakeQueue{}, &fakeCache{}, refreshFetcher(), &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)

	item := queue.WorkItem{ID: 1}
	item.Inputs.SourcePath = path

	got := w.refreshRecordingIdentity(context.Background(), item, models.Track{ArtistName: "A", TrackName: "T"})
	if got.TrackLength != 210 {
		t.Fatalf("refreshed TrackLength = %d, want 210", got.TrackLength)
	}
}
