package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
)

// TestGenerateFLACExtended_VorbisComment verifies that a header-only FLAC built
// with a Vorbis SYNCEDLYRICS comment is parsed by dhowden/tag and surfaces the
// value via Raw() under the lowercased key. This is the load-bearing assumption
// for issue #395 (extract embedded synced lyrics): dhowden must read the Vorbis
// Comment block even when the FLAC carries no audio frames.
func TestGenerateFLACExtended_VorbisComment(t *testing.T) {
	const synced = "[00:01.00]hello\n[00:02.00]world"
	data := GenerateFLACExtended(44100, 441000, map[string]string{"SYNCEDLYRICS": synced})

	if len(data) < 4 || string(data[:4]) != "fLaC" {
		t.Fatalf("GenerateFLACExtended did not produce a FLAC stream (len=%d)", len(data))
	}

	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	got, _ := m.Raw()["syncedlyrics"].(string)
	if got != synced {
		t.Errorf("Raw()[syncedlyrics] = %q, want %q", got, synced)
	}
}

// TestGenerateFLACExtended_NoComments: with no comments the extended generator
// is byte-identical to GenerateFLAC (the early-return path).
func TestGenerateFLACExtended_NoComments(t *testing.T) {
	if !bytes.Equal(GenerateFLACExtended(44100, 441000, nil), GenerateFLAC(44100, 441000)) {
		t.Error("GenerateFLACExtended(nil comments) != GenerateFLAC")
	}
}

// TestWriteFLACFileWithComments writes a commented FLAC to disk and reads the
// Vorbis comment back.
func TestWriteFLACFileWithComments(t *testing.T) {
	dir := t.TempDir()
	const synced = "[00:01.00]x"
	if err := WriteFLACFileWithComments(dir, "t.flac", 48000, 96000,
		map[string]string{"SYNCEDLYRICS": synced}); err != nil {
		t.Fatalf("WriteFLACFileWithComments: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "t.flac"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	m, err := tag.ReadFrom(f)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if got, _ := m.Raw()["syncedlyrics"].(string); got != synced {
		t.Errorf("syncedlyrics = %q, want %q", got, synced)
	}
}
