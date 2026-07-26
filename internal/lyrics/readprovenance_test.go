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

// TestReadProvenanceTags_UTF8BOM locks the BOM misparse shut. A UTF-8 BOM on
// line 1 used to make the first header line fail parseTagLine, which ended the
// header block before it began and discarded EVERY tag in the file. That reads
// a genuinely tagged sidecar as untagged, which is a file-deletion decision for
// `scan purge-provenance --no-source`. The BOM-prefixed header must parse
// IDENTICALLY to the same header without one, and the lyric body must be
// untouched.
func TestReadProvenanceTags_UTF8BOM(t *testing.T) {
	dir := t.TempDir()
	header := "[source:musixmatch]\n[ar:The Artist]\n[ti:The Title]\n[isrc:GBRC12345678]\n[00:01.00]first line\n"

	plain := filepath.Join(dir, "plain.lrc")
	if err := os.WriteFile(plain, []byte(header), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	bom := filepath.Join(dir, "bom.lrc")
	if err := os.WriteFile(bom, []byte("\xef\xbb\xbf"+header), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	want, err := ReadProvenanceTags(plain)
	if err != nil {
		t.Fatalf("ReadProvenanceTags(plain): %v", err)
	}
	// Precondition: the control file really does carry the tags, so an
	// equality assertion below cannot pass by both sides being empty.
	if want.Source != "musixmatch" || want.Artist != "The Artist" || want.ISRC != "GBRC12345678" {
		t.Fatalf("control file did not parse its own tags: %+v", want)
	}

	got, err := ReadProvenanceTags(bom)
	if err != nil {
		t.Fatalf("ReadProvenanceTags(bom): %v", err)
	}
	if got != want {
		t.Errorf("BOM-prefixed header parsed differently:\n got  %+v\n want %+v", got, want)
	}

	// The BOM is stripped from the FIRST line only; the lyric body is verbatim.
	_, lines, err := parseLRCHeader(bom)
	if err != nil {
		t.Fatalf("parseLRCHeader(bom): %v", err)
	}
	if len(lines) != 1 || lines[0] != "[00:01.00]first line" {
		t.Errorf("lyric lines = %q, want exactly [\"[00:01.00]first line\"]", lines)
	}
}

// TestParseLRCHeader_BOMOnlyStrippedFromFirstLine proves the strip is anchored
// to line 1: a BOM appearing mid-file is content and must survive verbatim.
func TestParseLRCHeader_BOMOnlyStrippedFromFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mid.lrc")
	if err := os.WriteFile(path, []byte("[source:musixmatch]\n[00:01.00]\xef\xbb\xbfmarked\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, lines, err := parseLRCHeader(path)
	if err != nil {
		t.Fatalf("parseLRCHeader: %v", err)
	}
	if len(lines) != 1 || lines[0] != "[00:01.00]\uFEFFmarked" {
		t.Errorf("lyric lines = %q; a mid-file BOM must not be stripped", lines)
	}
}

func TestReadProvenanceTags_MissingFile(t *testing.T) {
	if _, err := ReadProvenanceTags(filepath.Join(t.TempDir(), "nope.lrc")); err == nil {
		t.Error("expected an error for a missing file, got nil")
	}
}
