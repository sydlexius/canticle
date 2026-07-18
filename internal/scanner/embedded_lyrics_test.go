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
