package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

func TestScanLibrary_EmbeddedLyricsRespect(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "song.mp3", "Artist", "Title", "Album", "la la la"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	// off (default): embedded lyrics ignored, the file is scanned/enqueued.
	off, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100})
	if err != nil {
		t.Fatalf("scan off: %v", err)
	}
	if len(off) != 1 {
		t.Fatalf("off mode: got %d results; want 1", len(off))
	}

	// respect: a file that already carries embedded lyrics is skipped.
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "respect"})
	if err != nil {
		t.Fatalf("scan respect: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("respect mode: got %d results; want 0 (skipped)", len(res))
	}
}

// A track whose lyrics were extracted from tags to an unsynced .txt must still
// be enqueued when an upgrade pass is requested, so a provider can promote it to
// synced. Extraction produces the unsynced form only (the USLT frame), which is
// not a terminal result -- treating it as one freezes the track permanently in a
// worse state than a normal unsynced fetch, which upgrade does revisit.
//
// The ordering under test: the .txt sidecar check correctly defers to Upgrade,
// but the embedded-lyrics block runs afterward and skips the file regardless
// (#538).
func TestScanLibrary_EmbeddedLyricsExtractDoesNotBlockUpgrade(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "song.mp3", "Artist", "Title", "Album", "placeholder lyric text"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	// First pass: extraction writes the unsynced sidecar and skips the fetch.
	first, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("first pass: got %d results; want 0 (extracted, fetch skipped)", len(first))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.txt")); err != nil {
		t.Fatalf("expected song.txt after extraction: %v", err)
	}

	// Upgrade pass: the track holds only unsynced lyrics, so it must be enqueued
	// for a synced re-fetch.
	upgraded, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract", Upgrade: true})
	if err != nil {
		t.Fatalf("upgrade scan: %v", err)
	}
	if len(upgraded) != 1 {
		t.Fatalf("upgrade pass: got %d results; want 1 -- an extracted unsynced track must stay eligible for synced promotion, not be frozen at unsynced", len(upgraded))
	}
}

// A failed sidecar write must not drop the track. The extract path promises the
// track is still enqueued so an ordinary fetch is attempted -- silently skipping
// it would lose the work with only a warning to show for it.
func TestScanLibrary_EmbeddedLyricsExtractWriteFailureStillEnqueues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not block writes, so the failure cannot be injected")
	}
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "song.mp3", "Artist", "Title", "Album", "placeholder lyric text"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Read-only directory: the sidecar's temp file cannot be created, so
	// extraction fails. t.TempDir()'s cleanup needs the bit back.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results; want 1 -- a failed sidecar write must fall through to enqueue, not silently skip the track", len(res))
	}
	if _, statErr := os.Stat(filepath.Join(dir, "song.txt")); statErr == nil {
		t.Error("song.txt exists; the write was supposed to fail, so this test is not exercising the failure path")
	}
}

func TestScanLibrary_EmbeddedLyricsExtract(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "song.mp3", "Artist", "Title", "Album", "la la la"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan extract: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("extract mode: got %d results; want 0 (skipped after extraction)", len(res))
	}
	got, err := os.ReadFile(filepath.Join(dir, "song.txt"))
	if err != nil {
		t.Fatalf("expected song.txt sidecar: %v", err)
	}
	if string(got) != "la la la" {
		t.Errorf("sidecar content = %q; want %q", string(got), "la la la")
	}
}
