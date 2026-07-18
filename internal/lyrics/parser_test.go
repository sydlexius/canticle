package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// writeTestLRC is a helper that writes a minimal synced LRC file containing
// the provided extra header tags plus a fixed lyric line, for parser tests.
func writeTestLRC(t *testing.T, dir string, extraTags []string, lyricLines []string) string {
	t.Helper()
	path := filepath.Join(dir, "track.lrc")
	var sb strings.Builder
	sb.WriteString("[by:mxlrcgo-svc]\n")
	sb.WriteString("[ar:Test Artist]\n")
	sb.WriteString("[ti:Test Track]\n")
	for _, tag := range extraTags {
		sb.WriteString(tag + "\n")
	}
	for _, line := range lyricLines {
		sb.WriteString(line + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("writeTestLRC: %v", err)
	}
	return path
}

func TestParseTagLine(t *testing.T) {
	tests := []struct {
		line    string
		wantKey string
		wantVal string
		wantOK  bool
	}{
		{"[ar:Test Artist]", "ar", "Test Artist", true},
		{"[source:musixmatch]", "source", "musixmatch", true},
		{"[fetched:2024-01-02T03:04:05Z]", "fetched", "2024-01-02T03:04:05Z", true},
		{"[isrc:USRC12345678]", "isrc", "USRC12345678", true},
		{"[mbid:12345678-1234-1234-1234-123456789abc]", "mbid", "12345678-1234-1234-1234-123456789abc", true},
		{"[01:23.45]lyric line", "", "", false}, // timestamp line, not a tag
		{"not a tag", "", "", false},
		{"[nocolon]", "", "", false},
		{"", "", "", false},
		{"  [by:mxlrcgo-svc]  ", "by", "mxlrcgo-svc", true}, // leading/trailing whitespace
	}
	for _, tc := range tests {
		tag, ok := parseTagLine(tc.line)
		if ok != tc.wantOK {
			t.Errorf("parseTagLine(%q): got ok=%v, want %v", tc.line, ok, tc.wantOK)
			continue
		}
		if ok {
			if tag.key != tc.wantKey {
				t.Errorf("parseTagLine(%q): key=%q, want %q", tc.line, tag.key, tc.wantKey)
			}
			if tag.value != tc.wantVal {
				t.Errorf("parseTagLine(%q): value=%q, want %q", tc.line, tag.value, tc.wantVal)
			}
		}
	}
}

func TestInjectProvenance_AddsNewTags(t *testing.T) {
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello world"})

	pt := ProvenanceTags{
		Source:  "musixmatch",
		Fetched: "2024-06-15T12:00:00Z",
		ISRC:    "USRC12345678",
		MBID:    "12345678-1234-1234-1234-123456789abc",
	}
	inj, skipped, err := InjectProvenance(path, pt)
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}
	if inj != 4 {
		t.Errorf("injected=%d, want 4", inj)
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0", skipped)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"[source:musixmatch]",
		"[fetched:2024-06-15T12:00:00Z]",
		"[isrc:USRC12345678]",
		"[mbid:12345678-1234-1234-1234-123456789abc]",
		"[00:01.00]Hello world",
		"[by:mxlrcgo-svc]",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("result missing %q:\n%s", want, content)
		}
	}
}

func TestInjectProvenance_SkipsExisting(t *testing.T) {
	dir := t.TempDir()
	path := writeTestLRC(t, dir,
		[]string{"[source:petitlyrics]", "[isrc:GBRC12345678]"},
		[]string{"[00:01.00]Hello"},
	)

	pt := ProvenanceTags{
		Source:  "musixmatch",
		Fetched: "2024-06-15T12:00:00Z",
		ISRC:    "USRC12345678",
		MBID:    "12345678-1234-1234-1234-123456789abc",
	}
	inj, skipped, err := InjectProvenance(path, pt)
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}
	// source and isrc already exist -> skipped=2; fetched and mbid are new -> inj=2
	if skipped != 2 {
		t.Errorf("skipped=%d, want 2", skipped)
	}
	if inj != 2 {
		t.Errorf("injected=%d, want 2", inj)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	// Original source tag must be preserved (not overwritten).
	if !strings.Contains(content, "[source:petitlyrics]") {
		t.Error("original [source:] tag was overwritten")
	}
	if strings.Contains(content, "[source:musixmatch]") {
		t.Error("new [source:] tag was injected even though one already existed")
	}
	// Original isrc preserved.
	if !strings.Contains(content, "[isrc:GBRC12345678]") {
		t.Error("original [isrc:] tag was overwritten")
	}
}

