package scanner

import (
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

func TestIsAudioFile(t *testing.T) {
	cases := map[string]bool{
		"song.mp3":         true,
		"song.flac":        true,
		"SONG.FLAC":        true, // case-insensitive
		"a/b/c/track.m4a":  true,
		"track.ogg":        true,
		"lyrics.lrc":       false,
		"lyrics.txt":       false,
		"cover.jpg":        false,
		"noext":            false,
		"trailing.mp3.bak": false,
	}
	for name, want := range cases {
		if got := IsAudioFile(name); got != want {
			t.Errorf("IsAudioFile(%q) = %v; want %v", name, got, want)
		}
	}
}

// TestSupportedAudioExtensionsMatchesIsAudioFile guards against the two
// exported accessors over supportedFileTypes (IsAudioFile and
// SupportedAudioExtensions) drifting apart: every extension the enumerator
// returns must independently pass IsAudioFile, and vice versa via a synthetic
// name built from each extension. #691's stem probe (internal/revalidate)
// depends on this equivalence to classify audio identically to scanDir.
func TestSupportedAudioExtensionsMatchesIsAudioFile(t *testing.T) {
	exts := SupportedAudioExtensions()
	if len(exts) == 0 {
		t.Fatal("SupportedAudioExtensions returned no extensions")
	}
	for _, ext := range exts {
		if !IsAudioFile("track" + ext) {
			t.Errorf("SupportedAudioExtensions returned %q, but IsAudioFile(%q) = false", ext, "track"+ext)
		}
	}
	// Mutating the returned slice must not affect a later call: it is a
	// defensive copy, not a view onto the package's own backing slice.
	exts[0] = ".corrupted"
	if again := SupportedAudioExtensions(); again[0] == ".corrupted" {
		t.Error("SupportedAudioExtensions is not returning a defensive copy")
	}
}

func TestReadAudioProvenance(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3", "Some Artist", "Some Title", "Album", "",
		map[string]string{"TSRC": testISRC},
		map[string]string{"MusicBrainz Track Id": testMBID}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	isrc, mbid, artist, title, err := ReadAudioProvenance(filepath.Join(dir, "track.mp3"))
	if err != nil {
		t.Fatalf("ReadAudioProvenance: %v", err)
	}
	if isrc != testISRC {
		t.Errorf("isrc = %q; want %q", isrc, testISRC)
	}
	if mbid != testMBID {
		t.Errorf("mbid = %q; want %q", mbid, testMBID)
	}
	if artist != "Some Artist" {
		t.Errorf("artist = %q; want %q", artist, "Some Artist")
	}
	if title != "Some Title" {
		t.Errorf("title = %q; want %q", title, "Some Title")
	}
}

func TestReadAudioProvenance_MissingFile(t *testing.T) {
	if _, _, _, _, err := ReadAudioProvenance(filepath.Join(t.TempDir(), "nope.mp3")); err == nil {
		t.Error("expected an error for a missing file, got nil")
	}
}
