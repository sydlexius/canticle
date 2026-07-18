package scanner

import (
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// ReadArtistIdentity is the seam the reconcile-identity backfill uses to re-read
// a file's corrected artist/album-artist identity. It must apply the same
// multi-value recovery as a scan (issue #466), so an on-disk multi-value TPE1 /
// TXXX ARTISTS surfaces delimited, not run together.
func TestReadArtistIdentity_MultiValue(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha\x00Bravo", "Title", "Album", "",
		map[string]string{"TPE2": "Alpha\x00Bravo"},
		map[string]string{"ARTISTS": "Alpha\x00Bravo", "ALBUMARTISTS": "Alpha\x00Bravo"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	artist, albumArtist, err := ReadArtistIdentity(filepath.Join(dir, "track.mp3"))
	if err != nil {
		t.Fatalf("ReadArtistIdentity: %v", err)
	}
	if got, want := artist, "Alpha; Bravo"; got != want {
		t.Errorf("artist = %q; want %q", got, want)
	}
	if got, want := albumArtist, "Alpha; Bravo"; got != want {
		t.Errorf("albumArtist = %q; want %q", got, want)
	}
}

// A missing file surfaces an error rather than empty identity, so the backfill
// can distinguish "read failed" (skip) from "artist is genuinely empty".
func TestReadArtistIdentity_MissingFile(t *testing.T) {
	_, _, err := ReadArtistIdentity(filepath.Join(t.TempDir(), "nope.mp3"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
