package scanner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/testutil"
)

// bareMarkerFixture lays out one track per sidecar kind in a temp dir and
// returns the dir. Stems are invented names.
func bareMarkerFixture(t *testing.T, mtime time.Time) string {
	t.Helper()
	dir := t.TempDir()
	m := lyrics.InstrumentalMarker + "\n"
	sidecars := map[string]string{
		"bare":       m,
		"bareby":     "[by:canticle]\n" + m,
		"detector":   "[by:canticle]\n[source:" + lyrics.SourceDetector + "]\n" + m,
		"titlerule":  "[by:canticle]\n[source:" + lyrics.TitleRuleLaneName + "]\n" + m,
		"provider":   "[by:canticle]\n[source:musixmatch]\n" + m,
		"manual":     "[by:canticle]\n[source:" + lyrics.SourceManual + "]\n" + m,
		"unsynced":   "an invented unsynced lyric line\n",
		"lyricmark":  "an invented first line\n" + lyrics.InstrumentalMarker + "\nan invented last line\n",
		"unreadable": m,
	}
	for stem, body := range sidecars {
		if err := testutil.WriteFLACFileWithComments(dir, stem+".flac", 44100, 441000, map[string]string{}); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		p := filepath.Join(dir, stem+".txt")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	// A settled .lrc with no .txt.
	if err := testutil.WriteFLACFileWithComments(dir, "synced.flac", 44100, 441000, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "synced.lrc"), []byte("[00:01.00]an invented line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked marker whose target is a bare marker.
	if err := testutil.WriteFLACFileWithComments(dir, "linked.flac", 44100, 441000, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	// An unreadable marker: the file exists but cannot be opened.
	if err := os.Chmod(filepath.Join(dir, "unreadable.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "unreadable.txt"), 0o600) })
	return dir
}

func reopenedStems(t *testing.T, dir string, opts ScanOptions) []string {
	t.Helper()
	opts.MaxDepth = 1
	res, err := NewScanner().ScanLibrary(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got []string
	for _, r := range res {
		got = append(got, strings.TrimSuffix(filepath.Base(r.FilePath), ".flac"))
	}
	sort.Strings(got)
	return got
}

// TestScanLibrary_BareMarkersOnly pins #618: with --upgrade --bare-markers only a
// marker with no [source:] header reopens, and plain --upgrade is unchanged.
func TestScanLibrary_BareMarkersOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file; the unreadable fixture needs a non-root user")
	}
	old := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	dir := bareMarkerFixture(t, old)

	t.Run("bare_markers_reopens_only_bare", func(t *testing.T) {
		got := reopenedStems(t, dir, ScanOptions{Upgrade: true, BareMarkersOnly: true})
		if want := []string{"bare", "bareby"}; !reflect.DeepEqual(got, want) {
			t.Errorf("reopened %v, want %v", got, want)
		}
	})
	t.Run("plain_upgrade_unchanged", func(t *testing.T) {
		got := reopenedStems(t, dir, ScanOptions{Upgrade: true})
		want := []string{"bare", "bareby", "detector", "linked", "lyricmark", "provider", "titlerule", "unreadable", "unsynced"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("reopened %v, want %v", got, want)
		}
	})

	t.Run("composes_with_cutoff", func(t *testing.T) {
		cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		got := reopenedStems(t, dir, ScanOptions{Upgrade: true, BareMarkersOnly: true, UnsyncedBefore: cutoff})
		if want := []string{"bare", "bareby"}; !reflect.DeepEqual(got, want) {
			t.Errorf("older than cutoff: reopened %v, want %v", got, want)
		}
		before := old.Add(-time.Hour)
		got = reopenedStems(t, dir, ScanOptions{Upgrade: true, BareMarkersOnly: true, UnsyncedBefore: before})
		if len(got) != 0 {
			t.Errorf("newer than cutoff: reopened %v, want none", got)
		}
	})
}
