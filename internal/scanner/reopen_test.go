package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/testutil"
)

func TestReopenClassesFor(t *testing.T) {
	cases := []struct {
		name            string
		update, upgrade bool
		want            reopenClasses
	}{
		{"neither", false, false, reopenClasses{}},
		// --upgrade never sets Synced: a line .lrc's path to word sync is the
		// queue-driven word recheck, not a per-scan reopen (#575, #553).
		{"upgrade", false, true, reopenClasses{Unsynced: true}},
		{"update", true, false, reopenClasses{Unsynced: true, Synced: true}},
		{"both", true, true, reopenClasses{Unsynced: true, Synced: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reopenClassesFor(ScanOptions{Update: tc.update, Upgrade: tc.upgrade})
			if got != tc.want {
				t.Fatalf("reopenClassesFor(update=%v,upgrade=%v) = %+v, want %+v", tc.update, tc.upgrade, got, tc.want)
			}
		})
	}
}

// TestDetectorVersionMoved: the version reopen fires only for a detector
// marker whose recorded version differs from a known current one. An
// unreadable header never counts (it no longer pins anything either way).
func TestDetectorVersionMoved(t *testing.T) {
	marker := func(header string) string { return header + lyrics.InstrumentalMarker + "\n" }
	cases := []struct {
		name, body, current string
		want                bool
	}{
		{"detector bumped", marker("[source:canticle-detector]\n[dv:1.0]\n"), "2.0", true},
		{"detector same version", marker("[source:canticle-detector]\n[dv:1.0]\n"), "1.0", false},
		{"no current version", marker("[source:canticle-detector]\n[dv:1.0]\n"), "", false},
		{"detector without dv", marker("[source:canticle-detector]\n"), "2.0", false},
		{"provider marker", marker("[source:musixmatch]\n"), "2.0", false},
		// A [dv:] on a marker the detector did not write is not a detector
		// verdict, so a version bump must not reopen it (pins the IsDetector
		// check: without it this case reads "moved").
		{"provider marker with dv", marker("[source:musixmatch]\n[dv:1.0]\n"), "2.0", false},
		{"bare marker with dv", marker("[dv:1.0]\n"), "2.0", false},
		{"bare marker", marker(""), "2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "song.txt")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := detectorVersionMoved(p, tc.current); got != tc.want {
				t.Fatalf("detectorVersionMoved = %v, want %v", got, tc.want)
			}
		})
	}
	if detectorVersionMoved(filepath.Join(t.TempDir(), "missing.txt"), "2.0") {
		t.Error("an unreadable marker must not trigger the version reopen")
	}
}

// TestScanDir_MarkerHeaderReadOnlyWithoutFlagReopen pins the short-circuit in
// the instrumental branch: when --upgrade already granted the reopen, the
// marker's provenance header is never read (one fewer file read per marker per
// scan). Swapping the operands of that && still enqueues the marker, so only a
// count of header reads can tell the two orders apart.
func TestScanDir_MarkerHeaderReadOnlyWithoutFlagReopen(t *testing.T) {
	var reads int
	orig := readInstrumentalProvenance
	readInstrumentalProvenance = func(path string) (lyrics.InstrumentalProvenance, bool, error) {
		reads++
		return orig(path)
	}
	t.Cleanup(func() { readInstrumentalProvenance = orig })

	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": testArtist, "TITLE": testTitle}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	marker := "[source:" + lyrics.SourceDetector + "]\n[dv:v1]\n" + lyrics.InstrumentalMarker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte(marker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	for _, tc := range []struct {
		name      string
		upgrade   bool
		wantReads int
		wantState string
	}{
		// The flag grants the reopen; the header must not be consulted.
		{"upgrade", true, 0, "pending"},
		// No flag: the version check is the only reopen left, so it reads.
		// The version moved (v1 -> v2), so the marker reopens.
		{"no flag", false, 1, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads = 0
			res, err := NewScanner().ScanLibrary(context.Background(), dir, ScanOptions{
				MaxDepth: 1, Upgrade: tc.upgrade, DetectorVersion: "v2",
			})
			if err != nil {
				t.Fatalf("ScanLibrary: %v", err)
			}
			if len(res) != 1 || res[0].Status != tc.wantState {
				t.Fatalf("results = %+v; want one %q result", res, tc.wantState)
			}
			if reads != tc.wantReads {
				t.Errorf("provenance header reads = %d; want %d", reads, tc.wantReads)
			}
		})
	}
}
