package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/testutil"
)

// TestScanLibrary_ManualMarkerNeverReopened pins #1218: --upgrade and --update
// reopen an instrumental marker, except one carrying [source:manual]. The
// provider- and detector-written markers are the control group.
func TestScanLibrary_ManualMarkerNeverReopened(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   int
	}{
		{"manual", lyrics.SourceManual, 0},
		{"provider", "musixmatch", 1},
		{"detector", lyrics.SourceDetector, 1},
	}
	modes := map[string]ScanOptions{
		"upgrade": {MaxDepth: 100, Upgrade: true},
		"update":  {MaxDepth: 100, Update: true},
	}
	for _, tc := range cases {
		for mode, opts := range modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000, map[string]string{}); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				body := "[by:canticle]\n[source:" + tc.source + "]\n" + lyrics.InstrumentalMarker + "\n"
				if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				res, err := NewScanner().ScanLibrary(context.Background(), dir, opts)
				if err != nil {
					t.Fatalf("scan: %v", err)
				}
				if len(res) != tc.want {
					t.Errorf("got %d results, want %d", len(res), tc.want)
				}
			})
		}
	}
}

// A manual marker under a different extension case (song.TXT) must hold the
// track settled even when the exact-case song.txt (a provider marker) wins path
// resolution. Needs a case-sensitive filesystem; skipped elsewhere.
func TestScanLibrary_ManualMarkerCaseVariantNeverReopened(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.txt and song.TXT would alias")
	}
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000, map[string]string{}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	marker := func(name, source string) {
		body := "[by:canticle]\n[source:" + source + "]\n" + lyrics.InstrumentalMarker + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marker("song.txt", "musixmatch")
	marker("song.TXT", lyrics.SourceManual)
	res, err := NewScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, Upgrade: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("got %d results, want 0 (manual variant must settle the track)", len(res))
	}
}
