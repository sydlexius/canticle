package scanner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// A multi-value ID3v2.4 TPE1 frame is NUL-separated on disk, but dhowden/tag
// joins the parts with an empty string, so m.Artist() returns the discrete
// values run together ("AlphaBravoCharlie"). The parallel TXXX "ARTISTS" frame
// that standard taggers write alongside TPE1 preserves the boundaries; the
// scanner must recover the delimited value from it (issue #466).
func TestScanArtist_MultiValueRecoveredFromARTISTS(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha\x00Bravo\x00Charlie", "Title", "Album", "",
		nil, map[string]string{"ARTISTS": "Alpha\x00Bravo\x00Charlie"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got, want := results[0].Track.ArtistName, "Alpha; Bravo; Charlie"; got != want {
		t.Errorf("ArtistName = %q; want %q", got, want)
	}
}

// The album-artist path mirrors the artist path: a multi-value TPE2 frame is
// mangled by dhowden/tag, but the parallel TXXX "ALBUMARTISTS" frame preserves
// the boundaries.
func TestScanAlbumArtist_MultiValueRecoveredFromALBUMARTISTS(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha", "Title", "Album", "",
		map[string]string{"TPE2": "Alpha\x00Bravo"},
		map[string]string{"ALBUMARTISTS": "Alpha\x00Bravo"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got, want := results[0].Track.AlbumArtist, "Alpha; Bravo"; got != want {
		t.Errorf("AlbumArtist = %q; want %q", got, want)
	}
}

// A single-value artist (the common case, no ARTISTS frame) is unaffected: the
// standard accessor is returned verbatim.
func TestScanArtist_SingleValueUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Solo Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got, want := results[0].Track.ArtistName, "Solo Artist"; got != want {
		t.Errorf("ArtistName = %q; want %q", got, want)
	}
}

// ReadAudioProvenance feeds realign's exact-match tier the same identity a scan
// records, so it must recover the multi-value artist too.
func TestReadAudioProvenance_MultiValueArtist(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha\x00Bravo", "Title", "Album", "",
		nil, map[string]string{"ARTISTS": "Alpha\x00Bravo"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, _, artist, _, err := ReadAudioProvenance(filepath.Join(dir, "track.mp3"))
	if err != nil {
		t.Fatalf("ReadAudioProvenance: %v", err)
	}
	if got, want := artist, "Alpha; Bravo"; got != want {
		t.Errorf("artist = %q; want %q", got, want)
	}
}
