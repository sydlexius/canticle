package scanner

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// enrichFixtureDir writes a single track carrying both an ISRC and a MusicBrainz
// recording ID, used by the enrichment-gating tests.
func enrichFixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3", "Artist", "Title", "Album", "",
		map[string]string{"TSRC": testISRC},
		map[string]string{"MusicBrainz Track Id": testMBID}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// TestScanLibrary_EnrichOn_ExtractsAll verifies that with EnrichRecording=true the
// scanner extracts ISRC, recording MBID, and duration.
func TestScanLibrary_EnrichOn_ExtractsAll(t *testing.T) {
	dir := enrichFixtureDir(t)
	sc := &Scanner{probeFunc: func(string) (int, error) { return 180, nil }}

	results, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1, EnrichRecording: true})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	got := results[0].Track
	if got.ISRC != testISRC {
		t.Errorf("ISRC = %q; want %q", got.ISRC, testISRC)
	}
	if got.RecordingMBID != testMBID {
		t.Errorf("RecordingMBID = %q; want %q", got.RecordingMBID, testMBID)
	}
	if got.TrackLength != 180 {
		t.Errorf("TrackLength = %d; want 180", got.TrackLength)
	}
}

// TestScanLibrary_EnrichOff_SkipsAll verifies that with EnrichRecording=false the
// scanner skips ISRC, MBID, and duration extraction entirely (the whole #191
// enrichment unit is one switch) - and never even invokes the duration prober.
func TestScanLibrary_EnrichOff_SkipsAll(t *testing.T) {
	dir := enrichFixtureDir(t)
	probed := false
	sc := &Scanner{probeFunc: func(string) (int, error) { probed = true; return 180, nil }}

	results, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1, EnrichRecording: false})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1 (track must still be scanned)", len(results))
	}
	got := results[0].Track
	if got.ISRC != "" {
		t.Errorf("ISRC = %q; want empty (enrichment off)", got.ISRC)
	}
	if got.RecordingMBID != "" {
		t.Errorf("RecordingMBID = %q; want empty (enrichment off)", got.RecordingMBID)
	}
	if got.TrackLength != 0 {
		t.Errorf("TrackLength = %d; want 0 (enrichment off)", got.TrackLength)
	}
	if probed {
		t.Error("duration prober was called; want skipped when enrichment is off")
	}
	// Core (non-enrichment) metadata must still be populated.
	if got.TrackName != "Title" || got.ArtistName != "Artist" {
		t.Errorf("core tags lost: ArtistName=%q TrackName=%q", got.ArtistName, got.TrackName)
	}
}
