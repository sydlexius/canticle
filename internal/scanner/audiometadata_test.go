package scanner

import (
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// TestReadAudioMetadata_AllPresent uses a FLAC fixture because STREAMINFO gives
// an exact computed duration (totalSamples/sampleRate) while Vorbis comments
// carry ISRC and album, so one file exercises all three fields exactly.
func TestReadAudioMetadata_AllPresent(t *testing.T) {
	const sampleRate = 44100
	const totalSamples = 44100 * 180 // exactly 180 seconds

	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "track.flac", sampleRate, totalSamples,
		map[string]string{
			"ARTIST": "Artist",
			"TITLE":  "Title",
			"ALBUM":  "Album",
			"ISRC":   testISRC,
		}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := ReadAudioMetadata(filepath.Join(dir, "track.flac"))
	if err != nil {
		t.Fatalf("ReadAudioMetadata: %v", err)
	}
	if meta.TrackLength != 180 {
		t.Errorf("TrackLength = %d; want 180", meta.TrackLength)
	}
	if meta.ISRC != testISRC {
		t.Errorf("ISRC = %q; want %q", meta.ISRC, testISRC)
	}
	if meta.AlbumName != "Album" {
		t.Errorf("AlbumName = %q; want %q", meta.AlbumName, "Album")
	}
}

// TestReadAudioMetadata_TagsAbsent verifies the sentinel contract: a readable
// file missing these tags is not an error.
func TestReadAudioMetadata_TagsAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Artist", "Title", "", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := ReadAudioMetadata(filepath.Join(dir, "track.mp3"))
	if err != nil {
		t.Fatalf("ReadAudioMetadata: %v", err)
	}
	if meta.ISRC != "" {
		t.Errorf("ISRC = %q; want empty", meta.ISRC)
	}
	if meta.AlbumName != "" {
		t.Errorf("AlbumName = %q; want empty", meta.AlbumName)
	}
}

// TestReadAudioMetadata_MissingFile verifies an open failure is a wrapped error,
// so the caller can distinguish it from a genuinely untagged file.
func TestReadAudioMetadata_MissingFile(t *testing.T) {
	_, err := ReadAudioMetadata(filepath.Join(t.TempDir(), "nope.mp3"))
	if err == nil {
		t.Fatal("ReadAudioMetadata: got nil error for a missing file; want an error")
	}
}

// TestReadAudioMetadata_UnknownDurationIsNotAnError verifies that a file whose
// duration cannot be derived still returns its tags with TrackLength 0, rather
// than failing the whole read.
func TestReadAudioMetadata_UnknownDurationIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3", "Artist", "Title", "Album", "",
		map[string]string{"TSRC": testISRC}, nil); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := ReadAudioMetadata(filepath.Join(dir, "track.mp3"))
	if err != nil {
		t.Fatalf("ReadAudioMetadata: %v", err)
	}
	if meta.ISRC != testISRC {
		t.Errorf("ISRC = %q; want %q", meta.ISRC, testISRC)
	}
	if meta.AlbumName != "Album" {
		t.Errorf("AlbumName = %q; want %q", meta.AlbumName, "Album")
	}
	if meta.TrackLength < 0 {
		t.Errorf("TrackLength = %d; want 0 or a positive duration", meta.TrackLength)
	}
}
