package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/testutil"
)

// #505 feedback-loop guard: the audio mtime bump changes an input canticle's own
// caches key on. A bumped, already-settled file must NOT be re-emitted for
// fetching (that would re-fetch, maybe rewrite, and bump again). The only
// permitted consequence is one duration re-probe (audiodur keys on path, mtime,
// size); a further scan probes nothing.
func TestScan_BumpedSettledFileIsNotReenqueuedAndReprobesOnce(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFile(dir, "song.flac", 44100, 44100*210); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("[00:01.00]words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(dir, "song.flac")
	canonical, err := filepath.EvalSymlinks(audio)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingDurationStore{}
	sc := NewScanner(WithDurationStore(store), WithIndexStore(newRecordingIndexStore(canonical, audio)))
	scan := func() int {
		t.Helper()
		res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1, EnrichRecording: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(res)
	}
	if n := scan(); n != 0 {
		t.Fatalf("baseline scan emitted %d results; want 0", n)
	}
	baseline := len(store.paths)

	// The bump as the writer performs it: mtime only, atime untouched.
	if err := os.Chtimes(audio, time.Time{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := scan(); n != 0 {
		t.Fatalf("scan after a bump emitted %d results; want 0 (re-enqueue loop)", n)
	}
	if got := len(store.paths) - baseline; got != 1 {
		t.Fatalf("duration re-probes after a bump = %d; want exactly 1", got)
	}
	if n := scan(); n != 0 || len(store.paths)-baseline != 1 {
		t.Fatalf("next scan: results=%d extra probes=%d; want 0 and none", n, len(store.paths)-baseline-1)
	}
}
