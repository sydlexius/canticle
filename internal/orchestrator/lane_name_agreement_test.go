package orchestrator

import (
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
)

// The detector lane's name is spelled in two packages and they must agree.
//
// internal/lyrics decides a marker is detector-written by comparing
// models.Song.WinningLane against lyrics.DetectorLaneName. That value is set
// here, by this package. A silent disagreement would label every detector
// marker with [source:detector] instead of [source:canticle-detector], so
// IsDetector() would read false and the scanner would treat a provisional
// detector verdict as editorially terminal -- never re-checkable by --upgrade.
//
// lyrics cannot import orchestrator (orchestrator depends on lyrics), so the
// constant is duplicated by necessity. This test is what makes that duplication
// safe; without it the coupling is a comment, and comments do not fail builds.
func TestDetectorLaneNameAgreesWithLyricsProvenance(t *testing.T) {
	if detectorLaneName != lyrics.DetectorLaneName {
		t.Fatalf("lane name disagreement: orchestrator=%q lyrics=%q.\n"+
			"Every detector-written instrumental marker would be misattributed to a provider lane.",
			detectorLaneName, lyrics.DetectorLaneName)
	}
}
