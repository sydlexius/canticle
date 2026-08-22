package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/testutil"
)

// Shared fixture identity. The tags are load-bearing: prune's name tier filters
// its pool on an exact normalized TITLE, so a row indexed without them could
// never satisfy the gate.
const (
	testArtist = "Some Artist"
	testTitle  = "Some Title"
)

// recordingIndexStore is a real keyed fake, not a constant stub: the property
// worth asserting is that a file ALREADY in the index is not re-emitted, so
// Indexed must answer from what the test actually seeded.
type recordingIndexStore struct {
	indexed map[string]bool
	// lookups counts every query, so a test can assert the gate ran at all --
	// a store that is never consulted would otherwise look identical to one
	// that answered "not indexed" for everything.
	lookups int
	// lookupErr, when set, is returned by every Indexed call to exercise the
	// fail-closed branch.
	lookupErr error
}

func newRecordingIndexStore(seed ...string) *recordingIndexStore {
	s := &recordingIndexStore{indexed: map[string]bool{}}
	for _, p := range seed {
		s.indexed[p] = true
	}
	return s
}

func (r *recordingIndexStore) Indexed(_ context.Context, path string) (bool, error) {
	r.lookups++
	if r.lookupErr != nil {
		return false, r.lookupErr
	}
	return r.indexed[path], nil
}

// writeSettledTrack writes a tagged FLAC plus the sidecar that makes the
// scanner treat it as settled, which is exactly the state that makes the fetch
// path skip it before it is ever indexed.
func writeSettledTrack(t *testing.T, dir, sidecarExt string) string {
	t.Helper()
	const (
		stem   = "song"
		artist = testArtist
		title  = testTitle
	)
	if err := testutil.WriteFLACFileWithComments(dir, stem+".flac", 44100, 44100*30,
		map[string]string{"ARTIST": artist, "TITLE": title}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+sidecarExt), []byte("[00:01.00]x"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return filepath.Join(dir, stem+".flac")
}

// A settled file that is NOT yet in the index must still be emitted, so a file
// that MOVED (carrying its sidecar with it) can enter scan_results at its new
// path. Without this, prune's heuristic relink pool is structurally empty of
// exactly the files it needs to find (#786).
func TestScanLibrary_SettledButUnindexedFileIsEmitted(t *testing.T) {
	dir := t.TempDir()
	path := writeSettledTrack(t, dir, ".lrc")

	store := newRecordingIndexStore() // nothing indexed: the relocated-file case
	sc := NewScanner(WithIndexStore(store))

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results; want 1 (a settled-but-unindexed file must be indexed)", len(res))
	}
	if res[0].FilePath != path {
		t.Errorf("FilePath = %q; want %q", res[0].FilePath, path)
	}
	// GATE 5 of prune's name tier filters the pool on titleKey(Title), and an
	// empty title can never match a non-empty one -- so indexing the PATH alone
	// would be a no-op fix. The tags are the load-bearing part.
	if res[0].Track.TrackName != testTitle {
		t.Errorf("TrackName = %q; want %q (an untagged row can never satisfy prune's title gate)", res[0].Track.TrackName, testTitle)
	}
	if res[0].Track.ArtistName != testArtist {
		t.Errorf("ArtistName = %q; want %q", res[0].Track.ArtistName, testArtist)
	}
	// Status must NOT be pending: EnqueuePending drains exactly the pending rows,
	// so indexing a settled file as pending would queue a fetch for lyrics that
	// are already on disk -- changing the scan's outcome, which this path may
	// never do.
	if res[0].Status != "done" {
		t.Errorf("Status = %q; want %q (a settled file must never be enqueued for fetching)", res[0].Status, "done")
	}
}

// The steady-state case, and the whole reason the gate exists: a file already
// in the index is not re-emitted, so an ordinary scan of a settled library does
// no extra work.
func TestScanLibrary_SettledAndIndexedFileIsNotEmitted(t *testing.T) {
	dir := t.TempDir()
	path := writeSettledTrack(t, dir, ".lrc")

	store := newRecordingIndexStore(path)
	sc := NewScanner(WithIndexStore(store))

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d results; want 0 (an already-indexed settled file must stay skipped)", len(res))
	}
	if store.lookups == 0 {
		t.Error("index store was never consulted; the gate did not run")
	}
}

