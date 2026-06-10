package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
)

// TestGenerateID3v2Extended exercises the extended generator with both extra
// text frames (e.g. TSRC) and TXXX frames (e.g. MusicBrainz Track Id).
func TestGenerateID3v2Extended(t *testing.T) {
	data := GenerateID3v2Extended("Artøst", "Tîtle", "Albüm", "la la la",
		map[string]string{"TSRC": "USRC17607839"},
		map[string]string{"MusicBrainz Track Id": "11111111-2222-3333-4444-555555555555"})

	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if m.Artist() != "Artøst" || m.Title() != "Tîtle" || m.Album() != "Albüm" {
		t.Errorf("tags = %q / %q / %q, want Artøst / Tîtle / Albüm",
			m.Artist(), m.Title(), m.Album())
	}
}

// TestGenerateID3v2Extended_NilExtras covers the nil-extras path (what
// GenerateID3v2 delegates to).
func TestGenerateID3v2Extended_NilExtras(t *testing.T) {
	data := GenerateID3v2Extended("A", "T", "Al", "", nil, nil)
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if m.Artist() != "A" || m.Title() != "T" {
		t.Errorf("tags = %q / %q, want A / T", m.Artist(), m.Title())
	}
}

// TestGenerateFLAC verifies the synthetic FLAC generator emits a FLAC stream.
func TestGenerateFLAC(t *testing.T) {
	data := GenerateFLAC(44100, 441000) // ~10 seconds
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		t.Fatalf("GenerateFLAC did not produce a FLAC stream (len=%d)", len(data))
	}
}

// TestWriteAudioFileExtended writes a tagged file and reads it back.
func TestWriteAudioFileExtended(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAudioFileExtended(dir, "song.mp3", "Artist", "Title", "Album", "",
		map[string]string{"TSRC": "USRC17607839"}, nil); err != nil {
		t.Fatalf("WriteAudioFileExtended: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "song.mp3"))
	if err != nil {
		t.Fatalf("open written file: %v", err)
	}
	defer func() { _ = f.Close() }()

	m, err := tag.ReadFrom(f)
	if err != nil {
		t.Fatalf("ReadFrom written file: %v", err)
	}
	if m.Artist() != "Artist" {
		t.Errorf("Artist = %q, want Artist", m.Artist())
	}
}

// TestWriteFLACFile writes a synthetic FLAC and confirms a non-empty file.
func TestWriteFLACFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFLACFile(dir, "track.flac", 48000, 96000); err != nil {
		t.Fatalf("WriteFLACFile: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "track.flac"))
	if err != nil {
		t.Fatalf("stat written FLAC: %v", err)
	}
	if info.Size() == 0 {
		t.Error("written FLAC is empty")
	}
}
