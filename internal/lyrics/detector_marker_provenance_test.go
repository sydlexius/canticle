package lyrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// A detector-written instrumental marker must be identifiable AS a detector
// marker even when the model version is unknown.
//
// The writer stamps [source:canticle-detector] only when DetectorVersion is
// non-empty, and IsDetector() keys purely on that source token. So an empty
// version writes a marker indistinguishable from a PROVIDER one. Before #553
// the scanner treated a provider marker as editorially terminal, so the
// detector's verdict froze on disk under a provider's authority. --upgrade now
// reopens every marker, but the attribution still gates the detector-version
// reopen and the recalibration reverse path (which never deletes a provider
// marker), so a misattributed verdict is still stranded there.
//
// This was structurally unreachable while DetectorVersion was the app version (a
// build constant that is never empty). Keying it to the sidecar model (#684)
// makes empty a ROUTINE state: every process start before /health answers, and
// permanently against a sidecar too old to report a version.
func TestWriteInstrumental_DetectorLaneIsIdentifiableWithoutAModelVersion(t *testing.T) {
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName:   "Test Artist",
			TrackName:    "Test Track",
			Instrumental: 1,
		},
		WinningLane:     "detector",
		DetectorVersion: "", // unknown: sidecar booting, or too old to report one
	}

	if err := NewLRCWriter().WriteLRC(song, "", tmpDir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	path := filepath.Join(tmpDir, Slugify("Test Artist - Test Track")+".txt")
	prov, _, err := ReadInstrumentalProvenance(path)
	if err != nil {
		t.Fatalf("ReadInstrumentalProvenance: %v", err)
	}

	if !prov.IsDetector() {
		body, _ := os.ReadFile(path) //nolint:errcheck // reason: diagnostic only, the assertion has already failed
		t.Fatalf("IsDetector() = false for a marker the DETECTOR wrote (source=%q).\n"+
			"An unknown model version must not disguise a detector verdict as a provider one: "+
			"a detector-version bump can then never re-check it, and recalibration can never reverse it.\nmarker:\n%s",
			prov.Source, body)
	}
}