// A settled .txt is the shape the original #740 cohort had on disk, so it must
// be covered too -- not just the .lrc branch.
func TestScanLibrary_SettledUnsyncedTxtUnindexedIsEmitted(t *testing.T) {
	dir := t.TempDir()
	writeSettledTrack(t, dir, ".txt")

	store := newRecordingIndexStore()
	sc := NewScanner(WithIndexStore(store))

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results; want 1 (a settled .txt that is unindexed must be indexed)", len(res))
	}
	if res[0].Status != "done" {
		t.Errorf("Status = %q; want %q", res[0].Status, "done")
	}
}

// No store wired is the non-serve case (the fetch CLI). Behavior must be
// exactly what it was before this change: the settled file stays skipped.
func TestScanLibrary_NoIndexStoreLeavesSettledFileSkipped(t *testing.T) {
	dir := t.TempDir()
	writeSettledTrack(t, dir, ".lrc")

	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d results; want 0 (no index store must not change scan behavior)", len(res))
	}
}

// A lookup error must fail CLOSED toward not emitting. Guessing "not indexed"
// on a sick database would re-emit the whole library every scan, which is the
// disk-churn symptom this design exists to avoid.
func TestScanLibrary_IndexLookupErrorDoesNotEmit(t *testing.T) {
	dir := t.TempDir()
	writeSettledTrack(t, dir, ".lrc")

	store := newRecordingIndexStore()
	store.lookupErr = errors.New("database is locked")
	sc := NewScanner(WithIndexStore(store))

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d results; want 0 (a lookup error must fail closed toward not emitting)", len(res))
	}
}

// A settled INSTRUMENTAL marker is a skip too, and in serve mode it is the
// FIRST branch a lone .txt matches (the instrumental case is tested before the
// plain-unsynced one), so wiring only the .lrc and unsynced branches would
// leave an instrumental track invisible to the index forever.
func TestScanLibrary_SettledInstrumentalMarkerUnindexedIsEmitted(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": testArtist, "TITLE": testTitle}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte(lyrics.InstrumentalMarker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	store := newRecordingIndexStore()
	sc := NewScanner(WithIndexStore(store))

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results; want 1 (a settled instrumental marker that is unindexed must be indexed)", len(res))
	}
	if res[0].Status != "done" {
		t.Errorf("Status = %q; want %q", res[0].Status, "done")
	}
	if res[0].Track.TrackName != testTitle {
		t.Errorf("TrackName = %q; want %q", res[0].Track.TrackName, testTitle)
	}
}

// The two window-narrowed skips (#617's --unsynced-before). These sit INSIDE
// branches that already granted a reopen, so the file is genuinely
// fetch-eligible and is being skipped only because a dated repair run scoped
// this pass to an earlier cohort. It is still a settled file being skipped, so
// it must still be indexed -- otherwise running a dated repair leaves exactly
// the out-of-cohort files invisible to prune.
func TestScanLibrary_SettledOutsideRepairWindowIsStillIndexed(t *testing.T) {
	// A cutoff in the past puts any freshly-written sidecar OUTSIDE the window.
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("unsynced txt", func(t *testing.T) {
		dir := t.TempDir()
		writeSettledTrack(t, dir, ".txt")

		store := newRecordingIndexStore()
		sc := NewScanner(WithIndexStore(store))

		res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{
			MaxDepth: 1, Upgrade: true, UnsyncedBefore: past,
		})
		if err != nil {
			t.Fatalf("ScanLibrary: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d results; want 1 (outside the repair window is still a settled skip)", len(res))
		}
		if res[0].Status != "done" {
			t.Errorf("Status = %q; want %q", res[0].Status, "done")
		}
	})

	t.Run("provisional instrumental marker", func(t *testing.T) {
		dir := t.TempDir()
		if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 44100*30,
			map[string]string{"ARTIST": testArtist, "TITLE": testTitle}); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		// A DETECTOR-written marker is provisional, so --upgrade grants the reopen
		// and the window check is what stops it -- the branch under test.
		marker := "[source:" + lyrics.SourceDetector + "]\n[dv:v1]\n" + lyrics.InstrumentalMarker
		if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte(marker), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		store := newRecordingIndexStore()
		sc := NewScanner(WithIndexStore(store))

		res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{
			MaxDepth: 1, Upgrade: true, UnsyncedBefore: past, DetectorVersion: "v1",
		})
		if err != nil {
			t.Fatalf("ScanLibrary: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d results; want 1 (outside the repair window is still a settled skip)", len(res))
		}
		if res[0].Status != "done" {
			t.Errorf("Status = %q; want %q", res[0].Status, "done")
		}
	})
}