func TestInjectProvenance_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})

	pt := ProvenanceTags{Source: "musixmatch", Fetched: "2024-06-15T12:00:00Z"}

	inj1, _, err := InjectProvenance(path, pt)
	if err != nil {
		t.Fatalf("first inject: %v", err)
	}
	inj2, skipped2, err := InjectProvenance(path, pt)
	if err != nil {
		t.Fatalf("second inject: %v", err)
	}
	if inj2 != 0 {
		t.Errorf("second inject: inj=%d, want 0 (idempotent)", inj2)
	}
	if skipped2 != inj1 {
		t.Errorf("second inject: skipped=%d, want %d", skipped2, inj1)
	}

	// Verify no duplicate tags in the file.
	data, _ := os.ReadFile(path)
	lines := strings.Split(string(data), "\n")
	seen := map[string]int{}
	for _, l := range lines {
		if tag, ok := parseTagLine(l); ok && tag.key != "" {
			seen[tag.key]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("duplicate tag [%s:] appears %d times", k, n)
		}
	}
}

func TestInjectProvenance_PreservesLyricContent(t *testing.T) {
	dir := t.TempDir()
	lyricLines := []string{
		"[00:01.00]First line",
		"[00:02.50]Second line",
		"[00:04.00]Third line",
	}
	path := writeTestLRC(t, dir, nil, lyricLines)

	_, _, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"})
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	for _, l := range lyricLines {
		if !strings.Contains(content, l) {
			t.Errorf("lyric line %q missing from result", l)
		}
	}
}

func TestInjectProvenance_LRCOnly(t *testing.T) {
	dir := t.TempDir()
	txtPath := filepath.Join(dir, "track.txt")
	if err := os.WriteFile(txtPath, []byte("some content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := InjectProvenance(txtPath, ProvenanceTags{Source: "musixmatch"})
	if err == nil {
		t.Error("expected error for non-.lrc file, got nil")
	}
}

func TestInjectProvenance_NoOpWhenNoTags(t *testing.T) {
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})

	inj, skipped, err := InjectProvenance(path, ProvenanceTags{}) // all empty
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}
	if inj != 0 {
		t.Errorf("inj=%d, want 0 for empty ProvenanceTags", inj)
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0 for empty ProvenanceTags", skipped)
	}
}

// TestWriterProvenanceTags verifies that WriteLRC emits [source:], [fetched:],
// [ve:], [isrc:], and [mbid:] when the Song fields are set.
func TestWriterProvenanceTags(t *testing.T) {
	w := NewLRCWriter()
	dir := t.TempDir()

	fetchTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := models.Song{
		Track: models.Track{
			ArtistName:    "Test Artist",
			TrackName:     "Test Track",
			ISRC:          "USRC12345678",
			RecordingMBID: "abcd1234-abcd-abcd-abcd-abcd12345678",
		},
		Subtitles: models.Synced{
			Lines: []models.Lines{
				{Text: "Hello", Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 0}},
			},
		},
		WinningLane: "musixmatch",
		FetchedAt:   fetchTime,
	}

	if err := w.WriteLRC(song, "track.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "track.lrc"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	content := string(data)

	wantTags := []string{
		"[source:musixmatch]",
		"[fetched:2024-06-15T12:00:00Z]",
		"[isrc:USRC12345678]",
		"[mbid:abcd1234-abcd-abcd-abcd-abcd12345678]",
		"[ve:",
	}
	for _, tag := range wantTags {
		if !strings.Contains(content, tag) {
			t.Errorf("missing tag %q in output:\n%s", tag, content)
		}
	}
}

