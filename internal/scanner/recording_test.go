package scanner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/testutil"
)

const (
	testISRC = "GBRC12345678"
	testMBID = "550e8400-e29b-41d4-a716-446655440000"
)

// skipDurationScanner returns a Scanner whose probeFunc always returns 0,
// for tests that only care about tag extraction, not duration.
func skipDurationScanner() *Scanner {
	return &Scanner{probeFunc: func(string) (int, error) { return 0, nil }}
}

func TestExtractISRC_Present(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3", "Artist", "Title", "Album", "",
		map[string]string{"TSRC": testISRC}, nil); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.ISRC; got != testISRC {
		t.Errorf("Track.ISRC = %q; want %q", got, testISRC)
	}
}

func TestExtractISRC_Absent(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.ISRC; got != "" {
		t.Errorf("Track.ISRC = %q; want empty", got)
	}
}

func TestExtractRecordingMBID_Present(t *testing.T) {
	dir := t.TempDir()
	// mbz.Extract reads TXXX frames; "MusicBrainz Track Id" is the alias for
	// the Recording constant in the mbz package (mbz.tags[Recording]).
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3", "Artist", "Title", "Album", "",
		nil, map[string]string{"MusicBrainz Track Id": testMBID}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.RecordingMBID; got != testMBID {
		t.Errorf("Track.RecordingMBID = %q; want %q", got, testMBID)
	}
}

func TestExtractRecordingMBID_Absent(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.RecordingMBID; got != "" {
		t.Errorf("Track.RecordingMBID = %q; want empty", got)
	}
}

// TestProbeDuration_ProberCalled verifies that probeFunc is called and its return
// value populates Track.TrackLength.
func TestProbeDuration_ProberCalled(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sc := &Scanner{probeFunc: func(path string) (int, error) {
		if filepath.Base(path) != "track.mp3" {
			t.Errorf("probeFunc called with unexpected path %q", path)
		}
		return 180, nil
	}}
	results, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.TrackLength; got != 180 {
		t.Errorf("Track.TrackLength = %d; want 180", got)
	}
}

// TestProbeDuration_ProberError_GracefulDegrade verifies that a per-file probe
// error degrades TrackLength to 0 without failing the scan.
func TestProbeDuration_ProberError_GracefulDegrade(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sc := &Scanner{probeFunc: func(string) (int, error) { return 0, errors.New("corrupt file") }}
	results, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary must not fail on per-file probe error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1 (track must still be scanned)", len(results))
	}
	if got := results[0].Track.TrackLength; got != 0 {
		t.Errorf("Track.TrackLength = %d; want 0 (graceful degrade)", got)
	}
	wantPath := filepath.Join(dir, "track.mp3")
	if got := results[0].FilePath; got != wantPath {
		t.Errorf("FilePath = %q; want %q", got, wantPath)
	}
}

// TestAudioDuration_FLAC verifies that audioDuration correctly parses a STREAMINFO
// block to compute duration. The fixture encodes exactly 44100 samples at 44100 Hz
// (1 second), so int(secs) must equal 1.
func TestAudioDuration_FLAC(t *testing.T) {
	const sampleRate = 44100
	const totalSamples = 44100 // exactly 1 second

	data := testutil.GenerateFLAC(sampleRate, totalSamples)
	got, err := audioDuration(bytes.NewReader(data), ".flac")
	if err != nil {
		t.Fatalf("audioDuration: %v", err)
	}
	if got != 1 {
		t.Errorf("duration = %d; want 1", got)
	}
}

// TestAudioDuration_FLAC_MultiSecond exercises a longer duration.
func TestAudioDuration_FLAC_MultiSecond(t *testing.T) {
	const sampleRate = 44100
	const totalSamples = 44100 * 179 // 179 seconds exactly

	data := testutil.GenerateFLAC(sampleRate, totalSamples)
	got, err := audioDuration(bytes.NewReader(data), ".flac")
	if err != nil {
		t.Fatalf("audioDuration: %v", err)
	}
	if got != 179 {
		t.Errorf("duration = %d; want 179", got)
	}
}

// TestAudioDuration_UnknownExt verifies that an unrecognized extension returns
// an error and duration 0 (not a panic).
func TestAudioDuration_UnknownExt(t *testing.T) {
	got, err := audioDuration(bytes.NewReader([]byte("junk")), ".xyz")
	if err == nil {
		t.Error("expected error for unknown extension; got nil")
	}
	if got != 0 {
		t.Errorf("duration = %d; want 0 for error case", got)
	}
}

// TestWriteAudioFileExtended_CreatesFile verifies the testutil helper itself.
func TestWriteAudioFileExtended_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	err := testutil.WriteAudioFileExtended(dir, "track.mp3", "A", "T", "Al", "",
		map[string]string{"TSRC": "US-12345678"},
		map[string]string{"MusicBrainz Track Id": "some-uuid"})
	if err != nil {
		t.Fatalf("WriteAudioFileExtended: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "track.mp3")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}
