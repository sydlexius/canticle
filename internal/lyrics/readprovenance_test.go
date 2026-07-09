package lyrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProvenanceTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.lrc")
	content := "[ar:The Artist]\n[ti:The Title]\n[isrc:GBRC12345678]\n[mbid:550e8400-e29b-41d4-a716-446655440000]\n[00:01.00]first line\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pt, err := ReadProvenanceTags(path)
	if err != nil {
		t.Fatalf("ReadProvenanceTags: %v", err)
	}
	if pt.Artist != "The Artist" {
		t.Errorf("Artist = %q", pt.Artist)
	}
	if pt.Title != "The Title" {
		t.Errorf("Title = %q", pt.Title)
	}
	if pt.ISRC != "GBRC12345678" {
		t.Errorf("ISRC = %q", pt.ISRC)
	}
	if pt.MBID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("MBID = %q", pt.MBID)
	}
}

func TestReadProvenanceTags_NoHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(path, []byte("just some unsynced lyrics\nwith no header\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pt, err := ReadProvenanceTags(path)
	if err != nil {
		t.Fatalf("ReadProvenanceTags: %v", err)
	}
	if pt != (ProvenanceTags{}) {
		t.Errorf("expected zero-value tags for a headerless file, got %+v", pt)
	}
}

func TestReadProvenanceTags_MissingFile(t *testing.T) {
	if _, err := ReadProvenanceTags(filepath.Join(t.TempDir(), "nope.lrc")); err == nil {
		t.Error("expected an error for a missing file, got nil")
	}
}