// TestWriterProvenanceTags_AbsentWhenEmpty verifies that provenance tags are
// NOT emitted when the corresponding Song fields are zero/empty.
func TestWriterProvenanceTags_AbsentWhenEmpty(t *testing.T) {
	w := NewLRCWriter()
	dir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName: "Test Artist",
			TrackName:  "Test Track",
		},
		Subtitles: models.Synced{
			Lines: []models.Lines{
				{Text: "Hello", Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 0}},
			},
		},
		// WinningLane, FetchedAt, ISRC, RecordingMBID all zero/empty
	}

	if err := w.WriteLRC(song, "track.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "track.lrc"))
	content := string(data)

	for _, tag := range []string{"[source:", "[fetched:", "[isrc:", "[mbid:"} {
		if strings.Contains(content, tag) {
			t.Errorf("unexpected tag %q in output when field is empty:\n%s", tag, content)
		}
	}
}

func TestParseLRCHeader_NotFound(t *testing.T) {
	_, _, err := parseLRCHeader(filepath.Join(t.TempDir(), "nonexistent.lrc"))
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestInjectProvenance_ParseError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, file permission restrictions do not apply")
	}
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, _, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"})
	if err == nil {
		t.Error("expected error for unreadable file, got nil")
	}
}

func TestInjectProvenance_AtomicWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, directory permission restrictions do not apply")
	}
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})
	original, _ := os.ReadFile(path)

	// Make the directory read-only: os.CreateTemp cannot create new files,
	// but the existing .lrc file remains readable via the execute bit.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"})
	if err == nil {
		t.Error("expected error when directory is read-only, got nil")
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("original file unreadable after failed inject: %v", readErr)
	}
	if string(after) != string(original) {
		t.Error("original file was modified by failed inject")
	}
}

// TestWriterRoundTrip writes a tagged .lrc and parses it back to verify the
// header parser correctly identifies all tag keys and leaves lyric lines intact.
func TestWriterRoundTrip(t *testing.T) {
	w := NewLRCWriter()
	dir := t.TempDir()

	fetchTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := models.Song{
		Track: models.Track{
			ArtistName:    "Round Trip",
			TrackName:     "Test Song",
			AlbumName:     "Test Album",
			TrackLength:   180,
			ISRC:          "USRC12345678",
			RecordingMBID: "abcd1234-0000-0000-0000-000000000000",
		},
		Subtitles: models.Synced{
			Lines: []models.Lines{
				{Text: "Line one", Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 0}},
				{Text: "Line two", Time: models.Time{Minutes: 0, Seconds: 5, Hundredths: 50}},
			},
		},
		WinningLane: "musixmatch",
		FetchedAt:   fetchTime,
	}

	if err := w.WriteLRC(song, "roundtrip.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	path := filepath.Join(dir, "roundtrip.lrc")
	tags, lyricLines, err := parseLRCHeader(path)
	if err != nil {
		t.Fatalf("parseLRCHeader: %v", err)
	}

	// Build a key->value map from the parsed tags.
	tagMap := map[string]string{}
	for _, tag := range tags {
		if tag.key != "" {
			tagMap[tag.key] = tag.value
		}
	}

	wantKeys := []string{"by", "ar", "ti", "al", "length", "source", "fetched", "ve", "isrc", "mbid"}
	for _, k := range wantKeys {
		if _, ok := tagMap[k]; !ok {
			t.Errorf("parsed header missing tag key %q; tags: %v", k, tagMap)
		}
	}

	// Lyric lines must be preserved.
	lyricText := strings.Join(lyricLines, "\n")
	for _, want := range []string{"[00:01.00]Line one", "[00:05.50]Line two"} {
		if !strings.Contains(lyricText, want) {
			t.Errorf("lyric lines missing %q from:\n%s", want, lyricText)
		}
	}
}

// TestInjectProvenance_PreservesFileMode asserts MAJOR-1: a 0600 .lrc stays
// 0600 after InjectProvenance rewrites it.
func TestInjectProvenance_PreservesFileMode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, file permissions do not apply")
	}
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})
	// writeTestLRC creates with 0600; verify the pre-condition.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before inject: %v", err)
	}
	if before.Mode().Perm() != 0o600 {
		t.Fatalf("pre-condition: expected 0600, got %04o", before.Mode().Perm())
	}

	if _, _, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"}); err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after inject: %v", err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("file permissions widened: before=%04o, after=%04o", before.Mode().Perm(), after.Mode().Perm())
	}
}

