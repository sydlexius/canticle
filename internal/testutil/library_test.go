package testutil_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

func TestGenerateLibraryAndScan(t *testing.T) {
	dir := t.TempDir()
	n, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Artists: 2, Albums: 2, Tracks: 3})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if n != 12 {
		t.Fatalf("generated %d tracks; want 12", n)
	}

	sc := scanner.NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, scanner.ScanOptions{MaxDepth: 100})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 12 {
		t.Fatalf("scanned %d files; want 12", len(res))
	}
}

func TestGenerateLibraryEmbedsLyricsAndValidates(t *testing.T) {
	dir := t.TempDir()
	// EmbedLyrics path: tracks carry embedded lyrics, so a respect-mode scan
	// skips them all.
	n, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Artists: 1, Albums: 1, Tracks: 2, EmbedLyrics: true})
	if err != nil || n != 2 {
		t.Fatalf("generate embed: n=%d err=%v; want 2, nil", n, err)
	}
	sc := scanner.NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, scanner.ScanOptions{MaxDepth: 100, EmbeddedLyrics: "respect"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("respect scan of embedded library returned %d; want 0", len(res))
	}

	// Invalid spec is rejected.
	if _, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Artists: 0, Albums: 1, Tracks: 1}); err == nil {
		t.Fatal("GenerateLibrary with 0 artists = nil error; want a validation error")
	}
}

func TestGenerateLibraryRealistic(t *testing.T) {
	dir := t.TempDir()
	n, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Realistic: true, EmbedLyrics: true, LRCEvery: 5})
	if err != nil {
		t.Fatalf("generate realistic: %v", err)
	}
	if n != len(testutil.RealisticTracks) {
		t.Fatalf("generated %d; want %d (one per RealisticTracks entry)", n, len(testutil.RealisticTracks))
	}
	var mp3s, lrcs int
	if err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(d.Name(), ".mp3"):
			mp3s++
		case strings.HasSuffix(d.Name(), ".lrc"):
			lrcs++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if mp3s != n {
		t.Fatalf("found %d .mp3 files; want %d", mp3s, n)
	}
	if lrcs == 0 {
		t.Fatalf("LRCEvery=5 should have produced at least one .lrc sidecar")
	}
}

func TestGenerateLibraryLRCSidecarsAreSkipped(t *testing.T) {
	dir := t.TempDir()
	if _, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Artists: 1, Albums: 1, Tracks: 4, LRCEvery: 1}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	sc := scanner.NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, scanner.ScanOptions{MaxDepth: 100})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("scanned %d files; want 0 (all have .lrc sidecars)", len(res))
	}
}

// TestGenerateLibraryConcurrentScans exercises the scan path on synthetic data
// from multiple goroutines at once (the deeper cross-process SQLITE_BUSY path is
// covered by the scan/queue concurrency tests).
func TestGenerateLibraryConcurrentScans(t *testing.T) {
	dir := t.TempDir()
	if _, err := testutil.GenerateLibrary(dir, testutil.GenSpec{Artists: 3, Albums: 2, Tracks: 2}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	sc := scanner.NewScanner()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := sc.ScanLibrary(context.Background(), dir, scanner.ScanOptions{MaxDepth: 100})
			if err != nil {
				errs <- err
				return
			}
			if len(res) != 12 {
				errs <- nil
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent scan: %v", err)
		}
		t.Fatalf("concurrent scan returned an unexpected file count")
	}
}
