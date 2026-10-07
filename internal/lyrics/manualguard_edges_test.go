package lyrics

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Parsing edges of the probe: CRLF, a leading BOM and a different-case tag
// value still read as manual (an operator's hand edit is still their verdict);
// a file over the 64 KiB cap reads as not manual and falls to the rung guard.
func TestManualMarkerOnDisk_ParsingEdges(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"crlf":       {"[by:canticle]\r\n[source:manual]\r\n" + InstrumentalMarker + "\r\n", true},
		"bom":        {"\xef\xbb\xbf" + manualMarkerBody, true},
		"value case": {"[by:canticle]\n[source:Manual]\n" + InstrumentalMarker + "\n", true},
		"oversize":   {manualMarkerBody + string(bytes.Repeat([]byte("x\n"), 1<<16)), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "song.txt")
			seed(t, p, c.body)
			if got := ManualMarkerOnDisk(p); got != c.want {
				t.Errorf("ManualMarkerOnDisk = %v, want %v", got, c.want)
			}
		})
	}
}

// A write to song.txt must also refuse beside a manual same-extension case
// variant (Song.TXT). Only meaningful on a case-sensitive filesystem.
func TestWriteLRC_ManualMarkerSameExtensionCaseVariant(t *testing.T) {
	dir := t.TempDir()
	seed(t, filepath.Join(dir, "CaseProbe.tmp"), "x")
	if _, err := os.Stat(filepath.Join(dir, "caseprobe.tmp")); err == nil {
		t.Skip("case-insensitive filesystem: a same-extension case variant cannot coexist here")
	}
	manual := filepath.Join(dir, "song.TXT")
	seed(t, manual, manualMarkerBody)
	_, unsynced, _ := rungSongs()
	w := NewLRCWriter()
	w.SetForceOverwrite(true)
	mustKeptManual(t, w.WriteLRC(unsynced, "song.txt", dir))
	if got := readText(t, manual); got != manualMarkerBody {
		t.Errorf("marker changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.txt")); err == nil {
		t.Error("a second .txt was written beside the manual marker")
	}
}