// TestInjectProvenance_SkipsSymlink asserts MINOR-5: a symlinked .lrc is skipped
// (the link is not replaced by a regular file) and the call returns no error.
func TestInjectProvenance_SkipsSymlink(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, symlink semantics differ")
	}
	dir := t.TempDir()
	real := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})
	linkPath := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(real, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	inj, skip, err := InjectProvenance(linkPath, ProvenanceTags{Source: "musixmatch"})
	if err != nil {
		t.Fatalf("InjectProvenance on symlink returned error: %v", err)
	}
	if inj != 0 || skip != 0 {
		t.Errorf("expected (0,0) for symlink skip, got (%d,%d)", inj, skip)
	}

	// The link must still be a symlink (not replaced by a regular file).
	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat after inject: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
}

// TestInjectProvenance_TagValueSanitized asserts MINOR-10: ']' and newlines in a
// new tag value are stripped so they cannot break out of [key:value] syntax.
func TestInjectProvenance_TagValueSanitized(t *testing.T) {
	dir := t.TempDir()
	path := writeTestLRC(t, dir, nil, []string{"[00:01.00]Hello"})

	// Value with ']' and embedded newline.
	badSource := "evil]value\ninjected"
	inj, _, err := InjectProvenance(path, ProvenanceTags{Source: badSource})
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}
	if inj != 1 {
		t.Fatalf("expected 1 tag injected, got %d", inj)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Contains(content, "]value") {
		t.Errorf("sanitization failed: raw ']' in output:\n%s", content)
	}
	if strings.Contains(content, "\ninjected") {
		t.Errorf("sanitization failed: embedded newline in output:\n%s", content)
	}
}

// TestInjectProvenance_CaseInsensitiveDedup asserts NIT-13: a hand-edited file
// with a capitalized [Source:] tag is recognized as "source already present" and
// not duplicated.
func TestInjectProvenance_CaseInsensitiveDedup(t *testing.T) {
	dir := t.TempDir()
	// Write a file with a capitalized tag key.
	path := filepath.Join(dir, "track.lrc")
	content := "[by:mxlrcgo-svc]\n[Source:petitlyrics]\n[00:01.00]Hello\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	inj, skip, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"})
	if err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}
	if inj != 0 {
		t.Errorf("expected 0 injected (key already present case-insensitively), got %d", inj)
	}
	if skip != 1 {
		t.Errorf("expected 1 skipped, got %d", skip)
	}

	// Verify no duplicate [source:] was added.
	result, _ := os.ReadFile(path)
	if count := strings.Count(string(result), "Source"); count > 1 {
		t.Errorf("duplicate source tag after inject:\n%s", string(result))
	}
}

// TestInjectProvenance_TagsBeforeBlankLine asserts MINOR-7: new tags are
// inserted within the header tag block, before any blank header separator line.
func TestInjectProvenance_TagsBeforeBlankLine(t *testing.T) {
	dir := t.TempDir()
	// File with a blank separator line between header and lyric body.
	path := filepath.Join(dir, "track.lrc")
	content := "[by:mxlrcgo-svc]\n[ar:Test]\n\n[00:01.00]Hello\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := InjectProvenance(path, ProvenanceTags{Source: "musixmatch"}); err != nil {
		t.Fatalf("InjectProvenance: %v", err)
	}

	result, _ := os.ReadFile(path)
	lines := strings.Split(string(result), "\n")
	// Find positions of [source:] and the blank line.
	sourceIdx, blankIdx := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "[source:") {
			sourceIdx = i
		}
		if l == "" && blankIdx == -1 {
			blankIdx = i
		}
	}
	if sourceIdx < 0 {
		t.Fatal("no [source:] tag found")
	}
	if blankIdx >= 0 && sourceIdx > blankIdx {
		t.Errorf("[source:] at line %d is AFTER the blank line at %d; should be before", sourceIdx, blankIdx)
	}
}
