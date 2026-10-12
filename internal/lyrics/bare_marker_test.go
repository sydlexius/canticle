package lyrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// A bare marker that is re-decided as instrumental comes back stamped, so the
// bare population shrinks for good (#618).
func TestWriteLRCNoDowngrade_BareMarkerComesBackStamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, Slugify("Test Artist - Quiet Piece")+".txt")
	if err := os.WriteFile(fp, []byte(InstrumentalMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !BareMarkerOnDisk(fp) {
		t.Fatal("premise broken: the fixture should read as a bare marker")
	}
	song := models.Song{
		Track:           models.Track{ArtistName: "Test Artist", TrackName: "Quiet Piece", Instrumental: 1},
		WinningLane:     DetectorLaneName,
		DetectorVersion: "test-model-1",
	}
	if err := NewLRCWriter().WriteLRCNoDowngrade(song, "", dir); err != nil {
		t.Fatalf("WriteLRCNoDowngrade: %v", err)
	}
	prov, isMarker, err := ReadInstrumentalProvenance(fp)
	if err != nil || !isMarker || prov.Source != SourceDetector {
		t.Errorf("after re-decision: source=%q marker=%v err=%v; want a %q-stamped marker", prov.Source, isMarker, err, SourceDetector)
	}
	if BareMarkerOnDisk(fp) {
		t.Error("a re-decided marker must no longer read as bare")
	}
}

// BareMarkerOnDisk is true only for a marker with no [source:] value whose body
// is the marker line alone (#618).
func TestBareMarkerOnDisk_Table(t *testing.T) {
	m := InstrumentalMarker
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"bare", m + "\n", true},
		{"bare_by_header", "[by:canticle]\n" + m + "\n", true},
		{"bom", "\xef\xbb\xbf" + m + "\n", true},
		{"crlf", m + "\r\n", true},
		{"leading_blank_lines", "\n\n" + m + "\n", true},
		{"empty_source_header", "[source:]\n" + m + "\n", true},
		{"whitespace_source_header", "[source:   ]\n" + m + "\n", true},
		{"stamped", "[by:canticle]\n[source:detector]\n" + m + "\n", false},
		{"lyric_with_marker_line", "an invented first line\n" + m + "\nan invented last line\n", false},
		{"no_marker", "an invented unsynced lyric line\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fp := filepath.Join(t.TempDir(), "x.txt")
			if err := os.WriteFile(fp, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := BareMarkerOnDisk(fp); got != tc.want {
				t.Errorf("BareMarkerOnDisk = %v, want %v", got, tc.want)
			}
		})
	}
}
